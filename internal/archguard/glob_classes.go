package archguard

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

func translateGlobClass(pattern string, start int) (string, int, bool) {
	if !utf8.ValidString(pattern) {
		return "", start, false
	}
	index := start + 1
	if index >= len(pattern) {
		return "", start, false
	}

	negated := false
	if pattern[index] == '^' {
		negated = true
		index++
	}
	contentStart := index
	for index < len(pattern) && pattern[index] != ']' {
		_, size := utf8.DecodeRuneInString(pattern[index:])
		if size == 0 {
			return "", start, false
		}
		index += size
	}
	if index >= len(pattern) || index == contentStart {
		return "", start, false
	}

	raw := pattern[contentStart:index]
	if strings.ContainsRune(raw, '/') {
		return "", start, false
	}

	var class strings.Builder
	class.WriteByte('[')
	if negated {
		// A path-segment character class must never match the separator.
		class.WriteString("^/")
	}
	for _, r := range raw {
		if unicode.IsControl(r) {
			return "", start, false
		}
		switch r {
		case '\\', '[', ']':
			class.WriteByte('\\')
			class.WriteRune(r)
		case '^':
			class.WriteString(`\^`)
		default:
			// Preserve '-' so standard ranges such as [a-z] and [α-ω]
			// retain their regular-expression meaning.
			class.WriteRune(r)
		}
	}
	class.WriteByte(']')

	translated := class.String()
	re, err := regexp.Compile("^" + translated + "$")
	if err != nil || re.MatchString("/") {
		return "", start, false
	}
	return translated, index + 1, true
}
