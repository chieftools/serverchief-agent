//go:build !unix

package pkglock

func probeFcntl(string) (int, bool, error) { return 0, false, nil }

func processAlive(int) bool { return false }
