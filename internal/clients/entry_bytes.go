package clients

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
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
	return extractTOMLEntryPhysicalTokenWithOwnedFooter(configBytes, section, name, false)
}

// Only the inverse owner may admit a footer created by its own byte transform;
// it must still compare the complete returned span with its private applied
// preimage before writing. All unproven readers retain strict ambiguity refusal.
func extractTOMLEntryPhysicalTokenWithOwnedFooter(configBytes []byte, section, name string, ownedFooter bool) (*entryPhysicalToken, bool, error) {
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
		if !ownedFooter && tomlHasStandaloneFooter(configBytes, lastExpressionEnd) {
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

// replaceCodexTransportBytes changes only the adapter-owned transport fields
// and, for relocation, the selected key in its contiguous named-table block.
// Unknown fields are never interpreted or re-encoded.
func replaceCodexTransportBytes(configBytes []byte, source, target string, entry MCPEntry) ([]byte, error) {
	return replaceCodexTransportBytesForKind(configBytes, source, target, entry, CodexTransportHTTP)
}
func replaceCodexTransportBytesForKind(configBytes []byte, source, target string, entry MCPEntry, kind CodexTransport) ([]byte, error) {
	token, present, err := extractTOMLEntryPhysicalToken(configBytes, "mcp_servers", source)
	if err != nil || !present {
		return nil, fmt.Errorf("%w: selected transport table is not byte-editable: %v", ErrCodexLayerParseFailed, err)
	}
	document, err := decodeCodexTOMLBytes(configBytes)
	if err != nil {
		return nil, err
	}
	servers := document["mcp_servers"].(map[string]any)
	raw := servers[source].(map[string]any)
	expectedEntry := codexHTTPReplacement(raw, entry)
	transportFields := codexHTTPTransportFields(entry)
	if kind == CodexTransportStdio {
		expectedEntry = codexSharedStdioReplacement(raw, entry)
		transportFields = codexSharedStdioTransportFields(entry)
	}
	if source == target && reflect.DeepEqual(raw, expectedEntry) {
		return append([]byte(nil), configBytes...), nil
	}
	if source != target {
		if _, occupied := servers[target]; occupied {
			return nil, fmt.Errorf("%w: target table already exists", ErrCodexTargetNameConflict)
		}
		delete(servers, source)
	}
	servers[target] = expectedEntry

	var transport bytes.Buffer
	if err := toml.NewEncoder(&transport).SetTablesInline(true).Encode(transportFields); err != nil {
		return nil, err
	}
	var keyEncoding bytes.Buffer
	if err := toml.NewEncoder(&keyEncoding).Encode(map[string]any{target: true}); err != nil {
		return nil, err
	}
	var keyParser unstable.Parser
	keyParser.Reset(keyEncoding.Bytes())
	if !keyParser.NextExpression() {
		return nil, fmt.Errorf("%w: target key encoding failed", ErrCodexLayerParseFailed)
	}
	keyIterator := keyParser.Expression().Key()
	if !keyIterator.Next() {
		return nil, fmt.Errorf("%w: target key encoding failed", ErrCodexLayerParseFailed)
	}
	targetKey := append([]byte(nil), keyParser.Raw(keyIterator.Node().Raw)...)

	type edit struct {
		start, end int
		value      []byte
	}
	var edits []edit
	block := token.memberBytes
	var parser unstable.Parser
	parser.KeepComments = true
	parser.Reset(block)
	var table []string
	for parser.NextExpression() {
		expression := parser.Expression()
		if expression.Kind == unstable.Table || expression.Kind == unstable.ArrayTable {
			var keys []*unstable.Node
			table = nil
			iterator := expression.Key()
			for iterator.Next() {
				node := iterator.Node()
				keys = append(keys, node)
				table = append(table, string(node.Data))
			}
			if len(keys) < 2 || table[0] != "mcp_servers" || table[1] != source {
				return nil, fmt.Errorf("%w: selected table block changed", ErrCodexLayerParseFailed)
			}
			lineStart := bytes.LastIndexByte(block[:keys[0].Raw.Offset], '\n') + 1
			if len(table) > 2 && codexTransportField(table[2]) {
				start := lineStart + bytes.IndexByte(block[lineStart:int(keys[0].Raw.Offset)], '[')
				end := int(keys[len(keys)-1].Raw.Offset + keys[len(keys)-1].Raw.Length)
				end += bytes.IndexByte(block[end:], ']') + 1
				if expression.Kind == unstable.ArrayTable {
					end++
				}
				edits = append(edits, edit{start: start, end: end})
				continue
			}
			if source != target {
				node := keys[1]
				edits = append(edits, edit{start: int(node.Raw.Offset), end: int(node.Raw.Offset + node.Raw.Length), value: targetKey})
			}
			if len(table) == 2 {
				lineEnd := bytes.IndexByte(block[lineStart:], '\n')
				insertAt := len(block)
				value := transport.Bytes()
				if lineEnd >= 0 {
					insertAt = lineStart + lineEnd + 1
					if insertAt >= 2 && block[insertAt-2] == '\r' {
						value = bytes.ReplaceAll(value, []byte("\n"), []byte("\r\n"))
					}
				} else {
					value = append([]byte{'\n'}, value...)
				}
				edits = append(edits, edit{start: insertAt, end: insertAt, value: value})
			}
			continue
		}
		if expression.Kind != unstable.KeyValue {
			continue
		}
		remove := len(table) > 2 && codexTransportField(table[2])
		if len(table) == 2 {
			keys := expression.Key()
			remove = keys.Next() && codexTransportField(string(keys.Node().Data))
		}
		if remove {
			start := int(expression.Raw.Offset)
			end := start + int(expression.Raw.Length)
			var comments []byte
			var retainComments func(*unstable.Node)
			retainComments = func(node *unstable.Node) {
				for ; node != nil; node = node.Next() {
					if node.Kind == unstable.Comment && int(node.Raw.Offset) >= start && int(node.Raw.Offset+node.Raw.Length) <= end {
						comments = append(comments, parser.Raw(node.Raw)...)
						comments = append(comments, '\n')
					}
					retainComments(node.Child())
				}
			}
			retainComments(expression)
			edits = append(edits, edit{start: start, end: end, value: comments})
		}
	}
	if err := parser.Error(); err != nil {
		return nil, fmt.Errorf("%w: parse selected transport block: %v", ErrCodexLayerParseFailed, err)
	}
	sort.SliceStable(edits, func(i, j int) bool {
		if edits[i].start == edits[j].start {
			return edits[i].end < edits[j].end
		}
		return edits[i].start < edits[j].start
	})
	var changed []byte
	offset := 0
	for _, patch := range edits {
		if patch.start < offset || patch.end < patch.start || patch.end > len(block) {
			return nil, fmt.Errorf("%w: overlapping transport expression spans", ErrCodexLayerParseFailed)
		}
		changed = append(changed, block[offset:patch.start]...)
		changed = append(changed, patch.value...)
		offset = patch.end
	}
	changed = append(changed, block[offset:]...)
	out := append([]byte(nil), configBytes[:token.memberStart]...)
	out = append(out, changed...)
	out = append(out, configBytes[token.memberEnd:]...)
	readback, err := decodeCodexTOMLBytes(out)
	if err != nil || !reflect.DeepEqual(readback, document) {
		return nil, fmt.Errorf("%w: transport byte edit differs from expected settings", ErrCodexLayerParseFailed)
	}
	return out, nil
}

// Attachment insertion and inverse share this physical TOML owner. Only the
// selected boolean value and newly encoded tables can change.
func encodeCodexAttachmentTable(name string, fields map[string]any) ([]byte, error) {
	var output bytes.Buffer
	if err := toml.NewEncoder(&output).Encode(map[string]any{"mcp_servers": map[string]any{name: fields}}); err != nil {
		return nil, err
	}
	encoded := output.Bytes()
	token, present, err := extractTOMLEntryPhysicalToken(encoded, "mcp_servers", name)
	if err != nil || !present {
		return nil, ErrProjectClientInvalid
	}
	// The encoder may emit an empty parent table; only the owned child table is inserted.
	return append([]byte(nil), token.memberBytes...), nil
}

func deriveCodexAttachmentOriginalToken(before []byte, logical, managed, operation string) ([]byte, error) {
	doc, err := decodeCodexTOMLBytes(before)
	if err != nil {
		return nil, err
	}
	servers, _ := doc["mcp_servers"].(map[string]any)
	if _, present := servers[managed]; present {
		return nil, ErrProjectClientEntryConflict
	}
	raw, present := servers[logical]
	switch operation {
	case "create-absent":
		if present {
			return nil, ErrProjectClientEntryConflict
		}
		return nil, nil
	case "replace-enable-override":
		fields, ok := raw.(map[string]any)
		enabled, isBool := fields["enabled"].(bool)
		if !present || !ok || len(fields) != 1 || !isBool || !enabled {
			return nil, ErrProjectClientEntryConflict
		}
		token, found, err := extractTOMLEntryPhysicalToken(before, "mcp_servers", logical)
		if err != nil || !found {
			return nil, ErrProjectClientEntryConflict
		}
		return append([]byte(nil), token.memberBytes...), nil
	default:
		return nil, ErrProjectClientInvalid
	}
}

func createCodexProjectAttachmentBytes(before []byte, logical, managed string, aliasFields map[string]any, operation string) ([]byte, []byte, []byte, []byte, error) {
	original, err := deriveCodexAttachmentOriginalToken(before, logical, managed, operation)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	out := append([]byte(nil), before...)
	if operation == "replace-enable-override" {
		token, _, _ := extractTOMLEntryPhysicalToken(before, "mcp_servers", logical)
		var parser unstable.Parser
		parser.KeepComments = true
		parser.Reset(original)
		start, end, found := 0, 0, false
		for parser.NextExpression() {
			node := parser.Expression()
			if node.Kind != unstable.KeyValue {
				continue
			}
			key := node.Key()
			if !key.Next() || string(key.Node().Data) != "enabled" || key.Next() {
				return nil, nil, nil, nil, ErrProjectClientEntryConflict
			}
			value := node.Value()
			if found || string(parser.Raw(value.Raw)) != "true" {
				return nil, nil, nil, nil, ErrProjectClientEntryConflict
			}
			start, end, found = int(value.Raw.Offset), int(value.Raw.Offset+value.Raw.Length), true
		}
		if parser.Error() != nil || !found {
			return nil, nil, nil, nil, ErrProjectClientEntryConflict
		}
		start += token.memberStart
		end += token.memberStart
		out = append(append(append([]byte(nil), before[:start]...), []byte("false")...), before[end:]...)
	}
	alias, err := encodeCodexAttachmentTable(managed, aliasFields)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	separator := []byte("\n\n")
	if len(out) == 0 || out[len(out)-1] == '\n' {
		separator = []byte("\n")
	}
	var appended []byte
	if operation == "create-absent" {
		mask, err := encodeCodexAttachmentTable(logical, map[string]any{"enabled": false})
		if err != nil {
			return nil, nil, nil, nil, err
		}
		appended = append(appended, separator...)
		appended = append(appended, mask...)
		appended = append(appended, '\n')
		appended = append(appended, alias...)
	} else {
		appended = append(appended, separator...)
		appended = append(appended, alias...)
	}
	out = append(out, appended...)
	parsed, err := decodeCodexTOMLBytes(out)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	expected, err := decodeCodexTOMLBytes(before)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	servers, ok := expected["mcp_servers"].(map[string]any)
	if !ok {
		servers = map[string]any{}
		expected["mcp_servers"] = servers
	}
	servers[logical] = map[string]any{"enabled": false}
	servers[managed] = cloneCodexRawEntry(aliasFields)
	if !reflect.DeepEqual(parsed, expected) {
		return nil, nil, nil, nil, ErrProjectClientInvalid
	}
	mask, mp, err := extractTOMLEntryPhysicalToken(out, "mcp_servers", logical)
	if err != nil || !mp {
		return nil, nil, nil, nil, ErrProjectClientInvalid
	}
	aliasToken, ap, err := extractTOMLEntryPhysicalToken(out, "mcp_servers", managed)
	if err != nil || !ap {
		return nil, nil, nil, nil, ErrProjectClientInvalid
	}
	// appended is the exact insertion suffix; inverse will remove its generated
	// separators only while the owned tables remain byte-identical.
	return out, append([]byte(nil), mask.memberBytes...), append([]byte(nil), aliasToken.memberBytes...), append([]byte(nil), appended...), nil
}

func restoreCodexProjectAttachmentBytes(current []byte, logical, managed string, ownedMask, ownedAlias, appended, original []byte, operation string) ([]byte, error) {
	mask, mp, err := extractTOMLEntryPhysicalToken(current, "mcp_servers", logical)
	if err != nil || !mp || !bytes.Equal(mask.memberBytes, ownedMask) {
		return nil, ErrProjectClientRollbackIncomplete
	}
	alias, ap, err := extractTOMLEntryPhysicalToken(current, "mcp_servers", managed)
	if err != nil || !ap || !bytes.Equal(alias.memberBytes, ownedAlias) {
		return nil, ErrProjectClientRollbackIncomplete
	}
	// Locate the uniquely owned inserted suffix, not a coincidental table fragment.
	pos := bytes.Index(current, appended)
	if pos < 0 || bytes.Index(current[pos+len(appended):], appended) >= 0 {
		return nil, ErrProjectClientRollbackIncomplete
	}
	out := append(append([]byte(nil), current[:pos]...), current[pos+len(appended):]...)
	if operation == "replace-enable-override" {
		owned, present, err := extractTOMLEntryPhysicalToken(out, "mcp_servers", logical)
		if err != nil || !present || !bytes.Equal(owned.memberBytes, ownedMask) {
			return nil, ErrProjectClientRollbackIncomplete
		}
		out = append(append(append([]byte(nil), out[:owned.memberStart]...), original...), out[owned.memberEnd:]...)
	} else if operation != "create-absent" {
		return nil, ErrProjectClientRollbackIncomplete
	}
	return out, nil
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

func codexSharedStdioTransportFields(entry MCPEntry) map[string]any {
	return map[string]any{"command": "mcphub", "args": []any{"relay", "--url", entry.RelayURL}}
}
func codexSharedStdioReplacement(source map[string]any, entry MCPEntry) map[string]any {
	fields := cloneCodexRawEntry(source)
	for key := range fields {
		if codexTransportField(key) {
			delete(fields, key)
		}
	}
	for key, value := range codexSharedStdioTransportFields(entry) {
		fields[key] = value
	}
	return fields
}

// Preparation uses the same original physical boundary/footer/editor as HTTP.
// It performs no IO and returns independent exact tokens before publication.
func PrepareCodexSharedStdioTransportTokens(originalGlobal []byte, logicalEntryName string, target MCPEntry) (beforeToken, appliedToken []byte, err error) {
	if err = validateCodexSharedStdioTarget(logicalEntryName, target); err != nil {
		return nil, nil, err
	}
	before, present, err := extractTOMLEntryPhysicalToken(originalGlobal, "mcp_servers", logicalEntryName)
	if err != nil || !present {
		return nil, nil, errors.Join(ErrCodexSharedStdioReplayEvidenceInvalid, err)
	}
	rendered, err := replaceCodexTransportBytesForKind(originalGlobal, logicalEntryName, logicalEntryName, target, CodexTransportStdio)
	if err != nil {
		return nil, nil, err
	}
	applied, present, err := extractTOMLEntryPhysicalTokenWithOwnedFooter(rendered, "mcp_servers", logicalEntryName, true)
	if err != nil || !present {
		return nil, nil, errors.Join(ErrCodexSharedStdioReplayEvidenceInvalid, err)
	}
	return bytes.Clone(before.memberBytes), bytes.Clone(applied.memberBytes), nil
}

// Only frozen owned tokens may be selected. Everything outside the exact current
// span comes from CURRENT, preserving later unrelated bytes on inverse/repeat.
func replaceCodexSharedStdioFrozenToken(current []byte, name string, evidence CodexSharedStdioReplayEvidenceV1, desired CodexSharedStdioTokenState) ([]byte, bool, error) {
	token, present, err := extractTOMLEntryPhysicalTokenWithOwnedFooter(current, "mcp_servers", name, true)
	if err != nil || !present {
		return nil, false, errors.Join(ErrCodexTargetNameConflict, ErrCodexSharedStdioReplayTokenChanged, err)
	}
	wanted, other := evidence.BeforeToken, evidence.AppliedToken
	if desired == CodexSharedStdioApplied {
		wanted, other = evidence.AppliedToken, evidence.BeforeToken
	} else if desired != CodexSharedStdioBefore {
		return nil, false, ErrCodexSharedStdioReplayEvidenceInvalid
	}
	if bytes.Equal(token.memberBytes, wanted) {
		return bytes.Clone(current), false, nil
	}
	if !bytes.Equal(token.memberBytes, other) {
		return nil, false, errors.Join(ErrCodexTargetNameConflict, ErrCodexSharedStdioReplayTokenChanged)
	}
	out := make([]byte, 0, len(current)-len(token.memberBytes)+len(wanted))
	out = append(out, current[:token.memberStart]...)
	out = append(out, wanted...)
	out = append(out, current[token.memberEnd:]...)
	received, present, err := extractTOMLEntryPhysicalTokenWithOwnedFooter(out, "mcp_servers", name, true)
	if err != nil || !present || !bytes.Equal(received.memberBytes, wanted) {
		return nil, false, errors.Join(ErrCodexSharedStdioReplayEvidenceInvalid, err)
	}
	return out, true, nil
}

// Validate a frozen producer's token without re-rendering it through a later
// renderer. Semantic transport plus the existing physical parser's retained
// expression/comment spans establish the original nontransport relationship.
func validateCodexSharedStdioFrozenTokens(original []byte, name string, target MCPEntry, before, applied []byte) error {
	token, present, err := extractTOMLEntryPhysicalToken(original, "mcp_servers", name)
	if err != nil || !present || !bytes.Equal(token.memberBytes, before) {
		return errors.Join(ErrCodexSharedStdioReplayEvidenceInvalid, err)
	}
	candidate := append([]byte(nil), original[:token.memberStart]...)
	candidate = append(candidate, applied...)
	candidate = append(candidate, original[token.memberEnd:]...)
	observed, present, err := extractTOMLEntryPhysicalTokenWithOwnedFooter(candidate, "mcp_servers", name, true)
	if err != nil || !present || !bytes.Equal(observed.memberBytes, applied) {
		return errors.Join(ErrCodexSharedStdioReplayEvidenceInvalid, err)
	}
	expected, err := decodeCodexTOMLBytes(original)
	if err != nil {
		return err
	}
	received, err := decodeCodexTOMLBytes(candidate)
	if err != nil {
		return err
	}
	servers, ok := expected["mcp_servers"].(map[string]any)
	if !ok {
		return ErrCodexSharedStdioReplayEvidenceInvalid
	}
	source, ok := servers[name].(map[string]any)
	if !ok {
		return ErrCodexSharedStdioReplayEvidenceInvalid
	}
	servers[name] = codexSharedStdioReplacement(source, target)
	if !reflect.DeepEqual(expected, received) {
		return ErrCodexSharedStdioReplayEvidenceInvalid
	}
	preserved := func(block []byte) ([]string, error) {
		var parser unstable.Parser
		parser.KeepComments = true
		parser.Reset(block)
		var table []string
		var spans []string
		for parser.NextExpression() {
			expression := parser.Expression()
			var comments func(*unstable.Node)
			comments = func(node *unstable.Node) {
				for ; node != nil; node = node.Next() {
					if node.Kind == unstable.Comment {
						spans = append(spans, "comment:"+string(parser.Raw(node.Raw)))
					}
					comments(node.Child())
				}
			}
			comments(expression)
			transport := false
			if expression.Kind == unstable.Table || expression.Kind == unstable.ArrayTable {
				table = nil
				keys := expression.Key()
				for keys.Next() {
					table = append(table, string(keys.Node().Data))
				}
				transport = len(table) > 2 && codexTransportField(table[2])
			} else if expression.Kind == unstable.KeyValue {
				transport = len(table) > 2 && codexTransportField(table[2])
				if len(table) == 2 {
					keys := expression.Key()
					transport = keys.Next() && codexTransportField(string(keys.Node().Data))
				}
			} else {
				continue
			}
			if !transport {
				spans = append(spans, "expression:"+string(parser.Raw(expression.Raw)))
			}
		}
		return spans, parser.Error()
	}
	originalSpans, err := preserved(before)
	if err != nil {
		return err
	}
	appliedSpans, err := preserved(applied)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(originalSpans, appliedSpans) {
		return ErrCodexSharedStdioReplayEvidenceInvalid
	}
	return nil
}
