//go:build !linux

package csi

import (
	"os"

	"k8s.io/mount-utils"
)

// Fallback for non-Linux platforms — defer to mount-utils' platform impl
// (Windows uses os.Symlink for directory junctions).
func bindMount(src, target string, readOnly bool) error {
	m := mount.New("")
	opts := []string{"bind"}
	if readOnly {
		opts = append(opts, "ro")
	}
	return m.Mount(src, target, "", opts)
}

func unmountQuiet(target string) error {
	m := mount.New("")
	if err := m.Unmount(target); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return nil
}

func isMountPoint(target string) (notMnt bool, err error) {
	m := mount.New("")
	return m.IsLikelyNotMountPoint(target)
}
