//go:build linux

package shell

import "golang.org/x/sys/unix"

const termiosGet = unix.TCGETS
