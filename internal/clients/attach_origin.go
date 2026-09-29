package clients

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
)

// SelectedEntryOriginVerdict is an admission result, never a write authorization.
type SelectedEntryOriginVerdict string

const (
	SelectedEntryOriginTargetOwnedDirect SelectedEntryOriginVerdict = "target-owned-direct"
	SelectedEntryOriginTargetOwnedHTTP   SelectedEntryOriginVerdict = "target-owned-http"
	SelectedEntryOriginHigherOrMixed     SelectedEntryOriginVerdict = "higher-or-mixed"
	SelectedEntryOriginBelowOrImport     SelectedEntryOriginVerdict = "below-or-import-only"
	SelectedEntryOriginMissing           SelectedEntryOriginVerdict = "missing"
	SelectedEntryOriginUnsupported       SelectedEntryOriginVerdict = "disabled-or-unsupported-transport"
	SelectedEntryOriginMalformed         SelectedEntryOriginVerdict = "malformed-or-unreadable"
	SelectedEntryOriginUnwritable        SelectedEntryOriginVerdict = "unwritable"
	SelectedEntryOriginAliasOrAmbiguous  SelectedEntryOriginVerdict = "alias-or-ambiguous"
	SelectedEntryOriginUnsupportedClient SelectedEntryOriginVerdict = "unsupported-client"
)

// SelectedEntryOriginV1 contains point-in-time physical and effective proof.
// Raw entry and target path stay in-process; callers must redact before display.
type SelectedEntryOriginV1 struct {
	Version                      int
	Verdict                      SelectedEntryOriginVerdict
	ClientID                     string
	EntryName                    string
	Scope                        string
	SourceKind                   string
	TargetPathInternal           string
	TargetBytesSHA256            string
	EntryTokenSHA256             string
	EffectiveDirectEntryInternal *MCPEntry
}

func (v SelectedEntryOriginV1) Redacted() SelectedEntryOriginV1 {
	v.TargetPathInternal = ""
	v.EffectiveDirectEntryInternal = nil
	return v
}

// SelectedEntryOriginReader is optional. A nil/zero verdict never admits attach.
type SelectedEntryOriginReader interface {
	ReadSelectedEntryOrigin(context.Context, string) SelectedEntryOriginV1
}

func (l *lockingClient) ReadSelectedEntryOrigin(ctx context.Context, name string) SelectedEntryOriginV1 {
	reader, ok := l.Client.(SelectedEntryOriginReader)
	if !ok {
		return SelectedEntryOriginV1{Version: 1, Verdict: SelectedEntryOriginUnsupportedClient, ClientID: l.Name(), EntryName: name}
	}
	return reader.ReadSelectedEntryOrigin(ctx, name)
}

