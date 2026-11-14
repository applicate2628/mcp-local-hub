//go:build windows && amd64 && cgo

// Package caller is an isolated foreign-caller experiment, not a Hub backend.
package caller

/*
#cgo CFLAGS: -std=c11 -Wall -Wextra -Werror
#include <windows.h>
#include <stdlib.h>
#include <string.h>
#include "../include/owner_v1.h"
typedef struct {
    HMODULE module;
    mh_create_fn create;
    mh_create_env_fn create_env;
    mh_create_io_fn create_io;
    mh_admit_fn admit;
    mh_stop_fn stop;
    mh_finish_fn finish;
    mh_release_fn release;
    mh_admit_checked_fn admit_checked;
} bridge;
static bridge *bridge_open(const uint16_t *path, uint32_t *error) {
    bridge *b = (bridge*)calloc(1, sizeof(*b));
    if (!b) { *error=ERROR_NOT_ENOUGH_MEMORY; return NULL; }
    b->module=LoadLibraryExW((LPCWSTR)path, NULL,
        LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR|LOAD_LIBRARY_SEARCH_SYSTEM32);
    if (!b->module) { *error=GetLastError(); free(b); return NULL; }
    // GetProcAddress returns a generic function pointer; copy its representation
    // into a declared function type without hiding a compiler cast warning.
    FARPROC p; mh_version_fn version;
#define RESOLVE(target, symbol) do { p=GetProcAddress(b->module,symbol); \
    if (!p) { *error=GetLastError(); goto fail; } \
    _Static_assert(sizeof(target)==sizeof(p), "function pointer size"); \
    memcpy(&(target), &p, sizeof(target)); } while (0)
    RESOLVE(version,"mh_version1");
    RESOLVE(b->create,"mh_create1"); RESOLVE(b->admit,"mh_admit1");
    RESOLVE(b->stop,"mh_stop1"); RESOLVE(b->finish,"mh_finish1");
    RESOLVE(b->release,"mh_release1");
    RESOLVE(b->admit_checked,"mh_admit_checked1");
    RESOLVE(b->create_env,"mh_create_env1");
    RESOLVE(b->create_io,"mh_create_io1");
#undef RESOLVE
    if (version()!=1) { *error=ERROR_REVISION_MISMATCH; goto fail; }
    return b;
fail:
    FreeLibrary(b->module); free(b); return NULL;
}
static uint32_t bridge_close(bridge *b) {
    if (!FreeLibrary(b->module)) return GetLastError();
    free(b); return 0;
}
static uint32_t bridge_create(bridge *b, const mh_create *c, mh_owner **p, mh_out *o) {
    return b->create(c,sizeof(*c),p,o,sizeof(*o));
}
static uint32_t bridge_create_io(bridge *b,const mh_create *c,const mh_env_entry *entries,size_t count,
    uintptr_t in,uintptr_t out,uintptr_t err,mh_owner **p,mh_out *o) {
    mh_io_context context={{entries,count},{in,out,err}};
    return b->create_io(c,sizeof(*c),&context,sizeof(context),p,o,sizeof(*o));
}
static uint32_t bridge_create_env(bridge *b,const mh_create *c,const mh_env_entry *entries,size_t count,mh_owner **p,mh_out *o) {
    mh_environment env={entries,count};
    return b->create_env(c,sizeof(*c),&env,sizeof(env),p,o,sizeof(*o));
}static uint32_t bridge_admit(bridge *b,mh_owner *p,uint32_t a,mh_out *o){return b->admit(p,a,o,sizeof(*o));}
static uint32_t bridge_stop(bridge *b,mh_owner *p,uint32_t a,mh_out *o){return b->stop(p,a,o,sizeof(*o));}
static uint32_t bridge_finish(bridge *b,mh_owner *p,uint32_t a,mh_out *o){return b->finish(p,a,o,sizeof(*o));}
static uint32_t bridge_release(bridge *b,mh_owner **p,mh_out *o){return b->release(p,o,sizeof(*o));}
extern uint32_t mh_go_admission(uint32_t, uint16_t*, size_t, uintptr_t);
static uint32_t bridge_check(uint32_t pid, const uint16_t *image, size_t n, uintptr_t token) {
    // cgo exports mutable pointer types; the Go handler treats this as read-only
    // and immediately copies. No permission to mutate the Rust buffer is granted.
    return mh_go_admission(pid,(uint16_t*)image,n,token);
}
static uint32_t bridge_checked(bridge *b,mh_owner *p,uintptr_t token,mh_out *o) {
    return b->admit_checked(p,bridge_check,token,o,sizeof(*o));
}
*/
import "C"

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime/cgo"
	"sync"
	"syscall"
	"unicode/utf8"
	"unsafe"
)

