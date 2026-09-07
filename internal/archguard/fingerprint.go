package archguard

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
)

func normalizeEvidence(s string) string {
	// Evidence may contain Go source expressions and literals. Internal
	// whitespace, backslashes, and slashes are semantic and must not collapse
	// into the same fingerprint. Renderers already produce canonical Go syntax;
	// only transport-level line endings and outer whitespace are normalized.
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.TrimSpace(s)
}

func Fingerprint(v Violation) string {
	evidence := normalizeEvidence(v.Evidence)
	if v.Kind == KindFileBudget {
		switch {
		case strings.HasPrefix(evidence, "production_"):
			evidence = "production_line_count"
		case strings.HasPrefix(evidence, "test_"):
			evidence = "test_line_count"
		}
	}
	payload := strings.Join([]string{
		string(v.Kind),
		filepath.ToSlash(v.Location.Path),
		v.Location.Symbol,
		evidence,
	}, "\x00")
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}
