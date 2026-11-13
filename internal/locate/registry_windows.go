//go:build windows

package locate

import "golang.org/x/sys/windows/registry"

// registryInstallPaths reads InstallPath from the 32-bit view
// (WOW6432Node) first, then the native one.
func registryInstallPaths() []string {
	var out []string
	for _, view := range []uint32{registry.WOW64_32KEY, registry.WOW64_64KEY} {
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Blizzard Entertainment\World of Warcraft`, registry.QUERY_VALUE|view)
		if err != nil {
			continue
		}
		p, _, err := k.GetStringValue("InstallPath")
		_ = k.Close()
		if err == nil && p != "" {
			out = append(out, p)
		}
	}
	return out
}