const (
	ok            = 0
	argument      = 1
	abiMismatch   = 2
	nativeFailure = 3
	denied        = 4
	invalidState  = 5
	notSettled    = 6
	pending       = 1
	complete      = 2
	cleanupFailed = 3
)

type Outcome struct {
	State, Flags, PID, RootExit, Active uint32
	NativeError                         int32
	CleanupMask                         uint32
	CleanupCodes                        [3]int32
}
type CallError struct {
	Status uint32
	Native int32
}

func (e *CallError) Error() string {
	return fmt.Sprintf("owner ABI status=%d native=%d", e.Status, e.Native)
}
func result(status C.uint32_t, o C.mh_out) (Outcome, error) {
	v := Outcome{State: uint32(o.state), Flags: uint32(o.flags), PID: uint32(o.pid), RootExit: uint32(o.root_exit), Active: uint32(o.active), NativeError: int32(o.native_error), CleanupMask: uint32(o.cleanup_mask)}
	for i := range v.CleanupCodes {
		v.CleanupCodes[i] = int32(o.cleanup_codes[i])
	}
	layoutKnown := o.size == C.sizeof_mh_out && o.version == 1
	var callErr error
	if status != 0 {
		var native int32
		if layoutKnown && uint32(status) == nativeFailure {
			native = v.NativeError
		}
		callErr = &CallError{uint32(status), native}
	}
	if !layoutKnown {
		return Outcome{}, errors.Join(callErr, fmt.Errorf("%w: incompatible layout", ErrInvalidOutcome))
	}
	if reason := outcomeViolation(uint32(status), v); reason != "" {
		return Outcome{}, errors.Join(callErr, fmt.Errorf("%w: %s", ErrInvalidOutcome, reason))
	}
	return v, callErr
}

// Mutexes are intentionally owned at the calling boundary. Raw C callers must
// provide equivalent exclusive access themselves. These values must not be copied.
type Library struct {
	mu   sync.Mutex
	raw  *C.bridge
	live int
}
type Owner struct {
	mu  sync.Mutex
	lib *Library
	raw *C.mh_owner
}

func Open(path string) (*Library, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("absolute trusted DLL path required")
	}
	p, free, err := wide(path)
	if err != nil {
		return nil, err
	}
	defer free()
	var code C.uint32_t
	b := C.bridge_open(p.data, &code)
	if b == nil {
		return nil, syscall.Errno(code)
	}
	return &Library{raw: b}, nil
}
func (l *Library) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.live != 0 {
		return errors.New("cannot unload library with live owners")
	}
	if l.raw == nil {
		return nil
	}
	if e := C.bridge_close(l.raw); e != 0 {
		return syscall.Errno(e)
	}
	l.raw = nil
	return nil
}

// wide allocates C-owned input, including a private terminator for DLL loading.
// The Rust API receives a length excluding that terminator and copies it. There
// are no Go pointers in any input; all C allocations die before New returns.
func wide(s string) (C.mh_u16, func(), error) {
	if !utf8.ValidString(s) {
		return C.mh_u16{}, func() {}, &CallError{argument, 0}
	}
	units, err := syscall.UTF16FromString(s)
	if err != nil {
		return C.mh_u16{}, func() {}, &CallError{argument, 0}
	}
	if len(units) > 32767 {
		return C.mh_u16{}, func() {}, &CallError{argument, 0}
	}
	p := C.malloc(C.size_t(len(units) * 2))
	if p == nil {
		return C.mh_u16{}, func() {}, errors.New("input allocation failed")
	}
	copy(unsafe.Slice((*uint16)(p), len(units)), units)
	return C.mh_u16{data: (*C.uint16_t)(p), len: C.size_t(len(units) - 1)}, func() { C.free(p) }, nil
}

