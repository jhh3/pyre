//go:build windows

package winenv

import (
	"os/exec"
	"path/filepath"

	"golang.org/x/sys/windows/registry"
)

func lookPath(name string) (string, error) { return exec.LookPath(name) }

// RegistryPath returns the machine and user PATH as currently stored in the
// registry (expanded), so a node started at logon finds tools installed
// afterwards (winget, MSI and nvm-windows installers edit these values).
func RegistryPath() []string {
	var out []string
	for _, k := range []struct {
		root registry.Key
		path string
	}{
		{registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`},
		{registry.CURRENT_USER, `Environment`},
	} {
		key, err := registry.OpenKey(k.root, k.path, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		v, _, err := key.GetStringValue("Path")
		key.Close()
		if err != nil {
			continue
		}
		if x, err := registry.ExpandString(v); err == nil {
			v = x
		}
		out = append(out, filepath.SplitList(v)...)
	}
	return out
}
