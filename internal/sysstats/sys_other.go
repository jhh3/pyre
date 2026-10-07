//go:build !linux && !darwin

package sysstats

func statfs(string) (FSInfo, error) { return FSInfo{}, errUnsupported }
