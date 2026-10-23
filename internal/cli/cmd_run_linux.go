package cli

import "syscall"

// childSysProcAttr: the child gets SIGKILL when pyre run dies (a SIGKILLed
// wrapper forwards nothing), and with group its own process group.
func childSysProcAttr(group bool) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL, Setpgid: group}
}
