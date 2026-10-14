// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package rsync

import (
	"io/fs"
	"os"
	"path"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/parallelworks/xfer/rsync/internal/flist"
)

func pid() int { return os.Getpid() }

// sysInfo extracts the Unix attributes rsync transfers from a FileInfo.
type sysInfo struct {
	uid, gid  uint32
	dev, ino  uint64
	nlink     uint64
	rdev      uint64
	mtimeNsec int32
}

func statSys(fi fs.FileInfo) sysInfo {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return sysInfo{nlink: 1, mtimeNsec: int32(fi.ModTime().Nanosecond())}
	}
	return sysInfo{
		uid: st.Uid, gid: st.Gid,
		dev: uint64(st.Dev), ino: uint64(st.Ino), nlink: uint64(st.Nlink),
		rdev:      uint64(st.Rdev),
		mtimeNsec: int32(fi.ModTime().Nanosecond()),
	}
}

// wireMode converts a Go FileMode to rsync's wire mode.
func wireMode(m fs.FileMode) uint32 {
	perm := uint32(m.Perm())
	if m&fs.ModeSetuid != 0 {
		perm |= 0o4000
	}
	if m&fs.ModeSetgid != 0 {
		perm |= 0o2000
	}
	if m&fs.ModeSticky != 0 {
		perm |= 0o1000
	}
	switch {
	case m.IsDir():
		return flist.TypeDir | perm
	case m&fs.ModeSymlink != 0:
		return flist.TypeLink | perm
	case m&fs.ModeNamedPipe != 0:
		return flist.TypeFIFO | perm
	case m&fs.ModeSocket != 0:
		return flist.TypeSocket | perm
	case m&fs.ModeDevice != 0 && m&fs.ModeCharDevice != 0:
		return flist.TypeChar | perm
	case m&fs.ModeDevice != 0:
		return flist.TypeBlock | perm
	}
	return flist.TypeReg | perm
}

// goMode converts permission bits of a wire mode to a Go FileMode.
func goMode(wire uint32) fs.FileMode {
	m := fs.FileMode(wire & 0o777)
	if wire&0o4000 != 0 {
		m |= fs.ModeSetuid
	}
	if wire&0o2000 != 0 {
		m |= fs.ModeSetgid
	}
	if wire&0o1000 != 0 {
		m |= fs.ModeSticky
	}
	return m
}

func devMajor(rdev uint64) uint32 { return unix.Major(rdev) }
func devMinor(rdev uint64) uint32 { return unix.Minor(rdev) }

// currentUmask returns the process umask.
func currentUmask() uint32 {
	m := unix.Umask(0)
	unix.Umask(m)
	return uint32(m)
}

func isRoot() bool { return os.Geteuid() == 0 }

// withParent opens the directory containing name inside root and calls fn
// with its descriptor and name's base name. It lets us use *at system
// calls that os.Root does not offer while staying inside the root.
func withParent(root *os.Root, name string, fn func(dirfd int, base string) error) error {
	dir, base := path.Split(name)
	if dir == "" {
		dir = "."
	}
	d, err := root.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return fn(int(d.Fd()), base)
}

// lchtimes sets the modification time of name without following a final
// symlink.
func lchtimes(root *os.Root, name string, mtime time.Time) error {
	return withParent(root, name, func(dirfd int, base string) error {
		now := unix.NsecToTimespec(time.Now().UnixNano())
		ts := []unix.Timespec{now, unix.NsecToTimespec(mtime.UnixNano())}
		return unix.UtimesNanoAt(dirfd, base, ts, unix.AT_SYMLINK_NOFOLLOW)
	})
}

// mknod creates a device or special file. The path is resolved by name
// below the root's directory; os.Root has no mknod.
func mknod(root *os.Root, name string, f *flist.File) error {
	if err := checkInRoot(root, name); err != nil {
		return err
	}
	p := path.Join(root.Name(), name)
	mode := f.Mode & 0o7777
	switch f.Mode & flist.TypeMask {
	case flist.TypeFIFO:
		return unix.Mkfifo(p, mode)
	case flist.TypeChar:
		mode |= unix.S_IFCHR
	case flist.TypeBlock:
		mode |= unix.S_IFBLK
	case flist.TypeSocket:
		mode |= unix.S_IFSOCK
	}
	return unix.Mknod(p, mode, int(unix.Mkdev(f.RdevMajor, f.RdevMinor)))
}

// checkInRoot verifies that the parent directory of name resolves inside
// root, so that path-based calls cannot be redirected by symlinks.
func checkInRoot(root *os.Root, name string) error {
	dir := path.Dir(name)
	fi, err := root.Stat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return &fs.PathError{Op: "mknod", Path: name, Err: syscall.ENOTDIR}
	}
	return withParent(root, name, func(int, string) error { return nil })
}
