package binaryadmission

import (
	"errors"
	"fmt"
	"strings"
)

const windowsBackendRunHoldNamespace = "mcphub-backend-run-hold-"
const windowsBackendRunHoldMarkerV1 = windowsBackendRunHoldNamespace + "v1"

var ErrWindowsBackendRunHoldUnverified = errors.New("exact Windows backend run hold build authority unavailable")

// An admitted declaration is exactly one LF-delimited line before the
// existing source/runtime/entry suffix. Other occurrences remain untrusted.
func splitWindowsBackendRunHoldMarker(prefix string) (string, bool, error) {
	lower := strings.ToLower(prefix)
	if !strings.Contains(lower, windowsBackendRunHoldNamespace) {
		return prefix, false, nil
	}
	head, found := strings.CutSuffix(prefix, "\n"+windowsBackendRunHoldMarkerV1)
	if !found || strings.HasSuffix(head, "\r") || strings.Count(lower, windowsBackendRunHoldNamespace) != 1 {
		return "", false, errors.New("invalid Windows backend run hold Comments marker")
	}
	return head, true, nil
}

// The three existing suffixes have one order: source, runtime, entry.
// Decode once in reverse using the existing entry-record owner; no parser
// calls back into a public composition reader and no marker is normalized.
type windowsBuildCommentsComposition struct {
	prefix,entryPrefix,sourceEpoch,runtimeSHA256 string
	entry WindowsCanonicalEntryBuildBindingV1
	hasEntry bool
	backendRunHoldV1 bool
}
func parseWindowsBuildCommentsComposition(comments string)(v windowsBuildCommentsComposition,err error){
	v.entryPrefix=comments
	if strings.Contains(strings.ToUpper(comments),canonicalEntryCommentsNamespace){
		v.entryPrefix,v.entry,err=ParseWindowsCanonicalEntryCommentsV1(comments)
		if err!=nil{return v,err};v.hasEntry=true
	}
	v.prefix,v.sourceEpoch,v.runtimeSHA256,err=parseWindowsBuildCommentsPrefix(v.entryPrefix)
	if err != nil {
		return v, err
	}
	v.prefix, v.backendRunHoldV1, err = splitWindowsBackendRunHoldMarker(v.prefix)
	if err == nil && v.backendRunHoldV1 && (!v.hasEntry || v.sourceEpoch == "" || v.runtimeSHA256 == "") {
		err = ErrWindowsBackendRunHoldUnverified
	}
	return v, err
}
func parseWindowsBuildCommentsPrefix(comments string)(prefix,epoch,runtime string,err error){
	if strings.Contains(strings.ToUpper(comments),canonicalEntryCommentsNamespace){return "","","",fmt.Errorf("unknown or reordered canonical entry Comments marker")}
	lower:=strings.ToLower(comments)
	const runtimeMarker="mcphub-runtime-v1;sha256="
	if strings.Contains(lower,"mcphub-runtime-"){
		if strings.Count(lower,"mcphub-runtime-")!=1||strings.Count(comments,runtimeMarker)!=1{return "","","",fmt.Errorf("RUNTIME_BUILD_MARKER_INVALID")}
		i:=strings.Index(comments,runtimeMarker)
		if i==0||comments[i-1]!='\n'||!validLowerHex(comments[i+len(runtimeMarker):],64,64){return "","","",fmt.Errorf("RUNTIME_BUILD_MARKER_INVALID")}
		runtime=comments[i+len(runtimeMarker):];comments=comments[:i-1]
	}
	const sourceMarker="mcphub-source-v1;sha256="
	lower=strings.ToLower(comments)
	if strings.Contains(lower,"mcphub-source-"){
		if strings.Count(lower,"mcphub-source-")!=1||strings.Count(comments,sourceMarker)!=1{return "","","",fmt.Errorf("PUBLISHER_SOURCE_EPOCH_MISMATCH: duplicate or unknown source suffix")}
		i:=strings.Index(comments,sourceMarker)
		if i==0||comments[i-1]!='\n'||!validLowerHex(comments[i+len(sourceMarker):],64,64){return "","","",fmt.Errorf("PUBLISHER_SOURCE_EPOCH_MISMATCH: source suffix grammar")}
		epoch=comments[i+len(sourceMarker):];comments=comments[:i-1]
	}
	if runtime!=""&&epoch==""{return "","","",fmt.Errorf("RUNTIME_BUILD_SOURCE_BINDING_MISSING")}
	return comments,epoch,runtime,nil
}
