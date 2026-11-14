package api

import (
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// SerenaPreparationPolicyV1 is a historical codec only. Its original field
// order, identity and validation describe old records, never live support.
type SerenaPreparationPolicyV1 struct {
	Version             int              `json:"version"`
	UVVersion           string           `json:"uv_version"`
	InterpreterSelector string           `json:"interpreter_selector"`
	WorkLimit           time.Duration    `json:"work_limit_ns"`
	CleanupLimit        time.Duration    `json:"cleanup_limit_ns"`
	OwnedTreeLimits     SerenaTreeLimits `json:"owned_tree_limits"`
}

// These installation-policy limits are not service startup/repair limits.
// Cold-build fit is unverified until the admitted production acceptance run.
func historicalSerenaPreparationPolicy() SerenaPreparationPolicyV1 {
	return SerenaPreparationPolicyV1{Version: 1, UVVersion: "0.9.21", InterpreterSelector: "3.13.11", WorkLimit: 300 * time.Second, CleanupLimit: 45 * time.Second, OwnedTreeLimits: SerenaTreeLimits{Entries: 131072, Depth: 64, RecordBytes: 64 << 20, EncodedPathBytes: 64 << 20, LogicalBytes: 32 << 30, OpenHandles: 256}}
}

func (p SerenaPreparationPolicyV1) Validate() error {
	if p.Version != 1 || p.UVVersion != "0.9.21" || p.WorkLimit <= 0 || p.CleanupLimit <= 0 || !p.OwnedTreeLimits.valid() {
		return errors.New("SERENA_PREPARATION_POLICY_INVALID")
	}
	parts := strings.Split(p.InterpreterSelector, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return errors.New("SERENA_PREPARATION_INTERPRETER_SELECTOR_INVALID")
	}
	version := [3]int{}
	for i, component := range parts {
		v, err := strconv.Atoi(component)
		if err != nil || v < 0 || strconv.Itoa(v) != component {
			return errors.New("SERENA_PREPARATION_INTERPRETER_SELECTOR_INVALID")
		}
		version[i] = v
	}
	if version[0] != 3 || version[1] < 11 || version[1] >= 15 {
		return errors.New("SERENA_PREPARATION_INTERPRETER_SELECTOR_UNSUPPORTED")
	}
	return nil
}

func (p SerenaPreparationPolicyV1) IdentitySHA256() (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return preparedSerenaHash(raw), nil
}

// The security floor applies before ordinary uv installation effects. The
// Python source floor is owned here alongside discovery and tuple admission.
const serenaUVSecurityMinimum = "0.13.4"
const serenaPythonSourceMinimum = "3.11"

type SerenaPreparationPolicyV2 struct {
	Version              int              `json:"version"`
	UVMinimumVersion     string           `json:"uv_minimum_version"`
	PythonMinimumVersion string           `json:"python_minimum_version"`
	WorkLimit            time.Duration    `json:"work_limit_ns"`
	CleanupLimit         time.Duration    `json:"cleanup_limit_ns"`
	OwnedTreeLimits      SerenaTreeLimits `json:"owned_tree_limits"`
}

func DefaultSerenaPreparationPolicy() SerenaPreparationPolicyV2 {
	return SerenaPreparationPolicyV2{Version: 2, UVMinimumVersion: serenaUVSecurityMinimum, PythonMinimumVersion: serenaPythonSourceMinimum, WorkLimit: 300 * time.Second, CleanupLimit: 45 * time.Second, OwnedTreeLimits: DefaultSerenaTreeLimits()}
}

// A present override is complete. Missing values are never filled from defaults.
func ResolveSerenaPreparationPolicy(override *SerenaPreparationPolicyV2) (SerenaPreparationPolicyV2, error) {
	p := DefaultSerenaPreparationPolicy()
	if override != nil {
		p = *override
	}
	if err := p.Validate(); err != nil {
		return SerenaPreparationPolicyV2{}, err
	}
	return p, nil
}

func serenaNumericVersion(value string, minimumParts int) ([3]int, error) {
	var result [3]int
	parts := strings.Split(value, ".")
	if len(parts) < minimumParts || len(parts) > len(result) {
		return result, errors.New("SERENA_VERSION_EVIDENCE_INVALID")
	}
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 || strconv.Itoa(n) != part {
			return result, errors.New("SERENA_VERSION_EVIDENCE_INVALID")
		}
		result[i] = n
	}
	if result == [3]int{} {
		return result, errors.New("SERENA_VERSION_EVIDENCE_INVALID")
	}
	return result, nil
}
func serenaVersionCompare(a, b [3]int) int {
	for i := range a {
		if a[i] < b[i] {
			return -1
		}
		if a[i] > b[i] {
			return 1
		}
	}
	return 0
}
func (p SerenaPreparationPolicyV2) Validate() error {
	if err := p.validateCanonical(); err != nil {
		return err
	}
	uv, _ := serenaNumericVersion(p.UVMinimumVersion, 3)
	floor, _ := serenaNumericVersion(serenaUVSecurityMinimum, 3)
	if serenaVersionCompare(uv, floor) < 0 {
		return errors.New("SERENA_PREPARATION_UV_MINIMUM_INVALID")
	}
	py, _ := serenaNumericVersion(p.PythonMinimumVersion, 2)
	pyfloor, _ := serenaNumericVersion(serenaPythonSourceMinimum, 2)
	if serenaVersionCompare(py, pyfloor) < 0 {
		return errors.New("SERENA_PREPARATION_PYTHON_MINIMUM_INVALID")
	}
	return nil
}

