package clients

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
	"github.com/tailscale/hujson"
)

type entryPhysicalSyntax uint8

const (
	entryPhysicalJSONC entryPhysicalSyntax = iota + 1
	entryPhysicalTOML
)

// entryPhysicalToken is the package-private lexical compare-and-splice token
// used by direct cleanup. targetBytes is the exact removable member/table span;
// memberBytes excludes only a JSON structural separator so an absent target can
// be reinserted even when its former neighbours changed. None of these ranges or
// anchors escape package clients.
type entryPhysicalToken struct {
	syntax       entryPhysicalSyntax
	section      string
	name         string
	targetBytes  []byte
	memberBytes  []byte
	memberStart  int
	memberEnd    int
	removeStart  int
	removeEnd    int
	previousName string
	nextName     string
	nextPrefix   []byte
	wasFirst     bool
}

func extractEntryPhysicalToken(client Client, configBytes []byte, name string) (*entryPhysicalToken, bool, error) {
	mutator, ok := client.(CASEntryMutator)
	if !ok {
		return nil, false, fmt.Errorf("client %s does not expose direct-cleanup CAS", client.Name())
	}
	return mutator.EntryPhysicalToken(configBytes, name)
}

func (c *claudeCode) EntryPhysicalToken(configBytes []byte, name string) (*entryPhysicalToken, bool, error) {
	return extractJSONCEntryPhysicalToken(configBytes, claudeCodeMCPServersKey, name)
}

func (*codexCLI) EntryPhysicalToken(configBytes []byte, name string) (*entryPhysicalToken, bool, error) {
	return extractTOMLEntryPhysicalToken(configBytes, "mcp_servers", name)
}

func (c *cursorClient) EntryPhysicalToken(configBytes []byte, name string) (*entryPhysicalToken, bool, error) {
	return extractJSONCEntryPhysicalToken(configBytes, c.sectionKey(), name)
}

func (*vscodeClient) EntryPhysicalToken(configBytes []byte, name string) (*entryPhysicalToken, bool, error) {
	return extractJSONCEntryPhysicalToken(configBytes, vscodeServersKey, name)
}

func (g *geminiCLI) EntryPhysicalToken(configBytes []byte, name string) (*entryPhysicalToken, bool, error) {
	return extractJSONCEntryPhysicalToken(configBytes, g.sectionKey(), name)
}

func (q *qwenCLI) EntryPhysicalToken(configBytes []byte, name string) (*entryPhysicalToken, bool, error) {
	return extractJSONCEntryPhysicalToken(configBytes, q.sectionKey(), name)
}

func (a *antigravityClient) EntryPhysicalToken(configBytes []byte, name string) (*entryPhysicalToken, bool, error) {
	return extractJSONCEntryPhysicalToken(configBytes, a.sectionKey(), name)
}

func (*openCodeClient) EntryPhysicalToken(configBytes []byte, name string) (*entryPhysicalToken, bool, error) {
	return extractJSONCEntryPhysicalToken(configBytes, openCodeMCPKey, name)
}

func (*mimoCodeClient) EntryPhysicalToken(configBytes []byte, name string) (*entryPhysicalToken, bool, error) {
	return extractJSONCEntryPhysicalToken(configBytes, mimoCodeMCPKey, name)
}

func entryPhysicalTokensEqual(a, b *entryPhysicalToken) bool {
	return a != nil && b != nil &&
		a.syntax == b.syntax && a.section == b.section && a.name == b.name &&
		bytes.Equal(a.targetBytes, b.targetBytes)
}

func removeEntryPhysical(configBytes []byte, token *entryPhysicalToken) ([]byte, error) {
	if token == nil || token.memberStart < 0 || token.memberEnd < token.memberStart ||
		token.removeStart < 0 || token.removeEnd < token.removeStart || token.removeEnd > len(configBytes) ||
		token.memberEnd > len(configBytes) {
		return nil, fmt.Errorf("invalid physical removal range")
	}
	if !bytes.Equal(configBytes[token.memberStart:token.memberEnd], token.targetBytes) {
		return nil, fmt.Errorf("physical target range no longer matches token")
	}
	out := make([]byte, 0, len(configBytes)-len(token.targetBytes))
	out = append(out, configBytes[:token.removeStart]...)
	out = append(out, configBytes[token.removeEnd:]...)
	return out, nil
}

