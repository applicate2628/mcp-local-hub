package daemon

import (
	"runtime"
	"sort"
	"strings"
)

// MergeDaemonEnvironment retains the supervisor owner's platform-aware,
// deterministic parent < manifest < overlay precedence.
func MergeDaemonEnvironment(parent []string, manifest, overlay map[string]string) []string {
	if len(manifest) == 0 && len(overlay) == 0 {
		return nil
	}

	winCaseFold := runtime.GOOS == "windows"

	// normKey returns the lookup key used for collision detection.
	// On Windows it folds to uppercase so PATH/Path/path collide.
	// On POSIX it returns the key verbatim.
	normKey := func(k string) string {
		if winCaseFold {
			return strings.ToUpper(k)
		}
		return k
	}

	// entries holds the winning "<key>=<value>" for each normalized
	// key. Later writes (manifest after parent, overlay after
	// manifest) overwrite earlier ones, so the highest-precedence
	// source wins.
	entries := make(map[string]string)

	// Parent goes in first. Lines without '=' are skipped (defensive;
	// os.Environ() never emits malformed entries but a test caller
	// might supply hand-crafted input).
	for _, kv := range parent {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		entries[normKey(kv[:eq])] = kv
	}

	// Manifest overrides parent for the same normalized key.
	// Within a single layer, two source keys can collide under
	// Windows case-fold (e.g. "PATH" and "Path" both in the manifest
	// map). Sort the source keys before applying so the
	// last-write-wins outcome is deterministic across map iteration
	// orders. Sort is by original (non-normalized) key so on POSIX
	// the two cases remain distinct buckets; on Windows the sort is
	// still deterministic and the later case (e.g. "Path" > "PATH"
	// in lexicographic order) wins.
	mKeys := make([]string, 0, len(manifest))
	for k := range manifest {
		mKeys = append(mKeys, k)
	}
	sort.Strings(mKeys)
	for _, k := range mKeys {
		entries[normKey(k)] = k + "=" + manifest[k]
	}

	// Overlay overrides both for the same normalized key. Same
	// within-layer sort discipline as above.
	oKeys := make([]string, 0, len(overlay))
	for k := range overlay {
		oKeys = append(oKeys, k)
	}
	sort.Strings(oKeys)
	for _, k := range oKeys {
		entries[normKey(k)] = k + "=" + overlay[k]
	}

	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]string, 0, len(entries))
	for _, k := range keys {
		out = append(out, entries[k])
	}
	return out
}

func composeChildEnvFrom(parent []string, env map[string]string, unset []string) []string {
	var unsetSet map[string]bool
	if len(unset) > 0 {
		unsetSet = make(map[string]bool, len(unset))
		for _, k := range unset {
			unsetSet[envKeyNorm(k)] = true
		}
	}
	out := make([]string, 0, len(parent)+len(env))
	for _, kv := range parent {
		if unsetSet != nil {
			if k, _, ok := strings.Cut(kv, "="); ok && unsetSet[envKeyNorm(k)] {
				continue
			}
		}
		out = append(out, kv)
	}
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}
