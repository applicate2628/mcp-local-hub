package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

func preparedStdioFixture(t *testing.T, snapshot *StdioLaunchContext, env map[string]string) (*StdioHost, *StdioLaunchPreparation) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewPreparedStdioHost(snapshot.ctx, StdioLaunchInputs{
		Context: snapshot, Config: HostConfig{Command: executable, Env: env},
		BaseArgs: []string{"-test.run=^$"},
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err := h.PreparedLaunch()
	if err != nil {
		t.Fatal(err)
	}
	return h, p
}

func TestStdioLaunchExecutionProfileDataAndFingerprint(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	snapshot, err := CaptureStdioLaunchContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h, p := preparedStdioFixture(t, snapshot, nil)
	// In-package actorless calculation fixtures isolate the framing from the
	// test host's environment. They are never passed to StartPrepared.
	p.command.Env = []string{}
	empty, err := p.ObserveExecutionProfile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if empty.EffectiveEnvironmentSHA256 != "f2301b10252ee88984123d19eb7332e6c1f5250709615942ad045a9d03853b27" {
		t.Fatal("empty effective environment does not match the fixed framing vector")
	}
	p.command.Env = []string{"z=two", "a=first", "a=last"}
	profile, err := p.ObserveExecutionProfile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if profile.CommandPath != p.CommandPath() || profile.WorkingDirectory != snapshot.WorkingDirectory() ||
		len(profile.Arguments) != 1 || profile.Arguments[0] != "-test.run=^$" ||
		profile.EffectiveEnvironmentSHA256 != "b41fd2c189aabc954d404c70a1a989448183f34e0702fdfa2463ffe910721647" {
		t.Fatal("profile did not receive the actual ordered calculation and effective environment")
	}
	profile.Arguments[0] = "mutated DATA copy"
	if p.Arguments()[0] != "-test.run=^$" {
		t.Fatal("profile arguments alias the retained command")
	}
	p.command.Env = []string{"a=last", "z=two"}
	reordered, err := p.ObserveExecutionProfile(ctx)
	if err != nil || reordered.EffectiveEnvironmentSHA256 != profile.EffectiveEnvironmentSHA256 {
		t.Fatal("entry order or superseded duplicate changed effective identity")
	}
	p.command.Env = []string{"A=first", "a=last", "z=two"}
	cased, err := p.ObserveExecutionProfile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if same := cased.EffectiveEnvironmentSHA256 == profile.EffectiveEnvironmentSHA256; same != (runtime.GOOS == "windows") {
		t.Fatal("profile did not retain the existing platform environment-key semantics")
	}
	p.command.Env = []string{"a=changed", "z=two"}
	changed, err := p.ObserveExecutionProfile(ctx)
	if err != nil || changed.EffectiveEnvironmentSHA256 == profile.EffectiveEnvironmentSHA256 {
		t.Fatal("effective value change was not represented")
	}
	p.command.Env = []string{"a=last", "z=two", "empty="}
	extraEmpty, err := p.ObserveExecutionProfile(ctx)
	if err != nil || extraEmpty.EffectiveEnvironmentSHA256 == profile.EffectiveEnvironmentSHA256 {
		t.Fatal("an additional empty value was omitted from effective identity")
	}
	const sentinel = "synthetic-private-profile-value"
	private := StdioLaunchExecutionProfile{CommandPath: sentinel, Arguments: []string{sentinel}, WorkingDirectory: sentinel, EffectiveEnvironmentSHA256: sentinel}
	if private.String() != "<stdio launch execution profile>" || private.GoString() != private.String() {
		t.Fatal("private profile string methods changed their fixed mask")
	}
	for _, value := range []any{private, &private} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
			if strings.Contains(fmt.Sprintf(verb, value), sentinel) {
				t.Fatal("profile formatting exposed private fields")
			}
		}
		raw, err := json.Marshal(value)
		if err != nil || string(raw) != "{}" {
			t.Fatal("profile became a public JSON carrier")
		}
	}
	if h.cmd != nil || h.original != nil || h.job != nil || p.command.Process != nil || p.consumed.Load() {
		t.Fatal("read-only profile consumed or launched the preparation")
	}
}