// Historical bodies retain their canonical identity independently of today's
// source/security floor. Only live resolution applies that current floor.
func (p SerenaPreparationPolicyV2) validateCanonical() error {
	if p.Version != 2 || p.WorkLimit <= 0 || p.CleanupLimit <= 0 || !p.OwnedTreeLimits.valid() {
		return errors.New("SERENA_PREPARATION_POLICY_INVALID")
	}
	_, e := serenaNumericVersion(p.UVMinimumVersion, 3)
	if e != nil {
		return errors.Join(e, errors.New("SERENA_PREPARATION_UV_MINIMUM_INVALID"))
	}
	_, e = serenaNumericVersion(p.PythonMinimumVersion, 2)
	if e != nil {
		return errors.Join(e, errors.New("SERENA_PREPARATION_PYTHON_MINIMUM_INVALID"))
	}
	return nil
}
func (p SerenaPreparationPolicyV2) IdentitySHA256() (string, error) {
	if e := p.Validate(); e != nil {
		return "", e
	}
	return p.canonicalIdentitySHA256()
}

func (p SerenaPreparationPolicyV2) canonicalIdentitySHA256() (string, error) {
	if err := p.validateCanonical(); err != nil {
		return "", err
	}
	raw, e := json.Marshal(p)
	if e != nil {
		return "", e
	}
	return preparedSerenaHash(raw), nil
}
func (p SerenaPreparationPolicyV2) pythonFindArgs() []string {
	return []string{"python", "find", "--no-project", "--system", "--no-config", "--no-python-downloads", "--offline", ">=" + p.PythonMinimumVersion}
}

// The token grammar accepts stable, pre-release and build versions. A release
// candidate of the floor is below its final release, rather than patched proof.
var serenaUVVersionToken = regexp.MustCompile(`^([0-9]+\.[0-9]+\.[0-9]+)(?:[-.]?((?:a|b|rc|alpha|beta|dev|pre|preview)[-.]?[0-9]+)(?:[.-][A-Za-z0-9]+)*)?(?:\+[A-Za-z0-9]+(?:[.-][A-Za-z0-9]+)*)?$`)

func (p SerenaPreparationPolicyV2) validateUVVersion(raw []byte) error {
	banner := strings.TrimSpace(string(raw))
	fields := strings.Fields(banner)
	if len(fields) < 2 || fields[0] != "uv" || strings.ContainsAny(banner, "\r\n\x00") {
		return errors.New("SERENA_PREPARATION_UV_VERSION_EVIDENCE_UNAVAILABLE")
	}
	if len(fields) > 2 && (!strings.HasPrefix(fields[2], "(") || !strings.HasSuffix(fields[len(fields)-1], ")")) {
		return errors.New("SERENA_PREPARATION_UV_VERSION_EVIDENCE_UNAVAILABLE")
	}
	match := serenaUVVersionToken.FindStringSubmatch(fields[1])
	if match == nil {
		return errors.New("SERENA_PREPARATION_UV_VERSION_EVIDENCE_UNAVAILABLE")
	}
	actual, e := serenaNumericVersion(match[1], 3)
	if e != nil {
		return e
	}
	floor, e := serenaNumericVersion(p.UVMinimumVersion, 3)
	if e != nil {
		return e
	}
	comparison := serenaVersionCompare(actual, floor)
	if comparison < 0 || comparison == 0 && match[2] != "" {
		return errors.New("SERENA_PREPARATION_UV_BELOW_MINIMUM")
	}
	return nil
}
func (p SerenaPreparationPolicyV2) validatePythonVersion(actual []int) error {
	floor, e := serenaNumericVersion(p.PythonMinimumVersion, 2)
	if e != nil {
		return e
	}
	if len(actual) != 3 || actual[0] < 0 || actual[1] < 0 || actual[2] < 0 || [3]int{actual[0], actual[1], actual[2]} == [3]int{} {
		return errors.New("SERENA_PREPARATION_INTERPRETER_VERSION_EVIDENCE_INVALID")
	}
	if serenaVersionCompare([3]int{actual[0], actual[1], actual[2]}, floor) < 0 {
		return errors.New("SERENA_PREPARATION_INTERPRETER_UNSUPPORTED")
	}
	return nil
}
