// Package cmakelex shares the CMake delimiter rules used by the vcpkg readers.
package cmakelex

// SkipQuoted returns the byte offset just past the quote at start, or the end of input.
func SkipQuoted(src string, start int) int {
	for i := start + 1; i < len(src); i++ {
		if src[i] == '\\' && i+1 < len(src) {
			i++
			continue
		}
		if src[i] == '"' {
			return i + 1
		}
	}
	return len(src)
}

// MatchBracketOpen returns the delimiter width and raw-content offset for a bracket at at.
func MatchBracketOpen(src string, at int) (equals, contentStart int, ok bool) {
	if at >= len(src) || src[at] != '[' {
		return 0, 0, false
	}
	p := at + 1
	for p < len(src) && src[p] == '=' {
		p++
	}
	if p >= len(src) || src[p] != '[' {
		return 0, 0, false
	}
	return p - at - 1, p + 1, true
}

// FindBracketClose returns the raw-content end and offset after a matching closing delimiter.
func FindBracketClose(src string, contentStart, equals int) (contentEnd, afterClose int, ok bool) {
	for p := contentStart; p < len(src); p++ {
		if src[p] != ']' {
			continue
		}
		q := p + 1
		for e := 0; e < equals && q < len(src) && src[q] == '='; e++ {
			q++
		}
		if q == p+1+equals && q < len(src) && src[q] == ']' {
			return p, q + 1, true
		}
	}
	return 0, 0, false
}
