package sysstats

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

// BootTime is when this machine last booted. pyred compares it with a
// run's start to tell a host reboot (every process died with the machine)
// from a run process that died on its own (crash resume, interrupted
// reports).
func BootTime() (time.Time, error) { return NewSampler().BootTime() }

// BootTime reads the boot time the sampler's platform reports: Linux
// /proc/stat btime, macOS kern.boottime, Windows now minus GetTickCount64.
func (s *Sampler) BootTime() (time.Time, error) {
	switch s.GOOS {
	case "linux":
		b, err := s.read("stat")
		if err != nil {
			return time.Time{}, err
		}
		return parseBtime(b)
	case "darwin":
		b, err := s.Sysctl("kern.boottime")
		if err != nil {
			return time.Time{}, err
		}
		return parseBoottime(b)
	case "windows":
		up, err := winUptime()
		if err != nil {
			return time.Time{}, err
		}
		return s.Now().Add(-up), nil
	}
	return time.Time{}, errUnsupportedBoot
}

var errUnsupportedBoot = errors.New("sysstats: boot time is not available on this platform")

// parseBtime reads the "btime <unix seconds>" line of /proc/stat.
func parseBtime(stat string) (time.Time, error) {
	for _, line := range strings.Split(stat, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "btime" {
			sec, err := strconv.ParseInt(f[1], 10, 64)
			if err != nil || sec <= 0 {
				return time.Time{}, errors.New("sysstats: bad btime in /proc/stat")
			}
			return time.Unix(sec, 0), nil
		}
	}
	return time.Time{}, errors.New("sysstats: no btime in /proc/stat")
}

// parseBoottime decodes macOS kern.boottime, a struct timeval (int64
// seconds, int32 microseconds, little endian; one trailing NUL may be
// trimmed).
func parseBoottime(b []byte) (time.Time, error) {
	if len(b) < 8 {
		return time.Time{}, errors.New("sysstats: short kern.boottime")
	}
	sec := int64(le64(b, 0)) //nolint:gosec // seconds since 1970 fit
	if sec <= 0 {
		return time.Time{}, errors.New("sysstats: bad kern.boottime")
	}
	usec := int64(le32(b, 8))
	if usec >= 1e6 {
		usec = 0
	}
	return time.Unix(sec, usec*1000), nil
}
