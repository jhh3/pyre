package sysstats

import "syscall"

func sysctlRaw(name string) ([]byte, error) {
	s, err := syscall.Sysctl(name)
	if err != nil {
		return nil, err
	}
	return []byte(s), nil
}