func TestStdioLaunchExecutionProfileLifetime(t *testing.T) {
	for _, name := range []string{"nil context", "foreign context", "cancelled", "consumed", "generation changed", "ambient changed"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			snapshot, err := CaptureStdioLaunchContext(ctx)
			if err != nil {
				t.Fatal(err)
			}
			h, p := preparedStdioFixture(t, snapshot, nil)
			observedContext := ctx
			want := ErrStdioLaunchContext
			switch name {
			case "nil context":
				observedContext = nil
			case "foreign context":
				observedContext = context.WithValue(ctx, struct{}{}, "different context identity")
			case "cancelled":
				cancel()
				want = context.Canceled
			case "consumed":
				p.consumed.Store(true)
				want = ErrStdioLaunchConsumed
			case "generation changed":
				h.preparedGeneration = &stdioLaunchGeneration{issued: true}
				want = ErrStdioLaunchPartial
			case "ambient changed":
				t.Setenv("MCPHUB_STDIO_PROFILE_DRIFT_TEST", "synthetic-change")
				want = ErrStdioLaunchChanged
			}
			view, err := p.ObserveExecutionProfile(observedContext)
			var failure *StdioLaunchPreparationError
			if !errors.Is(err, want) || !errors.As(err, &failure) || failure.PartialPreparation() != p {
				t.Fatalf("profile refusal lost original partial/cause: %v", err)
			}
			if view.CommandPath != "" || view.Arguments != nil || view.WorkingDirectory != "" || view.EffectiveEnvironmentSHA256 != "" {
				t.Fatal("refused profile returned usable DATA")
			}
			if h.cmd != nil || h.original != nil || p.command.Process != nil {
				t.Fatal("refused read-only observation reached a creator")
			}
		})
	}
}

func TestStdioLaunchExecutionProfilePartialOutcomes(t *testing.T) {
	t.Run("expired original context", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Time{})
		defer cancel()
		snapshot, captureErr := CaptureStdioLaunchContext(ctx)
		if !errors.Is(captureErr, context.DeadlineExceeded) {
			t.Fatal("fixture did not retain an expired original context")
		}
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		_, prepareErr := NewPreparedStdioHost(ctx, StdioLaunchInputs{Context: snapshot, Config: HostConfig{Command: executable}})
		var partial *StdioLaunchPreparationError
		if !errors.As(prepareErr, &partial) || partial.PartialPreparation() == nil {
			t.Fatal("fixture lost the original partial preparation")
		}
		p := partial.PartialPreparation()
		_, err = p.ObserveExecutionProfile(ctx)
		var failure *StdioLaunchPreparationError
		// Preserve the original preparation's actual cause, including its
		// existing incomplete-capture refusal; do not relabel it here.
		if !errors.Is(err, partial.Unwrap()) || !errors.As(err, &failure) || failure.PartialPreparation() != p {
			t.Fatal("expired profile discarded the original cause/partial")
		}
	})
	t.Run("invalid effective environment", func(t *testing.T) {
		ctx := context.Background()
		snapshot, err := CaptureStdioLaunchContext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		h, p := preparedStdioFixture(t, snapshot, nil)
		p.command.Env = []string{"synthetic-entry-without-separator"}
		view, err := p.ObserveExecutionProfile(ctx)
		var failure *StdioLaunchPreparationError
		if err == nil || !errors.As(err, &failure) || failure.PartialPreparation() != p || view.EffectiveEnvironmentSHA256 != "" {
			t.Fatal("invalid effective environment was hidden or lost its partial outcome")
		}
		if h.cmd != nil || h.original != nil || p.command.Process != nil {
			t.Fatal("invalid profile acquired process custody")
		}
	})
}

type foreignProjectServicePreparedLaunch struct{}

func (foreignProjectServicePreparedLaunch) ProjectServiceExecutionProfile(context.Context) (api.ProjectServiceExecutionProfile, error) {
	panic("foreign DATA port must not be consulted by the lower equality owner")
}
func (foreignProjectServicePreparedLaunch) SameProjectServiceExecution(context.Context, api.ProjectServicePreparedLaunch) (bool, error) {
	panic("foreign equality must not replace actual preparation comparison")
}
func (foreignProjectServicePreparedLaunch) EnvironmentValueMatches(string, string) (bool, bool) {
	return false, false
}

func TestStdioLaunchProjectServicePortUsesOriginalOwner(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	snapshot, err := CaptureStdioLaunchContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, p := preparedStdioFixture(t, snapshot, nil)
	_, peer := preparedStdioFixture(t, snapshot, nil)
	var port api.ProjectServicePreparedLaunch = p
	view, err := port.ProjectServiceExecutionProfile(ctx)
	if err != nil || view.CommandPath != p.CommandPath() || view.WorkingDirectory != p.WorkingDirectory() {
		t.Fatal("API port did not map the genuine view")
	}
	view.Arguments[0] = "changed API DATA"
	if p.Arguments()[0] != "-test.run=^$" {
		t.Fatal("API view aliases the retained command")
	}
	if equal, err := port.SameProjectServiceExecution(ctx, peer); err != nil || !equal {
		t.Fatalf("same genuine pair = %v, %v", equal, err)
	}
	if _, err := port.SameProjectServiceExecution(ctx, foreignProjectServicePreparedLaunch{}); !errors.Is(err, ErrStdioLaunchContext) {
		t.Fatal("foreign concrete port qualified")
	}
	if _, err := port.SameProjectServiceExecution(ctx, (*StdioLaunchPreparation)(nil)); !errors.Is(err, ErrStdioLaunchPartial) {
		t.Fatal("nil concrete peer qualified")
	}
	foreignContext := context.WithValue(ctx, struct{}{}, "foreign")
	if _, err := port.SameProjectServiceExecution(foreignContext, peer); !errors.Is(err, ErrStdioLaunchContext) {
		t.Fatal("fresh context rebound the original pair")
	}
	peer.command.Env = append(peer.command.Env, "MCPHUB_PROJECT_SERVICE_PORT_TEST=different")
	if equal, err := port.SameProjectServiceExecution(ctx, peer); err != nil || equal {
		t.Fatal("API port compared supplied DATA instead of actual effective execution")
	}
}

