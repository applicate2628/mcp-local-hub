//go:build windows

package sonnet

import "golang.org/x/sys/windows/registry"

func defaultRegistryRoots() (map[string]string, error) {
	out := map[string]string{}
	const base = `SOFTWARE\Sonnet Software`
	for _, view := range []uint32{registry.WOW64_64KEY, registry.WOW64_32KEY} {
		k, e := registry.OpenKey(registry.LOCAL_MACHINE, base, registry.READ|view)
		if e != nil {
			continue
		}
		names, e := k.ReadSubKeyNames(-1)
		k.Close()
		if e != nil {
			continue
		}
		for _, name := range names {
			child, e := registry.OpenKey(registry.LOCAL_MACHINE, base+`\`+name, registry.READ|view)
			if e != nil {
				continue
			}
			root, _, e := child.GetStringValue("SONNET_DIR")
			child.Close()
			if e == nil && root != "" {
				out[name] = root
			}
		}
	}
	return out, nil
}