// New may return both a non-nil owner and an error after native ownership has
// transferred. Retain and explicitly settle/release every non-nil returned owner.
func (l *Library) New(program, cwd string, args []string) (*Owner, Outcome, error) {
	return l.newVersion(program, cwd, args, 1)
}

// NewWithEnvironment supplies the complete explicit child environment. Like New,
// it may return both an owner and an error after native ownership transfers.
// Retain every non-nil owner, even on error; never retry an unknown outcome.
func (l *Library) NewWithEnvironment(program, cwd string, args []string, environment []EnvironmentEntry) (*Owner, Outcome, error) {
	return l.newRequest(program, cwd, args, 1, environment, true, nil)
}
func (l *Library) newVersion(program, cwd string, args []string, version uint32) (*Owner, Outcome, error) {
	return l.newRequest(program, cwd, args, version, nil, false, nil)
}
func (l *Library) newRequest(program, cwd string, args []string, version uint32, environment []EnvironmentEntry, explicitEnvironment bool, streams *Stdio) (owner *Owner, observed Outcome, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.raw == nil {
		return nil, Outcome{}, errors.New("library closed")
	}
	if len(args) > 64 || len(environment) > 256 {
		return nil, Outcome{}, &CallError{argument, 0}
	}
	p, fp, err := wide(program)
	if err != nil {
		return nil, Outcome{}, err
	}
	defer fp()
	d, fd, err := wide(cwd)
	if err != nil {
		return nil, Outcome{}, err
	}
	defer fd()
	var array unsafe.Pointer
	if len(args) > 0 {
		array = C.calloc(C.size_t(len(args)), C.sizeof_mh_u16)
		if array == nil {
			return nil, Outcome{}, errors.New("args allocation failed")
		}
		defer C.free(array)
	}
	views := unsafe.Slice((*C.mh_u16)(array), len(args))
	for i, s := range args {
		v, free, e := wide(s)
		if e != nil {
			return nil, Outcome{}, e
		}
		defer free()
		views[i] = v
	}
	var envArray unsafe.Pointer
	if len(environment) > 0 {
		envArray = C.calloc(C.size_t(len(environment)), C.sizeof_mh_env_entry)
		if envArray == nil {
			return nil, Outcome{}, errors.New("environment allocation failed")
		}
		defer C.free(envArray)
	}
	envViews := unsafe.Slice((*C.mh_env_entry)(envArray), len(environment))
	blockUnits := 1 // final NUL; each entry includes its own '=' and NUL
	for i, entry := range environment {
		name, freeName, e := wide(entry.Name)
		if e != nil {
			return nil, Outcome{}, e
		}
		defer freeName()
		value, freeValue, e := wide(entry.Value)
		if e != nil {
			return nil, Outcome{}, e
		}
		defer freeValue()
		blockUnits += int(name.len) + int(value.len) + 2
		if blockUnits > 32766 {
			return nil, Outcome{}, &CallError{argument, 0}
		}
		envViews[i] = C.mh_env_entry{name: name, value: value}
	}
	request := C.mh_create{version: C.uint32_t(version), program: p, cwd: d, args: (*C.mh_u16)(array), argc: C.size_t(len(args))}
	var ptr *C.mh_owner
	var output C.mh_out
	var status C.uint32_t
	if streams != nil {
		copies, copyErr := duplicateStdio(*streams)
		if copyErr != nil {
			return nil, Outcome{}, copyErr
		}
		// Keep copy-close errors separate from native effects: an already
		// returned owner survives even a late temporary-handle cleanup failure.
		defer func() { err = errors.Join(err, copies.close()) }()
		status = C.bridge_create_io(l.raw, &request, (*C.mh_env_entry)(envArray), C.size_t(len(environment)),
			C.uintptr_t(copies[0]), C.uintptr_t(copies[1]), C.uintptr_t(copies[2]), &ptr, &output)
	} else if explicitEnvironment {
		status = C.bridge_create_env(l.raw, &request, (*C.mh_env_entry)(envArray), C.size_t(len(environment)), &ptr, &output)
	} else {
		status = C.bridge_create(l.raw, &request, &ptr, &output)
	}
	out, e := result(status, output)
	// A transferred owner is an independent fact, even if result metadata is
	// invalid. The caller must retain any non-nil Owner returned with an error.
	if ptr != nil {
		l.live++
		return &Owner{lib: l, raw: ptr}, out, e
	}
	if e == nil {
		e = errors.New("ABI success without an owner")
	}
	return nil, out, e
}
func (o *Owner) Admit(allow bool) (Outcome, error) {
	v := uint32(0)
	if allow {
		v = 1
	}
	return o.call(0, v)
}
func (o *Owner) Stop(code uint32) (Outcome, error)           { return o.call(1, code) }
func (o *Owner) Finish(milliseconds uint32) (Outcome, error) { return o.call(2, milliseconds) }
func (o *Owner) call(operation int, value uint32) (Outcome, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.callLocked(operation, value)
}

