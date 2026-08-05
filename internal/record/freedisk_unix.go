//go:build darwin || linux

package record

import "syscall"

// freeDisk reports the free bytes available to unprivileged writes on the
// volume holding path.
func freeDisk(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
