package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync/atomic"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/process"
	"mcp-local-hub/internal/secrets"
)

var (
	ErrStdioLaunchContext  = errors.New("STDIO_LAUNCH_CONTEXT_UNAVAILABLE")
	ErrStdioLaunchChanged  = errors.New("STDIO_LAUNCH_CONTEXT_CHANGED")
	ErrStdioLaunchConsumed = errors.New("STDIO_LAUNCH_ALREADY_CONSUMED")
	ErrStdioLaunchPartial  = errors.New("STDIO_LAUNCH_PREPARATION_UNAVAILABLE")
)

// StdioLaunchContext is one operation's actual parent context, not a supplied
// environment or a credential. No process-wide environment is changed.
type StdioLaunchContext struct {
	owner       *StdioLaunchContext
	ctx         context.Context
	environment []string
	cwd         string
	failure     error
}

func (s StdioLaunchContext) Format(w fmt.State, _ rune) { io.WriteString(w, "<stdio launch context>") }

func CaptureStdioLaunchContext(ctx context.Context) (*StdioLaunchContext, error) {
	s := &StdioLaunchContext{ctx: ctx}
	s.owner = s
	if ctx == nil {
		s.failure = ErrStdioLaunchContext
		return s, s.failure
	}
	if err := ctx.Err(); err != nil {
		s.failure = errors.Join(ErrStdioLaunchContext, err)
		return s, &StdioLaunchPreparationError{cause: s.failure, context: s}
	}
	s.environment = append([]string{}, os.Environ()...)
	var err error
	s.cwd, err = os.Getwd()
	if err != nil {
		s.failure = errors.Join(ErrStdioLaunchContext, err)
		return s, &StdioLaunchPreparationError{cause: s.failure, context: s}
	}
	return s, nil
}

func (s *StdioLaunchContext) valid() error {
	if s == nil || s.owner != s || s.ctx == nil || s.cwd == "" || s.environment == nil {
		return ErrStdioLaunchContext
	}
	return s.failure
}

func (s *StdioLaunchContext) LookupEnvironment(name string) (string, bool) {
	if s.valid() != nil {
		return "", false
	}
	for i := len(s.environment) - 1; i >= 0; i-- {
		key, value, ok := strings.Cut(s.environment[i], "=")
		if ok && (key == name || runtime.GOOS == "windows" && strings.EqualFold(key, name)) {
			return value, true
		}
	}
	return "", false
}

func (s *StdioLaunchContext) WorkingDirectory() string {
	if s.valid() != nil {
		return ""
	}
	return s.cwd
}

func (s *StdioLaunchContext) ParseManifest(r io.Reader) (*config.ServerManifest, error) {
	if err := s.valid(); err != nil {
		return nil, err
	}
	return config.ParseManifestWithEnvironment(r, s.LookupEnvironment)
}

func (s *StdioLaunchContext) BindResolver(r *secrets.Resolver) (*secrets.Resolver, error) {
	if err := s.valid(); err != nil {
		return nil, err
	}
	return r.WithEnvironmentLookup(s.LookupEnvironment)
}

// NewStdioHost finishes an ordinary immediate launch from the same context
// used for parsing and materialization. It preserves ordinary Start semantics.
func (s *StdioLaunchContext) NewStdioHost(cfg HostConfig) (*StdioHost, error) {
	if err := s.valid(); err != nil {
		return nil, err
	}
	h, err := NewStdioHost(cloneStdioLaunchConfig(cfg))
	if h != nil {
		h.launchContext = s
	}
	return h, err
}

// StdioLaunchInputs carries calculation data only. It has no runner, source
// receipt, permission flag or caller-created image/profile proof.
type StdioLaunchInputs struct {
	Config    HostConfig          `json:"-"`
	Context   *StdioLaunchContext `json:"-"`
	BaseArgs  []string            `json:"-"`
	ExtraArgs []string            `json:"-"`
}

