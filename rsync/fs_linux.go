// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package rsync

import (
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// futimes sets the modification time of an open file (and its access
// time to now) through its descriptor.
func futimes(f *os.File, mtime time.Time) error {
	ts := [2]unix.Timespec{{Nsec: unix.UTIME_NOW}, unix.NsecToTimespec(mtime.UnixNano())}
	sc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var errno unix.Errno
	if err := sc.Control(func(fd uintptr) {
		// utimensat with a NULL path acts on fd itself.
		_, _, errno = unix.Syscall6(unix.SYS_UTIMENSAT, fd, 0, uintptr(unsafe.Pointer(&ts[0])), 0, 0, 0)
	}); err != nil {
		return err
	}
	if errno != 0 {
		return errno
	}
	return nil
}

const oNoatime = unix.O_NOATIME

// preallocate reserves size bytes for f without changing its length.
func preallocate(f *os.File, size int64) error {
	return unix.Fallocate(int(f.Fd()), unix.FALLOC_FL_KEEP_SIZE, 0, size)
}
