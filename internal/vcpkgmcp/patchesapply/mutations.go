package patchesapply

import "strings"

// statementMutationDestinations classifies standard CMake commands that can
// write variables without using set()/unset(). relevant distinguishes commands
// in this mutator family from unrelated harmless commands; modeled is false
// when a relevant command's output contract cannot be identified safely.
func statementMutationDestinations(st statement) (destinations []string, relevant, modeled bool) {
	toks := tokenize(st.Args)
	switch st.Name {
	case "string":
		destinations, modeled = stringMutationDestinations(toks)
	case "list":
		destinations, modeled = listMutationDestinations(toks)
	case "file":
		destinations, modeled = fileMutationDestinations(toks)
	case "math":
		destinations, modeled = fixedMutationDestination(toks, "EXPR", 1)
	case "get_filename_component":
		destinations, modeled = getFilenameComponentMutationDestinations(toks)
	default:
		return nil, false, true
	}
	return destinations, true, modeled
}

func getFilenameComponentMutationDestinations(toks []token) ([]string, bool) {
	if len(toks) < 2 {
		return nil, false
	}
	destinations, ok := mutationVariablesAt(toks, []int{0})
	if !ok {
		return nil, false
	}
	for i := 2; i < len(toks); i++ {
		if toks[i].Quoted || toks[i].Text != "PROGRAM_ARGS" {
			continue
		}
		secondary, ok := mutationVariablesAt(toks, []int{i + 1})
		if !ok {
			return nil, false
		}
		destinations = append(destinations, secondary...)
		i++
	}
	return destinations, true
}

func staticMutationVariable(t token) (string, bool) {
	if t.Text == "" || !rePlainIdent.MatchString(t.Text) {
		return "", false
	}
	return t.Text, true
}

func fixedMutationDestination(toks []token, subcommand string, index int) ([]string, bool) {
	if len(toks) <= index || toks[0].Quoted || toks[0].Text != subcommand {
		return nil, false
	}
	name, ok := staticMutationVariable(toks[index])
	if !ok {
		return nil, false
	}
	return []string{name}, true
}

func listMutationDestinations(toks []token) ([]string, bool) {
	if len(toks) < 2 || toks[0].Quoted {
		return nil, false
	}
	subcommand := toks[0].Text
	var indices []int
	switch subcommand {
	case "APPEND", "PREPEND", "INSERT", "REMOVE_ITEM", "REMOVE_AT", "REMOVE_DUPLICATES", "FILTER", "REVERSE", "SORT":
		indices = []int{1}
	case "LENGTH":
		indices = []int{2}
	case "GET":
		if len(toks) < 4 {
			return nil, false
		}
		indices = []int{len(toks) - 1}
	case "SUBLIST":
		indices = []int{4}
	case "FIND", "JOIN":
		indices = []int{3}
	case "POP_BACK", "POP_FRONT":
		indices = make([]int, 0, len(toks)-1)
		for i := 1; i < len(toks); i++ {
			indices = append(indices, i)
		}
	case "TRANSFORM":
		indices = []int{1}
		for i := 2; i < len(toks); i++ {
			if toks[i].Text == "OUTPUT_VARIABLE" {
				if i+1 >= len(toks) {
					return nil, false
				}
				indices = []int{i + 1}
				break
			}
		}
	default:
		return nil, false
	}
	return mutationVariablesAt(toks, indices)
}

func fileMutationDestinations(toks []token) ([]string, bool) {
	if len(toks) == 0 || toks[0].Quoted {
		return nil, false
	}
	subcommand := toks[0].Text
	switch subcommand {
	case "READ", "STRINGS", "TIMESTAMP", "SIZE", "REAL_PATH", "TO_CMAKE_PATH", "TO_NATIVE_PATH":
		return mutationVariablesAt(toks, []int{2})
	case "GLOB", "GLOB_RECURSE", "RELATIVE_PATH":
		return mutationVariablesAt(toks, []int{1})
	case "READ_SYMLINK":
		destinations, ok := mutationVariablesAt(toks, []int{2})
		if !ok {
			return nil, false
		}
		optional, ok := mutationVariablesFollowing(toks, map[string]struct{}{"RESULT": {}})
		return append(destinations, optional...), ok
	case "MD5", "SHA1", "SHA224", "SHA256", "SHA384", "SHA512", "SHA3_224", "SHA3_256", "SHA3_384", "SHA3_512":
		return mutationVariablesAt(toks, []int{2})
	case "MAKE_DIRECTORY", "RENAME", "COPY_FILE", "CREATE_LINK":
		return mutationVariablesFollowing(toks, map[string]struct{}{"RESULT": {}})
	case "DOWNLOAD", "UPLOAD":
		return mutationVariablesFollowing(toks, map[string]struct{}{"STATUS": {}, "LOG": {}})
	case "LOCK":
		return mutationVariablesFollowing(toks, map[string]struct{}{"RESULT_VARIABLE": {}})
	case "GET_RUNTIME_DEPENDENCIES":
		for _, tok := range toks[1:] {
			if tok.Text == "CONFLICTING_DEPENDENCIES_PREFIX" {
				// This mode derives several variable names from one prefix; do not
				// guess which retained facts it overwrites.
				return nil, false
			}
		}
		return mutationVariablesFollowing(toks, map[string]struct{}{
			"RESOLVED_DEPENDENCIES_VAR":   {},
			"UNRESOLVED_DEPENDENCIES_VAR": {},
		})
	case "WRITE", "APPEND", "TOUCH", "TOUCH_NOCREATE", "GENERATE", "CONFIGURE",
		"REMOVE", "REMOVE_RECURSE", "COPY", "INSTALL", "CHMOD", "CHMOD_RECURSE",
		"ARCHIVE_CREATE", "ARCHIVE_EXTRACT":
		return nil, true
	default:
		return nil, false
	}
}

func mutationVariablesAt(toks []token, indices []int) ([]string, bool) {
	destinations := make([]string, 0, len(indices))
	for _, index := range indices {
		if index < 0 || index >= len(toks) {
			return nil, false
		}
		name, ok := staticMutationVariable(toks[index])
		if !ok {
			return nil, false
		}
		destinations = append(destinations, name)
	}
	return destinations, true
}

func mutationVariablesFollowing(toks []token, keywords map[string]struct{}) ([]string, bool) {
	var destinations []string
	for i := 1; i < len(toks); i++ {
		if _, ok := keywords[strings.ToUpper(toks[i].Text)]; !ok || toks[i].Quoted {
			continue
		}
		if i+1 >= len(toks) {
			return nil, false
		}
		name, ok := staticMutationVariable(toks[i+1])
		if !ok {
			return nil, false
		}
		destinations = append(destinations, name)
		i++
	}
	return destinations, true
}