func restoreEntryPhysical(configBytes []byte, token *entryPhysicalToken) ([]byte, error) {
	if token == nil {
		return nil, fmt.Errorf("nil physical restore token")
	}
	switch token.syntax {
	case entryPhysicalJSONC:
		return restoreJSONCEntryPhysical(configBytes, token)
	case entryPhysicalTOML:
		return restoreTOMLEntryPhysical(configBytes, token)
	default:
		return nil, fmt.Errorf("unsupported physical restore format %d", token.syntax)
	}
}

type jsoncPhysicalMember struct {
	name          string
	memberStart   int
	memberEnd     int
	commaOffset   int
	hasCommaAfter bool
	ambiguousTail bool
}

type jsoncPhysicalSection struct {
	openOffset int
	extraStart int
	members    []jsoncPhysicalMember
}

func parseJSONCPhysicalSection(configBytes []byte, section string) (*jsoncPhysicalSection, bool, error) {
	root, err := hujson.Parse(configBytes)
	if err != nil {
		return nil, false, fmt.Errorf("parse jsonc physical config: %w", err)
	}
	rootObject, ok := root.Value.(*hujson.Object)
	if !ok {
		return nil, false, fmt.Errorf("jsonc physical config root is not an object")
	}
	var sectionValue *hujson.Value
	for i := range rootObject.Members {
		member := &rootObject.Members[i]
		key, keyErr := jsonCStringAt(configBytes, member.Name.StartOffset, member.Name.EndOffset)
		if keyErr != nil {
			return nil, false, keyErr
		}
		if key != section {
			continue
		}
		if sectionValue != nil {
			return nil, false, fmt.Errorf("jsonc physical section %q is duplicated", section)
		}
		sectionValue = &member.Value
	}
	if sectionValue == nil {
		return nil, false, nil
	}
	object, ok := sectionValue.Value.(*hujson.Object)
	if !ok {
		return nil, false, fmt.Errorf("jsonc physical section %q is not an object", section)
	}
	view := &jsoncPhysicalSection{
		openOffset: sectionValue.StartOffset,
		extraStart: sectionValue.EndOffset - 1 - len(object.AfterExtra),
		members:    make([]jsoncPhysicalMember, 0, len(object.Members)),
	}
	seen := make(map[string]struct{}, len(object.Members))
	for i := range object.Members {
		member := &object.Members[i]
		key, keyErr := jsonCStringAt(configBytes, member.Name.StartOffset, member.Name.EndOffset)
		if keyErr != nil {
			return nil, false, keyErr
		}
		if _, duplicate := seen[key]; duplicate {
			return nil, false, fmt.Errorf("jsonc physical entry %q is duplicated", key)
		}
		seen[key] = struct{}{}
		start := member.Name.StartOffset - len(member.Name.BeforeExtra)
		end := member.Value.EndOffset + len(member.Value.AfterExtra)
		hasComma := i < len(object.Members)-1 || member.Value.AfterExtra != nil
		commaOffset := -1
		if hasComma {
			commaOffset = end
			if commaOffset >= len(configBytes) || configBytes[commaOffset] != ',' {
				return nil, false, fmt.Errorf("jsonc physical entry %q has an invalid delimiter", key)
			}
		}
		ambiguousTail := false
		if i == len(object.Members)-1 {
			extra := configBytes[view.extraStart : sectionValue.EndOffset-1]
			if hasComma {
				ambiguousTail = len(bytes.TrimSpace(extra)) > 0
			} else if newline := bytes.IndexByte(extra, '\n'); newline >= 0 {
				sameLine := extra[:newline+1]
				if len(bytes.TrimSpace(sameLine)) > 0 {
					end += len(sameLine)
				}
				ambiguousTail = len(bytes.TrimSpace(extra[newline+1:])) > 0
			} else if len(bytes.TrimSpace(extra)) > 0 {
				end += len(extra)
			}
		}
		view.members = append(view.members, jsoncPhysicalMember{
			name: key, memberStart: start, memberEnd: end,
			commaOffset: commaOffset, hasCommaAfter: hasComma, ambiguousTail: ambiguousTail,
		})
	}
	return view, true, nil
}

