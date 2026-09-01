package process

// TokenizeWindowsCommandLine is a pure-Go CommandLineToArgvW-compatible argv
// parser. It intentionally skips leading whitespace rather than emitting an
// empty argv[0], which is safe for callers that match command identity only.
func TokenizeWindowsCommandLine(commandLine string) []string {
	var tokens []string
	for len(commandLine) > 0 {
		if commandLine[0] == ' ' || commandLine[0] == '\t' {
			commandLine = commandLine[1:]
			continue
		}
		arg, rest := readNextWindowsArg(commandLine)
		tokens, commandLine = append(tokens, string(arg)), rest
	}
	return tokens
}

func readNextWindowsArg(commandLine string) (arg []byte, rest string) {
	var out []byte
	var inQuote bool
	var slashCount int
	for ; len(commandLine) > 0; commandLine = commandLine[1:] {
		char := commandLine[0]
		switch char {
		case ' ', '\t':
			if !inQuote {
				return appendWindowsBackslashes(out, slashCount), commandLine[1:]
			}
		case '"':
			out = appendWindowsBackslashes(out, slashCount/2)
			if slashCount%2 == 0 {
				if inQuote && len(commandLine) > 1 && commandLine[1] == '"' {
					out = append(out, char)
					commandLine = commandLine[1:]
				}
				inQuote = !inQuote
			} else {
				out = append(out, char)
			}
			slashCount = 0
			continue
		case '\\':
			slashCount++
			continue
		}
		out = appendWindowsBackslashes(out, slashCount)
		slashCount = 0
		out = append(out, char)
	}
	return appendWindowsBackslashes(out, slashCount), ""
}

func appendWindowsBackslashes(out []byte, count int) []byte {
	for ; count > 0; count-- {
		out = append(out, '\\')
	}
	return out
}
