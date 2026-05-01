//go:build !linux

package csi

import "fmt"

func statfs(path string) (int64, int64, int64, int64, int64, int64, error) {
	return 0, 0, 0, 0, 0, 0, fmt.Errorf("statfs not implemented on this platform (%s)", path)
}