func jsonCStringAt(configBytes []byte, start, end int) (string, error) {
	if start < 0 || end < start || end > len(configBytes) {
		return "", fmt.Errorf("invalid jsonc string range")
	}
	var value string
	if err := json.Unmarshal(configBytes[start:end], &value); err != nil {
		return "", fmt.Errorf("decode jsonc object key: %w", err)
	}
	return value, nil
}

func extractJSONCEntryPhysicalToken(configBytes []byte, section, name string) (*entryPhysicalToken, bool, error) {
	view, sectionPresent, err := parseJSONCPhysicalSection(configBytes, section)
	if err != nil || !sectionPresent {
		return nil, false, err
	}
	for i, member := range view.members {
		if member.name != name {
			continue
		}
		if member.ambiguousTail {
			return nil, false, fmt.Errorf("jsonc physical entry %q has ambiguous trailing comments", name)
		}
		memberBytes := append([]byte(nil), configBytes[member.memberStart:member.memberEnd]...)
		removeStart, removeEnd := member.memberStart, member.memberEnd
		if member.hasCommaAfter {
			removeEnd = member.commaOffset + 1
		} else if i > 0 {
			previous := view.members[i-1]
			if !previous.hasCommaAfter {
				return nil, false, fmt.Errorf("jsonc physical entry %q has no preceding delimiter", name)
			}
			removeStart = previous.commaOffset
		}
		token := &entryPhysicalToken{
			syntax:      entryPhysicalJSONC,
			section:     section,
			name:        name,
			targetBytes: memberBytes,
			memberBytes: memberBytes,
			memberStart: member.memberStart,
			memberEnd:   member.memberEnd,
			removeStart: removeStart,
			removeEnd:   removeEnd,
			wasFirst:    i == 0,
		}
		if i > 0 {
			token.previousName = view.members[i-1].name
		}
		if i+1 < len(view.members) {
			token.nextName = view.members[i+1].name
		}
		return token, true, nil
	}
	return nil, false, nil
}

func restoreJSONCEntryPhysical(configBytes []byte, token *entryPhysicalToken) ([]byte, error) {
	view, sectionPresent, err := parseJSONCPhysicalSection(configBytes, token.section)
	if err != nil {
		return nil, err
	}
	if !sectionPresent {
		return nil, fmt.Errorf("jsonc physical section %q is absent", token.section)
	}
	for _, member := range view.members {
		if member.name == token.name {
			return nil, fmt.Errorf("jsonc physical target %q is already present", token.name)
		}
	}
	insertAt := view.extraStart
	var insert []byte
	find := func(name string) int {
		for i := range view.members {
			if view.members[i].name == name {
				return i
			}
		}
		return -1
	}
	switch {
	case len(view.members) == 0:
		insertAt = view.openOffset + 1
		insert = append(insert, token.memberBytes...)
	case token.nextName != "" && find(token.nextName) >= 0:
		next := view.members[find(token.nextName)]
		insertAt = next.memberStart
		insert = append(insert, token.memberBytes...)
		insert = append(insert, ',')
	case token.previousName != "" && find(token.previousName) >= 0:
		previousIndex := find(token.previousName)
		previous := view.members[previousIndex]
		if previous.hasCommaAfter {
			insertAt = previous.commaOffset + 1
			insert = append(insert, token.memberBytes...)
			if previousIndex < len(view.members)-1 {
				insert = append(insert, ',')
			}
		} else {
			insertAt = view.extraStart
			insert = append(insert, ',')
			insert = append(insert, token.memberBytes...)
		}
	case token.wasFirst:
		insertAt = view.members[0].memberStart
		insert = append(insert, token.memberBytes...)
		insert = append(insert, ',')
	default:
		last := view.members[len(view.members)-1]
		if last.hasCommaAfter {
			insertAt = last.commaOffset + 1
			insert = append(insert, token.memberBytes...)
		} else {
			insertAt = view.extraStart
			insert = append(insert, ',')
			insert = append(insert, token.memberBytes...)
		}
	}
	out := make([]byte, 0, len(configBytes)+len(insert))
	out = append(out, configBytes[:insertAt]...)
	out = append(out, insert...)
	out = append(out, configBytes[insertAt:]...)
	restored, present, extractErr := extractJSONCEntryPhysicalToken(out, token.section, token.name)
	if extractErr != nil {
		return nil, extractErr
	}
	if !present || !entryPhysicalTokensEqual(restored, token) {
		return nil, fmt.Errorf("jsonc physical target %q was not restored byte-exactly", token.name)
	}
	return out, nil
}