func TestStdioLaunchPreparationRejectsForeignReceiver(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	snapshot, err := CaptureStdioLaunchContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewPreparedStdioHost(ctx, StdioLaunchInputs{Context: snapshot, Config: HostConfig{Command: executable}})
	if err != nil {
		t.Fatal(err)
	}
	// All mutex/atomic fields are unused at this point. This is the exact
	// exported concrete-host copy that previously bypassed owner validation.
	foreign := *h
	if err := foreign.StartPrepared(); !errors.Is(err, ErrStdioLaunchPartial) {
		t.Fatalf("foreign receiver = %v", err)
	}
	if h.prepared.consumed.Load() || foreign.original != nil || foreign.cmd != nil || h.original != nil {
		t.Fatal("foreign receiver consumed or acquired original custody")
	}
	if err := h.prepared.Validate(); err != nil {
		t.Fatalf("foreign refusal damaged original preparation: %v", err)
	}
}

func TestStdioLaunchPreparationActorlessComparison(t *testing.T) {
	snapshot, err := CaptureStdioLaunchContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	const sentinel = "synthetic-stdio-private-environment-value"
	h, p := preparedStdioFixture(t, snapshot, map[string]string{"MCPHUB_STDIO_TEST_PRIVATE": sentinel})
	_, same := preparedStdioFixture(t, snapshot, map[string]string{"MCPHUB_STDIO_TEST_PRIVATE": sentinel})
	if equal, err := p.SameExecution(same); err != nil || !equal {
		t.Fatalf("same execution = (%v,%v)", equal, err)
	}
	if h.cmd != nil || p.command.Process != nil || p.command.Stdin != nil || p.command.Stdout != nil || p.command.Stderr != nil || h.original != nil || h.job != nil {
		t.Fatal("actorless preparation acquired a process, pipe or native owner")
	}
	args := p.Arguments()
	args[0] = "mutated clone"
	if p.Arguments()[0] != "-test.run=^$" {
		t.Fatal("argument clone changed original command")
	}
	if present, equal := p.EnvironmentValueMatches("MCPHUB_STDIO_TEST_PRIVATE", sentinel); !present || !equal {
		t.Fatal("actual private environment did not compare")
	}
	for _, value := range []any{snapshot, p, StdioLaunchInputs{Config: HostConfig{Env: map[string]string{"plain": sentinel}}}} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), sentinel) || strings.Contains(fmt.Sprintf("%#v", value), sentinel) {
			t.Fatal("public formatting exposed private material")
		}
	}
	_, extra := preparedStdioFixture(t, snapshot, map[string]string{"MCPHUB_STDIO_TEST_PRIVATE": sentinel, "MCPHUB_STDIO_TEST_EXTRA": ""})
	if equal, err := p.SameExecution(extra); err != nil || equal {
		t.Fatalf("extra empty key comparison = (%v,%v)", equal, err)
	}
	foreignContext, err := CaptureStdioLaunchContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, foreign := preparedStdioFixture(t, foreignContext, map[string]string{"MCPHUB_STDIO_TEST_PRIVATE": sentinel})
	if _, err := p.SameExecution(foreign); !errors.Is(err, ErrStdioLaunchContext) {
		t.Fatalf("foreign capture comparison = %v", err)
	}
}

