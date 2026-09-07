package archguard

import "sort"

func packageStringEvaluationKeys(groups map[string][]int) []string {
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