type tomlPhysicalHeader struct {
	kind          unstable.Kind
	key           string
	lineStart     int
	leadingStart  int
	expressionEnd int
}

func parseTOMLPhysicalHeaders(configBytes []byte) ([]tomlPhysicalHeader, error) {
	var parser unstable.Parser
	parser.KeepComments = true
	parser.Reset(configBytes)
	var headers []tomlPhysicalHeader
	for parser.NextExpression() {
		expression := parser.Expression()
		expressionEnd := int(expression.Raw.Offset + expression.Raw.Length)
		if expression.Kind != unstable.Table && expression.Kind != unstable.ArrayTable {
			if expression.Kind == unstable.KeyValue && len(headers) > 0 {
				headers[len(headers)-1].expressionEnd = expressionEnd
			}
			continue
		}
		var parts []string
		firstKeyOffset := -1
		keys := expression.Key()
		for keys.Next() {
			keyNode := keys.Node()
			if firstKeyOffset < 0 {
				firstKeyOffset = int(keyNode.Raw.Offset)
			}
			parts = append(parts, string(keyNode.Data))
		}
		if firstKeyOffset < 0 {
			return nil, fmt.Errorf("toml physical table has no key")
		}
		lineStart := bytes.LastIndexByte(configBytes[:firstKeyOffset], '\n') + 1
		headers = append(headers, tomlPhysicalHeader{
			kind: expression.Kind, key: strings.Join(parts, "\x00"),
			lineStart: lineStart, leadingStart: lineStart, expressionEnd: expressionEnd,
		})
	}
	if err := parser.Error(); err != nil {
		return nil, fmt.Errorf("parse toml physical config: %w", err)
	}
	for i := 1; i < len(headers); i++ {
		start := headers[i].lineStart
		floor := headers[i-1].lineStart
		for start > floor {
			previousEnd := start
			previousStart := bytes.LastIndexByte(configBytes[:max(0, previousEnd-1)], '\n') + 1
			line := strings.TrimSpace(strings.TrimSuffix(string(configBytes[previousStart:previousEnd]), "\r\n"))
			if line != "" && !strings.HasPrefix(line, "#") {
				break
			}
			if previousStart <= floor {
				break
			}
			start = previousStart
		}
		headers[i].leadingStart = start
	}
	return headers, nil
}

func tomlHasStandaloneFooter(configBytes []byte, lastExpressionEnd int) bool {
	if lastExpressionEnd < 0 || lastExpressionEnd >= len(configBytes) {
		return false
	}
	lineEnd := bytes.IndexByte(configBytes[lastExpressionEnd:], '\n')
	if lineEnd < 0 {
		return false
	}
	for offset := lastExpressionEnd + lineEnd + 1; offset < len(configBytes); {
		next := bytes.IndexByte(configBytes[offset:], '\n')
		end := len(configBytes)
		if next >= 0 {
			end = offset + next
		}
		line := bytes.TrimLeft(configBytes[offset:end], " \t\r")
		if len(line) > 0 && line[0] == '#' {
			return true
		}
		if next < 0 {
			break
		}
		offset = end + 1
	}
	return false
}

