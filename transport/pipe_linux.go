// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package transport

import (
	"os"

	"golang.org/x/sys/unix"
)

// pipeSize is the capacity GrowPipe asks for: the default 64KiB pipe
// splits every large write into several partial writes and wakeups.
// Unprivileged processes may raise pipes up to /proc/sys/fs/pipe-max-size,
// 1MiB by default.
const pipeSize = 1 << 20

// GrowPipe enlarges the kernel buffer of a pipe. It does nothing for other
// kinds of files or when the kernel refuses.
func GrowPipe(f *os.File) {
	sc, err := f.SyscallConn()
	if err != nil {
		return
	}
	sc.Control(func(fd uintptr) {
		var st unix.Stat_t
		if unix.Fstat(int(fd), &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFIFO {
			return
		}
		unix.FcntlInt(fd, unix.F_SETPIPE_SZ, pipeSize)
	})
}
