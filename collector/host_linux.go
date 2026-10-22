//go:build linux

package collector

import "syscall"

func systemUname() (Uname, error) {
	var name syscall.Utsname
	if err := syscall.Uname(&name); err != nil {
		return Uname{}, err
	}
	return Uname{
		Sysname: utsString(name.Sysname[:]),
		Release: utsString(name.Release[:]),
		Version: utsString(name.Version[:]),
		Machine: utsString(name.Machine[:]),
	}, nil
}

// utsString converts a NUL-terminated utsname field. The element type is
// int8 or uint8 depending on the architecture.
func utsString[T int8 | uint8](field []T) string {
	buffer := make([]byte, 0, len(field))
	for _, character := range field {
		if character == 0 {
			break
		}
		buffer = append(buffer, byte(character))
	}
	return string(buffer)
}

func systemStatfs(path string) (FSStats, error) {
	var stats syscall.Statfs_t
	if err := syscall.Statfs(path, &stats); err != nil {
		return FSStats{}, err
	}
	return FSStats{
		SizeBytes:   uint64(stats.Blocks) * uint64(stats.Bsize),
		TotalInodes: uint64(stats.Files),
	}, nil
}