func extractTOMLEntryPhysicalToken(configBytes []byte, section, name string) (*entryPhysicalToken, bool, error) {
	var parsed map[string]any
	if err := toml.Unmarshal(configBytes, &parsed); err != nil {
		return nil, false, fmt.Errorf("parse toml physical config: %w", err)
	}
	servers, _ := parsed[section].(map[string]any)
	_, semanticPresent := servers[name]
	headers, err := parseTOMLPhysicalHeaders(configBytes)
	if err != nil {
		return nil, false, err
	}
	targetKey := section + "\x00" + name
	targetIndex := -1
	for i, header := range headers {
		if header.key != targetKey {
			continue
		}
		if header.kind != unstable.Table || targetIndex >= 0 {
			return nil, false, fmt.Errorf("toml physical entry %q is ambiguous", name)
		}
		targetIndex = i
	}
	if targetIndex < 0 {
		for _, header := range headers {
			if strings.HasPrefix(header.key, targetKey+"\x00") {
				return nil, false, fmt.Errorf("toml physical entry %q has descendant tables without an owning table", name)
			}
		}
		if semanticPresent {
			return nil, false, fmt.Errorf("toml physical entry %q is not represented by one named table block", name)
		}
		return nil, false, nil
	}
	if !semanticPresent {
		return nil, false, fmt.Errorf("toml physical table %q has no parsed entry", name)
	}
	start := headers[targetIndex].leadingStart
	end := len(configBytes)
	boundary := len(headers)
	for i := targetIndex + 1; i < len(headers); i++ {
		if strings.HasPrefix(headers[i].key, targetKey+"\x00") {
			continue
		}
		end = headers[i].leadingStart
		boundary = i
		break
	}
	for i := boundary + 1; i < len(headers); i++ {
		if strings.HasPrefix(headers[i].key, targetKey+"\x00") {
			return nil, false, fmt.Errorf("toml physical entry %q has a non-contiguous table block", name)
		}
	}
	if boundary == len(headers) {
		lastExpressionEnd := headers[targetIndex].expressionEnd
		for i := targetIndex + 1; i < boundary; i++ {
			if headers[i].expressionEnd > lastExpressionEnd {
				lastExpressionEnd = headers[i].expressionEnd
			}
		}
		if tomlHasStandaloneFooter(configBytes, lastExpressionEnd) {
			return nil, false, fmt.Errorf("toml physical entry %q has ambiguous trailing footer comments", name)
		}
	}
	token := &entryPhysicalToken{
		syntax: entryPhysicalTOML, section: section, name: name,
		targetBytes: append([]byte(nil), configBytes[start:end]...),
		memberBytes: append([]byte(nil), configBytes[start:end]...),
		memberStart: start, memberEnd: end,
		removeStart: start, removeEnd: end, wasFirst: targetIndex == 0,
	}
	if targetIndex > 0 {
		token.previousName = headers[targetIndex-1].key
	}
	if boundary < len(headers) {
		token.nextName = headers[boundary].key
		token.nextPrefix = append([]byte(nil), configBytes[headers[boundary].leadingStart:headers[boundary].lineStart]...)
	}
	return token, true, nil
}

