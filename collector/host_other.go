//go:build !linux

package collector

import "errors"

var errUnsupportedPlatform = errors.New("only supported on Linux")

func systemUname() (Uname, error) {
	return Uname{}, errUnsupportedPlatform
}

func systemStatfs(string) (FSStats, error) {
	return FSStats{}, errUnsupportedPlatform
}
