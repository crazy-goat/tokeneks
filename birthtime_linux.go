//go:build linux

package main

import (
	"os"
	"time"

	"golang.org/x/sys/unix"
)

func getCreatedAt(path string) time.Time {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}

	// Try to get birth time via statx (Linux >= 4.11)
	var stx unix.Statx_t
	err = unix.Statx(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_BTIME, &stx)
	if err == nil && stx.Mask&unix.STATX_BTIME != 0 {
		return time.Unix(int64(stx.Btime.Sec), int64(stx.Btime.Nsec))
	}

	// Fallback to modification time
	return info.ModTime()
}