func restoreTOMLEntryPhysical(configBytes []byte, token *entryPhysicalToken) ([]byte, error) {
	headers, err := parseTOMLPhysicalHeaders(configBytes)
	if err != nil {
		return nil, err
	}
	targetKey := token.section + "\x00" + token.name
	for _, header := range headers {
		if header.key == targetKey || strings.HasPrefix(header.key, targetKey+"\x00") {
			return nil, fmt.Errorf("toml physical target %q is already present or ambiguous", token.name)
		}
	}
	insertAt := len(configBytes)
	find := func(key string) int {
		for i := range headers {
			if headers[i].key == key {
				return i
			}
		}
		return -1
	}
	if token.nextName != "" {
		if next := find(token.nextName); next >= 0 {
			insertAt = headers[next].leadingStart
			if prefix := token.nextPrefix; len(prefix) > 0 && headers[next].lineStart >= len(prefix) &&
				bytes.Equal(configBytes[headers[next].lineStart-len(prefix):headers[next].lineStart], prefix) {
				insertAt = headers[next].lineStart - len(prefix)
			}
		}
	}
	if insertAt == len(configBytes) && token.wasFirst && len(headers) > 0 {
		insertAt = headers[0].leadingStart
	}
	if insertAt == len(configBytes) && token.previousName != "" {
		if previous := find(token.previousName); previous >= 0 && previous+1 < len(headers) {
			insertAt = headers[previous+1].leadingStart
		}
	}
	insert := make([]byte, 0, len(token.targetBytes)+2)
	if insertAt > 0 && configBytes[insertAt-1] != '\n' && len(token.targetBytes) > 0 && token.targetBytes[0] != '\n' {
		insert = append(insert, '\n')
	}
	insert = append(insert, token.targetBytes...)
	if insertAt < len(configBytes) && len(insert) > 0 && insert[len(insert)-1] != '\n' {
		insert = append(insert, '\n')
	}
	out := make([]byte, 0, len(configBytes)+len(insert))
	out = append(out, configBytes[:insertAt]...)
	out = append(out, insert...)
	out = append(out, configBytes[insertAt:]...)
	restored, present, extractErr := extractTOMLEntryPhysicalToken(out, token.section, token.name)
	if extractErr != nil {
		return nil, extractErr
	}
	if !present || !entryPhysicalTokensEqual(restored, token) {
		return nil, fmt.Errorf("toml physical target %q was not restored byte-exactly", token.name)
	}
	return out, nil
}

// EntryBytesChecker is the read-only capability of confirming whether a named MCP
// server entry is PHYSICALLY present in a given config-file byte blob — WITHOUT any
// disk read or mutation. adopt-provenance capture uses it to verify the EXACT
// snapshotted write-target bytes actually contain the adopted entry before pinning
// them, so a TOCTOU where the entry is deleted (and possibly re-created) mid-capture
// cannot pin a snapshot that a later de-adopt would restore as an ABSENCE (codex bot
// PR #528 r4 finding 3).
//
// It is implemented by the adopt-supported client adapters (single-file clients pin
// their own bytes; MiMoCode pins its write target) and forwarded by the
// lockingClient wrapper. "Present in bytes" mirrors each adapter's own GetEntry
// parse + section lookup. NOTE (MiMoCode): "present in the write-target bytes" is
// PHYSICAL presence in the write target (mimocode.json's "mcp"); an entry that
// resolves only from a LOWER/import layer never reaches this check — it is routed to
// present-merged-lower by SourceBelowWriteTarget before any snapshot.
type EntryBytesChecker interface {
	EntryPresentInBytes(configBytes []byte, name string) (bool, error)
}

// Compile-time proof that every adopt-supported adapter (and the wrapper every
// AllClients() adapter is wrapped in) satisfies the capability. jsonMCPClient covers
// its embedders (cursor / gemini-cli / qwen-cli / antigravity / relay clients).
var (
	_ EntryBytesChecker = (*jsonMCPClient)(nil)
	_ EntryBytesChecker = (*claudeCode)(nil)
	_ EntryBytesChecker = (*vscodeClient)(nil)
	_ EntryBytesChecker = (*openCodeClient)(nil)
	_ EntryBytesChecker = (*codexCLI)(nil)
	_ EntryBytesChecker = (*mimoCodeClient)(nil)
	_ EntryBytesChecker = (*lockingClient)(nil)
)

// jsoncEntryPresentInBytes parses jsonc config bytes and reports whether
// <section>[name] is a JSON object — the same parse + lookup the JSON-family
// adapters' GetEntry uses. Pure/read-only (no disk access).
func jsoncEntryPresentInBytes(configBytes []byte, section, name string) (bool, error) {
	_, present, err := jsoncEntryRawSubtree(configBytes, section, name)
	return present, err
}

