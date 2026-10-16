//go:build !linux && !darwin && !windows

package sysstats

func statfs(string) (FSInfo, error) { return FSInfo{}, errUnsupported }
