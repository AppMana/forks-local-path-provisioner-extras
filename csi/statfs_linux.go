//go:build linux

package csi

import "golang.org/x/sys/unix"

// statfs returns (available, total, used, inodesFree, inodes, inodesUsed).
func statfs(path string) (int64, int64, int64, int64, int64, int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, 0, 0, 0, 0, err
	}
	bsize := int64(st.Bsize)
	available := int64(st.Bavail) * bsize
	total := int64(st.Blocks) * bsize
	used := total - int64(st.Bfree)*bsize
	inodes := int64(st.Files)
	inodesFree := int64(st.Ffree)
	inodesUsed := inodes - inodesFree
	return available, total, used, inodesFree, inodes, inodesUsed, nil
}