func TestStdioLaunchContextFormatting(t *testing.T) {
	const privateValue = "synthetic-stdio-context-private-value"
	cause := errors.New(privateValue)
	snapshot := &StdioLaunchContext{ctx: context.Background(), environment: []string{"PRIVATE=" + privateValue}, cwd: privateValue, failure: cause}
	snapshot.owner = snapshot
	var nilContext *StdioLaunchContext
	for _, item := range []struct {
		name      string
		value     any
		formatted string
		json      string
	}{
		{"pointer", snapshot, "<stdio launch context>", "{}"},
		{"value", *snapshot, "<stdio launch context>", "{}"},
		{"zero pointer", &StdioLaunchContext{}, "<stdio launch context>", "{}"},
		{"zero value", StdioLaunchContext{}, "<stdio launch context>", "{}"},
		{"nil", nilContext, "<nil>", "null"},
	} {
		t.Run(item.name, func(t *testing.T) {
			for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
				if got := fmt.Sprintf(verb, item.value); got != item.formatted {
					t.Fatalf("context formatting for %s differed from fixed marker", verb)
				}
			}
			encoded, err := json.Marshal(item.value)
			if err != nil || string(encoded) != item.json {
				t.Fatal("context JSON contract changed")
			}
		})
	}
	if snapshot.owner != snapshot || snapshot.cwd != privateValue || len(snapshot.environment) != 1 || snapshot.environment[0] != "PRIVATE="+privateValue || snapshot.failure != cause || !errors.Is(snapshot.failure, cause) {
		t.Fatal("formatting changed original private capture or failure")
	}
}

func TestStdioLaunchPreparationRefusesBeforeCreator(t *testing.T) {
	t.Run("actual ambient changed", func(t *testing.T) {
		snapshot, err := CaptureStdioLaunchContext(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		h, p := preparedStdioFixture(t, snapshot, nil)
		t.Setenv("MCPHUB_STDIO_PREPARATION_DRIFT_TEST", "changed")
		if err := h.StartPrepared(); !errors.Is(err, ErrStdioLaunchChanged) {
			t.Fatalf("ambient drift = %v", err)
		}
		if p.command.Process != nil || h.original != nil || h.cmd != nil {
			t.Fatal("stale preparation reached creator")
		}
	})
	t.Run("original cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		snapshot, err := CaptureStdioLaunchContext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		h, p := preparedStdioFixture(t, snapshot, nil)
		cancel()
		if err := h.StartPrepared(); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled preparation = %v", err)
		}
		if p.command.Process != nil || h.original != nil {
			t.Fatal("cancelled preparation reached creator")
		}
	})
	t.Run("partial cause retained", func(t *testing.T) {
		snapshot, err := CaptureStdioLaunchContext(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		_, err = NewPreparedStdioHost(snapshot.ctx, StdioLaunchInputs{Context: snapshot, Config: HostConfig{Command: filepath.Join(t.TempDir(), "missing-image.exe")}})
		var failure *StdioLaunchPreparationError
		if !errors.As(err, &failure) || failure.Unwrap() == nil || failure.PartialPreparation() == nil {
			t.Fatal("dropped partial return lost original cause/preparation")
		}
		p := failure.PartialPreparation()
		if p.command.Process != nil {
			t.Fatal("partial preparation created a process")
		}
		if err := p.owner.StartPrepared(); err == nil {
			t.Fatal("partial preparation was startable")
		}
	})
	t.Run("single use guard", func(t *testing.T) {
		snapshot, err := CaptureStdioLaunchContext(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		h, p := preparedStdioFixture(t, snapshot, nil)
		// Synthetic consumed precondition; this is not a claimed successful Start.
		p.consumed.Store(true)
		if err := h.StartPrepared(); !errors.Is(err, ErrStdioLaunchConsumed) {
			t.Fatalf("consumed preparation = %v", err)
		}
		if p.command.Process != nil || h.original != nil {
			t.Fatal("replay reached creator")
		}
	})
}

func TestStdioLaunchPreparationDetectsImageReplacement(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	image := filepath.Join(t.TempDir(), "copied-test-image.exe")
	r, err := os.Open(executable)
	if err != nil {
		t.Fatal(err)
	}
	w, err := os.OpenFile(image, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
	if err != nil {
		_ = r.Close()
		t.Fatal(err)
	}
	_, copyErr := io.Copy(w, r)
	if err := errors.Join(copyErr, r.Close(), w.Close()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := CaptureStdioLaunchContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewPreparedStdioHost(snapshot.ctx, StdioLaunchInputs{Context: snapshot, Config: HostConfig{Command: image}})
	if err != nil {
		t.Fatal(err)
	}
	p, err := h.PreparedLaunch()
	if err != nil {
		t.Fatal(err)
	}
	if !p.image.closeAttempted || p.image.closeError != nil {
		t.Fatal("original image-reader close was not received")
	}
	w, err = os.OpenFile(image, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := w.Write([]byte("source-drift"))
	if err := errors.Join(writeErr, w.Close()); err != nil {
		t.Fatal(err)
	}
	if err := h.StartPrepared(); !errors.Is(err, ErrStdioLaunchChanged) {
		t.Fatalf("image drift = %v", err)
	}
	if p.command.Process != nil || h.original != nil {
		t.Fatal("image drift reached creator")
	}
}
