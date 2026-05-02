//go:build !linux && !windows

package lpp

import "fmt"

func defaultStatfs(path string) (int64, error) {
	return 0, fmt.Errorf("statfs not implemented on this platform (%s)", path)
}
