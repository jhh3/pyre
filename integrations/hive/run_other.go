//go:build !unix

package hive

import "os/exec"

func setProcessGroup(cmd *exec.Cmd) {}

func lowerPriority(cmd *exec.Cmd) {}