// callLocked requires this owner mutex. Both synchronous calls and the
// cancelable observer use the same native operation/result mapping.
func (o *Owner) callLocked(operation int, value uint32) (Outcome, error) {
	if o.raw == nil {
		return Outcome{}, errors.New("owner released")
	}
	var out C.mh_out
	var code C.uint32_t
	switch operation {
	case 0:
		code = C.bridge_admit(o.lib.raw, o.raw, C.uint32_t(value), &out)
	case 1:
		code = C.bridge_stop(o.lib.raw, o.raw, C.uint32_t(value), &out)
	case 2:
		code = C.bridge_finish(o.lib.raw, o.raw, C.uint32_t(value), &out)
	default:
		return Outcome{}, errors.New("invalid local operation")
	}
	observed, err := result(code, out)
	if err == nil && operation == 2 && observed.State == 0 {
		return Outcome{}, fmt.Errorf("%w: successful finish omitted settlement", ErrInvalidOutcome)
	}
	return observed, err
}

// Release never silently stops an active child. Call Finish until terminal first.
// No finalizer: GC cannot truthfully report settlement or native cleanup errors.
func (o *Owner) Release() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.raw == nil {
		return nil
	}
	var out C.mh_out
	code := C.bridge_release(o.lib.raw, &o.raw, &out)
	_, err := result(code, out)
	// Pointer clearing records completed deallocation, not successful outcome
	// reporting. Keep the library count correct even if metadata validation fails.
	if o.raw == nil {
		o.lib.mu.Lock()
		o.lib.live--
		o.lib.mu.Unlock()
	} else if err == nil {
		err = errors.New("release did not clear owner")
	}
	return err
}

// AdmitChecked evaluates only copied evidence from the original process handle,
// inside the native single-use admission. Policy runs with this owner locked:
// it must return and must not call owner methods (or wait for others to do so).
// No Go pointer or callback is retained in the DLL. Nil policy has no effect.
func (o *Owner) AdmitChecked(check func(ProcessObservation) error) (Outcome, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.raw == nil {
		return Outcome{}, errors.New("owner released")
	}
	if check == nil {
		return Outcome{}, &CallError{argument, 0}
	}
	state := &admissionCall{check: check}
	token := cgo.NewHandle(state)
	defer token.Delete()
	var out C.mh_out
	status := C.bridge_checked(o.lib.raw, o.raw, C.uintptr_t(token), &out)
	observed, err := result(status, out)
	if err == nil && state.calls != 1 {
		err = errors.New("checked admission returned without one policy decision")
	}
	return observed, errors.Join(err, state.err)
}
