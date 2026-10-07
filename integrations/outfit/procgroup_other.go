//go:build !unix

package outfit

import "os/exec"

func configureProcessGroup(*exec.Cmd) {}
