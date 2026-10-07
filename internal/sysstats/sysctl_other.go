//go:build !darwin

package sysstats

import "errors"

var errUnsupported = errors.New("sysstats: unsupported on this platform")

func sysctlRaw(string) ([]byte, error) { return nil, errUnsupported }