func (o *mimoCodeClient) ReadSelectedEntryOrigin(ctx context.Context, name string) SelectedEntryOriginV1 {
	result := SelectedEntryOriginV1{Version: 1, ClientID: o.Name(), EntryName: name, Scope: "known-global"}
	refuse := func(v SelectedEntryOriginVerdict) SelectedEntryOriginV1 { result.Verdict = v; return result }
	if ctx == nil || ctx.Err() != nil || name == "" || filepath.Base(o.path) != "mimocode.json" {
		return refuse(SelectedEntryOriginAliasOrAmbiguous)
	}
	info, err := os.Lstat(o.path)
	if err != nil {
		if os.IsNotExist(err) {
			return refuse(SelectedEntryOriginMissing)
		}
		return refuse(SelectedEntryOriginMalformed)
	}
	if !info.Mode().IsRegular() {
		return refuse(SelectedEntryOriginAliasOrAmbiguous)
	}
	file, err := os.OpenFile(o.path, os.O_WRONLY, 0)
	if err != nil {
		return refuse(SelectedEntryOriginUnwritable)
	}
	if err := file.Close(); err != nil {
		return refuse(SelectedEntryOriginUnwritable)
	}
	before, err := os.ReadFile(o.path)
	if err != nil {
		return refuse(SelectedEntryOriginMalformed)
	}
	ownAny, ownPresent, err := o.EntryRawSubtree(before, name)
	if err != nil {
		return refuse(SelectedEntryOriginMalformed)
	}
	if !ownPresent {
		entry, err := o.GetEntry(name)
		if err != nil {
			return refuse(SelectedEntryOriginMalformed)
		}
		if entry != nil {
			if entry.SourceBelowWriteTarget {
				return refuse(SelectedEntryOriginBelowOrImport)
			}
			return refuse(SelectedEntryOriginHigherOrMixed)
		}
		return refuse(SelectedEntryOriginMissing)
	}
	own, ok := ownAny.(map[string]any)
	if !ok {
		return refuse(SelectedEntryOriginMalformed)
	}
	if mimoCodeEntryDisabled(own) {
		return refuse(SelectedEntryOriginUnsupported)
	}
	direct := own["type"] == "local" && mimoCodeDirectCommand(own)
	httpURL, _ := own["url"].(string)
	parsedURL, urlErr := url.Parse(httpURL)
	http := own["type"] == "remote" && urlErr == nil && parsedURL.Host != "" && (parsedURL.Scheme == "http" || parsedURL.Scheme == "https")
	if !direct && !http {
		return refuse(SelectedEntryOriginUnsupported)
	}
	if alias, err := o.mimoCodeLowerLayerHardLinkedToWriteTargetDefines(name); err != nil {
		return refuse(SelectedEntryOriginMalformed)
	} else if alias {
		return refuse(SelectedEntryOriginAliasOrAmbiguous)
	}
	if src, err := o.mimoCodeManagedLayerShadows(name); err != nil {
		return refuse(SelectedEntryOriginMalformed)
	} else if src.Kind != "" {
		return refuse(SelectedEntryOriginHigherOrMixed)
	}
	// The ordinary managed shadow predicate permits enabled-only:true. Exact
	// attach origin is stricter: even that same-key contribution is mixed.
	if dir := mimoCodeManagedConfigDir(); dir != "" {
		for _, layer := range mimoCodeOverlayLayerNames {
			path := filepath.Join(dir, layer)
			defines, err := mimoCodeFileDefines(path, name)
			if err != nil {
				return refuse(SelectedEntryOriginMalformed)
			}
			if defines {
				return refuse(SelectedEntryOriginHigherOrMixed)
			}
		}
	}
	if o.inlineContent != "" {
		defines, err := mimoCodeInlineDefines(o.inlineContent, name)
		if err != nil {
			return refuse(SelectedEntryOriginMalformed)
		}
		if defines {
			return refuse(SelectedEntryOriginHigherOrMixed)
		}
	}
	seenTarget := false
	for _, path := range o.readLayerFiles() {
		if filepath.Clean(path) == filepath.Clean(o.path) {
			seenTarget = true
			continue
		}
		if !seenTarget || mimoCodePathsSamePhysical(path, o.path) {
			continue
		}
		defines, err := mimoCodeFileDefines(path, name)
		if err != nil {
			return refuse(SelectedEntryOriginMalformed)
		}
		if defines {
			return refuse(SelectedEntryOriginHigherOrMixed)
		}
	}
	if !seenTarget {
		return refuse(SelectedEntryOriginAliasOrAmbiguous)
	}
	effective, err := o.GetEntry(name)
	if err != nil {
		return refuse(SelectedEntryOriginMalformed)
	}
	if effective == nil || effective.Disabled || effective.SourceBelowWriteTarget {
		return refuse(SelectedEntryOriginAliasOrAmbiguous)
	}
	var right []byte
	if direct {
		if effective.Raw == nil {
			return refuse(SelectedEntryOriginAliasOrAmbiguous)
		}
		left, err := json.Marshal(effective.Raw)
		if err != nil {
			return refuse(SelectedEntryOriginMalformed)
		}
		right, err = json.Marshal(own)
		if err != nil || !bytes.Equal(left, right) {
			return refuse(SelectedEntryOriginHigherOrMixed)
		}
	} else if effective.Raw != nil || effective.URL != httpURL {
		return refuse(SelectedEntryOriginHigherOrMixed)
	}
	token, present, err := o.EntryPhysicalToken(before, name)
	if err != nil || !present || token == nil {
		return refuse(SelectedEntryOriginMalformed)
	}
	after, err := os.ReadFile(o.path)
	if err != nil || !bytes.Equal(before, after) || ctx.Err() != nil {
		return refuse(SelectedEntryOriginAliasOrAmbiguous)
	}
	if direct {
		copyRaw := map[string]any{}
		if err := json.Unmarshal(right, &copyRaw); err != nil {
			return refuse(SelectedEntryOriginMalformed)
		}
		result.Verdict = SelectedEntryOriginTargetOwnedDirect
		result.EffectiveDirectEntryInternal = &MCPEntry{Name: name, Raw: copyRaw}
	} else {
		result.Verdict = SelectedEntryOriginTargetOwnedHTTP
	}
	result.SourceKind = "write-target"
	result.TargetPathInternal = o.path
	result.TargetBytesSHA256 = attachOriginHash(before)
	result.EntryTokenSHA256 = attachOriginHash(token.memberBytes)
	return result
}

func mimoCodeDirectCommand(raw map[string]any) bool {
	command, ok := raw["command"].([]any)
	if !ok || len(command) == 0 {
		return false
	}
	for _, arg := range command {
		if s, ok := arg.(string); !ok || s == "" {
			return false
		}
	}
	return true
}

func attachOriginHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// SelectedEntryPhysicalTokenSHA256 uses the concrete adapter's lexical token
// owner, so receipt postimages are not reconstructed from merged JSON.
func SelectedEntryPhysicalTokenSHA256(client Client, raw []byte, name string) (string, error) {
	reader, ok := client.(interface {
		EntryPhysicalToken([]byte, string) (*entryPhysicalToken, bool, error)
	})
	if !ok {
		return "", fmt.Errorf("client %s has no physical token reader", client.Name())
	}
	token, present, err := reader.EntryPhysicalToken(raw, name)
	if err != nil {
		return "", err
	}
	if !present || token == nil {
		return "", errors.New("selected physical token absent")
	}
	return attachOriginHash(token.memberBytes), nil
}