func (i StdioLaunchInputs) Format(w fmt.State, _ rune) { io.WriteString(w, "<stdio launch inputs>") }

// StdioArguments is also used by the ordinary CLI launch path.
func StdioArguments(base, extra []string) []string {
	args := append([]string{}, base...)
	return append(args, extra...)
}

type stdioLaunchGeneration struct{ issued bool }

// StdioLaunchPreparation retains the exact command. Its comparison methods
// expose no environment map, command object, pipe or launch capability.
type StdioLaunchPreparation struct {
	owner      *StdioHost
	generation *stdioLaunchGeneration
	context    *StdioLaunchContext
	command    *exec.Cmd
	image      stdioLaunchImage
	failure    error
	consumed   atomic.Bool
}

func (p *StdioLaunchPreparation) Format(w fmt.State, _ rune) {
	io.WriteString(w, "<stdio launch preparation>")
}

// StdioLaunchExecutionProfile is private calculation DATA, not a launch permit.
// Environment values remain with the preparation that actually calculated them.
type StdioLaunchExecutionProfile struct {
	CommandPath                string   `json:"-"`
	Arguments                  []string `json:"-"`
	WorkingDirectory           string   `json:"-"`
	EffectiveEnvironmentSHA256 string   `json:"-"`
}

func (StdioLaunchExecutionProfile) String() string               { return "<stdio launch execution profile>" }
func (p StdioLaunchExecutionProfile) GoString() string           { return p.String() }
func (p StdioLaunchExecutionProfile) Format(w fmt.State, _ rune) { io.WriteString(w, p.String()) }

// The partial preparation and original cause remain on the error even if a
// caller discards the partial host return. Public formatting stays masked.
type StdioLaunchPreparationError struct {
	cause        error
	partial      *StdioLaunchPreparation
	context      *StdioLaunchContext
	recheckImage *stdioLaunchImage
}

func (e *StdioLaunchPreparationError) Error() string                               { return ErrStdioLaunchPartial.Error() }
func (e *StdioLaunchPreparationError) Unwrap() error                               { return e.cause }
func (e *StdioLaunchPreparationError) Format(w fmt.State, _ rune)                  { io.WriteString(w, e.Error()) }
func (e *StdioLaunchPreparationError) PartialPreparation() *StdioLaunchPreparation { return e.partial }

func NewPreparedStdioHost(ctx context.Context, inputs StdioLaunchInputs) (*StdioHost, error) {
	cfg := cloneStdioLaunchConfig(inputs.Config)
	if inputs.BaseArgs != nil || inputs.ExtraArgs != nil {
		cfg.Args = StdioArguments(inputs.BaseArgs, inputs.ExtraArgs)
	}
	h, hostErr := NewStdioHost(cfg)
	p := &StdioLaunchPreparation{owner: h, generation: &stdioLaunchGeneration{issued: true}}
	fail := func(err error) (*StdioHost, error) {
		p.failure = err
		return h, &StdioLaunchPreparationError{cause: err, partial: p}
	}
	if h != nil {
		h.prepared = p
		h.preparedGeneration = p.generation
	}
	if hostErr != nil {
		return fail(hostErr)
	}
	if cfg.LaunchCapability != nil {
		return fail(errors.New("prepared stdio does not accept a launch capability"))
	}
	s := inputs.Context
	if s == nil {
		var err error
		s, err = CaptureStdioLaunchContext(ctx)
		p.context = s
		if err != nil {
			return fail(err)
		}
	} else {
		p.context = s
	}
	if err := s.valid(); err != nil {
		return fail(err)
	}
	// The original context is retained; a delayed operation cannot rebind it.
	if ctx == nil || !reflect.TypeOf(ctx).Comparable() || ctx != s.ctx {
		return fail(ErrStdioLaunchContext)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	p.command = resolvePreparedStdioCommand(s, cfg)
	if p.command.Err != nil {
		return fail(p.command.Err)
	}
	var err error
	p.image, err = readStdioLaunchImage(ctx, p.command.Path)
	if err != nil {
		return fail(err)
	}
	return h, nil
}

func cloneStdioLaunchConfig(cfg HostConfig) HostConfig {
	cfg.Args = append([]string(nil), cfg.Args...)
	cfg.UnsetEnv = append([]string(nil), cfg.UnsetEnv...)
	if cfg.Env != nil {
		env := make(map[string]string, len(cfg.Env))
		for key, value := range cfg.Env {
			env[key] = value
		}
		cfg.Env = env
	}
	return cfg
}

func prepareStdioCommand(ctx context.Context, cfg HostConfig, parent []string) *exec.Cmd {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" && cfg.LaunchCapability == nil {
		cmd = exec.Command(cfg.Command, cfg.Args...)
	} else {
		cmd = exec.CommandContext(ctx, cfg.Command, cfg.Args...)
	}
	process.NoConsole(cmd)
	process.SetParentDeathSignal(cmd)
	cmd.Dir = cfg.WorkingDir
	unset := append(append([]string{}, cfg.UnsetEnv...), LaunchCapabilityHandleEnv)
	cmd.Env = composeChildEnvFrom(parent, cfg.Env, unset)
	return cmd
}

