//go:build linux

package csi

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// bindMount creates a read-write or read-only bind mount of src onto target
// via syscall directly, sidestepping mount-utils' shell-out to /bin/mount
// (the distroless runtime image carries no userspace mount binary).
func bindMount(src, target string, readOnly bool) error {
	if err := unix.Mount(src, target, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind %s -> %s: %w", src, target, err)
	}
	if readOnly {
		// Remount with read-only — a single mount(2) call can't both bind
		// AND change flags; it has to be done in two steps.
		if err := unix.Mount("", target, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
			_ = unix.Unmount(target, 0)
			return fmt.Errorf("remount %s read-only: %w", target, err)
		}
	}
	return nil
}

// unmountQuiet unmounts target, treating "not mounted" / "no such file" as
// success so NodeUnpublishVolume stays idempotent.
func unmountQuiet(target string) error {
	if err := unix.Unmount(target, 0); err != nil {
		if err == unix.EINVAL || err == unix.ENOENT {
			return nil
		}
		return err
	}
	return nil
}

// isMountPoint reports whether target is a mount point by comparing its
// device number with its parent's. If target doesn't exist, returns
// (true, nil) so NodeUnpublishVolume treats it as "already gone".
func isMountPoint(target string) (notMnt bool, err error) {
	st, err := os.Stat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, err
	}
	parent, err := os.Stat(target + "/..")
	if err != nil {
		return false, err
	}
	// os.FileInfo.Sys() returns *syscall.Stat_t (not unix.Stat_t) — they are
	// the same struct layout but distinct Go types.
	stSys := st.Sys().(*syscall.Stat_t)
	parentSys := parent.Sys().(*syscall.Stat_t)
	return stSys.Dev == parentSys.Dev, nil
}