// jsoncEntryRawSubtree is the single JSONC section extractor used by both the
// physical-presence check and Phase-4 classification. It returns the parsed
// on-disk entry value without projecting it onto the intentionally lean
// MCPEntry shape.
func jsoncEntryRawSubtree(configBytes []byte, section, name string) (any, bool, error) {
	m, err := parseJSONCBytes(configBytes)
	if err != nil {
		return nil, false, err
	}
	servers, _ := m[section].(map[string]any)
	if servers == nil {
		return nil, false, nil
	}
	subtree, ok := servers[name].(map[string]any)
	if !ok {
		return nil, false, nil
	}
	return subtree, true, nil
}

// tomlEntryPresentInBytes is the TOML analogue (codex-cli's readTOML parse).
func tomlEntryPresentInBytes(configBytes []byte, section, name string) (bool, error) {
	_, present, err := tomlEntryRawSubtree(configBytes, section, name)
	return present, err
}

// tomlEntryRawSubtree is the TOML analogue of jsoncEntryRawSubtree.
func tomlEntryRawSubtree(configBytes []byte, section, name string) (any, bool, error) {
	var m map[string]any
	if err := toml.Unmarshal(configBytes, &m); err != nil {
		return nil, false, fmt.Errorf("parse toml config bytes: %w", err)
	}
	servers, _ := m[section].(map[string]any)
	if servers == nil {
		return nil, false, nil
	}
	subtree, ok := servers[name].(map[string]any)
	if !ok {
		return nil, false, nil
	}
	return subtree, true, nil
}

// ---- adapter implementations (methods may live in any file of package clients) ----

// jsonMCPClient covers every embedder (cursor, gemini-cli, qwen-cli, antigravity,
// and the rest) via its sectionKey.
func (j *jsonMCPClient) EntryPresentInBytes(configBytes []byte, name string) (bool, error) {
	return jsoncEntryPresentInBytes(configBytes, j.sectionKey(), name)
}

func (c *claudeCode) EntryPresentInBytes(configBytes []byte, name string) (bool, error) {
	return jsoncEntryPresentInBytes(configBytes, claudeCodeMCPServersKey, name)
}

func (v *vscodeClient) EntryPresentInBytes(configBytes []byte, name string) (bool, error) {
	return jsoncEntryPresentInBytes(configBytes, vscodeServersKey, name)
}

func (o *openCodeClient) EntryPresentInBytes(configBytes []byte, name string) (bool, error) {
	return jsoncEntryPresentInBytes(configBytes, openCodeMCPKey, name)
}

func (c *codexCLI) EntryPresentInBytes(configBytes []byte, name string) (bool, error) {
	return tomlEntryPresentInBytes(configBytes, "mcp_servers", name)
}

// mimoCodeClient checks PHYSICAL presence in the write-target bytes (mimocode.json's
// "mcp"). An entry that resolves only from a lower/import layer is routed to
// present-merged-lower before this check (SourceBelowWriteTarget); an entry defined
// only in a layer ABOVE the write target (mimocode.jsonc etc.) is not physically in
// the write-target bytes and so reads absent here (conservative fail-closed at the
// capture caller — see the r4 report flag).
func (o *mimoCodeClient) EntryPresentInBytes(configBytes []byte, name string) (bool, error) {
	return jsoncEntryPresentInBytes(configBytes, mimoCodeMCPKey, name)
}

// lockingClient forwards to the wrapped adapter (read-only, no config lock needed).
// Every AllClients() adapter is lockingClient-wrapped, so this is how capture reaches
// the concrete adapter's EntryPresentInBytes. A wrapped client that does not
// implement the capability (a non-adopt client, never reached by capture) errors.
func (l *lockingClient) EntryPresentInBytes(configBytes []byte, name string) (bool, error) {
	if c, ok := l.Client.(EntryBytesChecker); ok {
		return c.EntryPresentInBytes(configBytes, name)
	}
	return false, fmt.Errorf("client %q does not support byte-level entry validation", l.Name())
}