// ErrDot is a Go lookup policy. Read the immutable executable's declared
// defaults and only this capture's GODEBUG override, never the live setting.
func capturedStdioErrDotDisabled(s *StdioLaunchContext) bool {
	disabled := false
	apply := func(settings string) {
		for _, setting := range strings.Split(settings, ",") {
			key, value, ok := strings.Cut(setting, "=")
			if ok && key == "execerrdot" {
				disabled = value == "0"
			}
		}
	}
	if info, available := debug.ReadBuildInfo(); available {
		for _, setting := range info.Settings {
			if setting.Key == "DefaultGODEBUG" {
				apply(setting.Value)
			}
		}
	}
	value, _ := s.LookupEnvironment("GODEBUG")
	apply(value)
	return disabled
}

func (h *StdioHost) PreparedLaunch() (*StdioLaunchPreparation, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.prepared == nil {
		return nil, ErrStdioLaunchPartial
	}
	if h.prepared.failure != nil {
		return h.prepared, &StdioLaunchPreparationError{cause: h.prepared.failure, partial: h.prepared}
	}
	return h.prepared, nil
}

func (p *StdioLaunchPreparation) CommandPath() string {
	if p == nil || p.command == nil || p.failure != nil {
		return ""
	}
	return p.command.Path
}
func (p *StdioLaunchPreparation) Arguments() []string {
	if p == nil || p.command == nil || p.failure != nil {
		return nil
	}
	return append([]string(nil), p.command.Args[1:]...)
}
func (p *StdioLaunchPreparation) WorkingDirectory() string {
	if p == nil || p.command == nil || p.failure != nil {
		return ""
	}
	if p.command.Dir != "" {
		return p.command.Dir
	}
	return p.context.cwd
}
func (p *StdioLaunchPreparation) EnvironmentValueMatches(name, value string) (present, matches bool) {
	if p == nil || p.command == nil || p.failure != nil {
		return false, false
	}
	for i := len(p.command.Env) - 1; i >= 0; i-- {
		key, actual, ok := strings.Cut(p.command.Env[i], "=")
		if ok && envKeyNorm(key) == envKeyNorm(name) {
			return true, actual == value
		}
	}
	return false, false
}

func (p *StdioLaunchPreparation) Validate() error {
	if p == nil {
		return &StdioLaunchPreparationError{cause: ErrStdioLaunchPartial}
	}
	return p.validateFor(p.owner)
}

