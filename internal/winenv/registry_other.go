//go:build !windows

package winenv

import "os/exec"

func lookPath(name string) (string, error) { return exec.LookPath(name) }

// RegistryPath is Windows-only; elsewhere it returns nil.
func RegistryPath() []string { return nil }
