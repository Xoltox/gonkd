package api

import "golang.org/x/sys/unix"

// statfsFree returns free bytes available on the filesystem holding dir,
// used to reject uploads that wouldn't fit in the target box's small
// (~60MB) tmpfs /tmp.
func statfsFree(dir string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