// ObserveExecutionProfile receives the same captured operation context and
// preserves the preparation's before/after validation and partial-error channel.
// The view describes this calculation; it acquires no execution or lifetime lease.
func (p *StdioLaunchPreparation) ObserveExecutionProfile(ctx context.Context) (StdioLaunchExecutionProfile, error) {
	if err := p.Validate(); err != nil {
		return StdioLaunchExecutionProfile{}, err
	}
	fail := func(err error) (StdioLaunchExecutionProfile, error) {
		return StdioLaunchExecutionProfile{}, &StdioLaunchPreparationError{cause: err, partial: p}
	}
	if ctx == nil || !reflect.TypeOf(ctx).Comparable() || ctx != p.context.ctx {
		return fail(ErrStdioLaunchContext)
	}
	values, err := effectiveStdioEnvironment(p.command.Env)
	if err != nil {
		return fail(err)
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	digest := sha256.New()
	io.WriteString(digest, "stdio-effective-environment-v1")
	digest.Write([]byte{0})
	var length [8]byte
	writeLength := func(n int) {
		binary.BigEndian.PutUint64(length[:], uint64(n))
		digest.Write(length[:])
	}
	writeLength(len(keys))
	for _, key := range keys {
		writeLength(len(key))
		io.WriteString(digest, key)
		writeLength(len(values[key]))
		io.WriteString(digest, values[key])
	}
	profile := StdioLaunchExecutionProfile{
		CommandPath:                p.command.Path,
		Arguments:                  append([]string{}, p.command.Args[1:]...),
		WorkingDirectory:           p.WorkingDirectory(),
		EffectiveEnvironmentSHA256: fmt.Sprintf("%x", digest.Sum(nil)),
	}
	if err := p.Validate(); err != nil {
		return StdioLaunchExecutionProfile{}, err
	}
	return profile, nil
}

// ProjectServiceExecutionProfile maps the actual lower observation to the
// API-owned read port. It performs no second execution/environment calculation.
func (p *StdioLaunchPreparation) ProjectServiceExecutionProfile(ctx context.Context) (api.ProjectServiceExecutionProfile, error) {
	profile, err := p.ObserveExecutionProfile(ctx)
	if err != nil {
		return api.ProjectServiceExecutionProfile{}, err
	}
	return api.ProjectServiceExecutionProfile{
		CommandPath:                profile.CommandPath,
		Arguments:                  profile.Arguments,
		WorkingDirectory:           profile.WorkingDirectory,
		EffectiveEnvironmentSHA256: profile.EffectiveEnvironmentSHA256,
	}, nil
}

func (p *StdioLaunchPreparation) SameProjectServiceExecution(ctx context.Context, other api.ProjectServicePreparedLaunch) (bool, error) {
	if _, err := p.ObserveExecutionProfile(ctx); err != nil {
		return false, err
	}
	peer, ok := other.(*StdioLaunchPreparation)
	if !ok {
		return false, &StdioLaunchPreparationError{cause: ErrStdioLaunchContext, partial: p}
	}
	if _, err := peer.ObserveExecutionProfile(ctx); err != nil {
		return false, err
	}
	return p.SameExecution(peer)
}

// The consuming host, rather than an owner retained on a copied host, is the
// authority for this instance check. Start calls this under its actual mutex.
func (p *StdioLaunchPreparation) validateFor(owner *StdioHost) error {
	var recheck *stdioLaunchImage
	fail := func(err error) error {
		return &StdioLaunchPreparationError{cause: err, partial: p, recheckImage: recheck}
	}
	if p == nil || owner == nil || p.owner != owner || owner.prepared != p || p.generation == nil ||
		owner.preparedGeneration != p.generation || p.command == nil || p.failure != nil {
		if p != nil && p.failure != nil {
			return fail(p.failure)
		}
		return fail(ErrStdioLaunchPartial)
	}
	if p.consumed.Load() {
		return fail(ErrStdioLaunchConsumed)
	}
	if err := p.context.valid(); err != nil {
		return fail(err)
	}
	if err := p.context.ctx.Err(); err != nil {
		return fail(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fail(err)
	}
	if cwd != p.context.cwd || !slices.Equal(os.Environ(), p.context.environment) {
		return fail(ErrStdioLaunchChanged)
	}
	current, err := readStdioLaunchImage(p.context.ctx, p.image.path)
	recheck = &current
	if err != nil {
		return fail(err)
	}
	if !os.SameFile(p.image.info, current.info) || p.image.info.Size() != current.info.Size() ||
		!p.image.info.ModTime().Equal(current.info.ModTime()) || p.image.digest != current.digest {
		return fail(ErrStdioLaunchChanged)
	}
	return nil
}

// SameExecution compares genuine calculations from one captured context. It
// provides data only; the API remains responsible for accepting their meaning.
func (p *StdioLaunchPreparation) SameExecution(other *StdioLaunchPreparation) (bool, error) {
	if err := p.Validate(); err != nil {
		return false, err
	}
	if err := other.Validate(); err != nil {
		return false, err
	}
	if p.context != other.context {
		return false, &StdioLaunchPreparationError{cause: ErrStdioLaunchContext, partial: p}
	}
	if p.command.Path != other.command.Path || p.command.Dir != other.command.Dir ||
		!slices.Equal(p.command.Args, other.command.Args) {
		return false, nil
	}
	left, err := effectiveStdioEnvironment(p.command.Env)
	if err != nil {
		return false, &StdioLaunchPreparationError{cause: err, partial: p}
	}
	right, err := effectiveStdioEnvironment(other.command.Env)
	if err != nil {
		return false, &StdioLaunchPreparationError{cause: err, partial: other}
	}
	if len(left) != len(right) {
		return false, nil
	}
	for key, value := range left {
		if actual, present := right[key]; !present || actual != value {
			return false, nil
		}
	}
	return true, nil
}

func effectiveStdioEnvironment(environment []string) (map[string]string, error) {
	values := make(map[string]string, len(environment))
	for _, entry := range environment {
		key, value, present := strings.Cut(entry, "=")
		if !present || key == "" || strings.ContainsRune(entry, 0) {
			return nil, errors.New("stdio environment entry is not representable")
		}
		values[envKeyNorm(key)] = value
	}
	return values, nil
}

type stdioLaunchImage struct {
	path           string
	info           os.FileInfo
	digest         [sha256.Size]byte
	closeAttempted bool
	closeError     error
}

func readStdioLaunchImage(ctx context.Context, path string) (image stdioLaunchImage, resultErr error) {
	if err := ctx.Err(); err != nil {
		return image, err
	}
	if !filepath.IsAbs(path) {
		return image, ErrStdioLaunchContext
	}
	image.path = path
	f, err := os.Open(path)
	if err != nil {
		return image, err
	}
	defer func() {
		image.closeAttempted = true
		image.closeError = f.Close()
		resultErr = errors.Join(resultErr, image.closeError)
	}()
	image.info, err = f.Stat()
	if err != nil {
		return image, err
	}
	if !image.info.Mode().IsRegular() {
		return image, errors.New("stdio launch image is not a regular file")
	}
	hash := sha256.New()
	count, err := io.Copy(hash, io.LimitReader(stdioLaunchReader{ctx: ctx, reader: f}, image.info.Size()+1))
	if err != nil {
		return image, err
	}
	after, err := f.Stat()
	if err != nil {
		return image, err
	}
	if count != image.info.Size() || after.Size() != image.info.Size() ||
		!after.ModTime().Equal(image.info.ModTime()) || !os.SameFile(after, image.info) {
		return image, ErrStdioLaunchChanged
	}
	copy(image.digest[:], hash.Sum(nil))
	return image, nil
}

type stdioLaunchReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r stdioLaunchReader) Read(bytes []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(bytes)
}
