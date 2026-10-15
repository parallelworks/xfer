// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

// Package flist encodes, decodes and orders rsync file lists.
package flist

import (
	"strings"
)

// Wire file types (the traditional Unix S_IFMT values).
const (
	TypeMask   = 0o170000
	TypeSocket = 0o140000
	TypeLink   = 0o120000
	TypeReg    = 0o100000
	TypeBlock  = 0o060000
	TypeDir    = 0o040000
	TypeChar   = 0o020000
	TypeFIFO   = 0o010000
)

// Local flags.
const (
	FlagTopDir     = 1 << 0  // a transfer root
	FlagContentDir = 1 << 2  // the directory's contents are in the list
	FlagImpliedDir = 1 << 6  // an ancestor sent only to hold -R paths
	FlagMountDir   = 1 << 7  // crosses a filesystem boundary (-x)
	FlagHlinkFirst = 1 << 8  // leader of a hard-link set
	FlagHlinked    = 1 << 9  // member of a hard-link set
	FlagCleared    = 1 << 15 // duplicate removed by Clean
)

// File is one file-list entry.
type File struct {
	Name                 string // relative wire path, "/"-separated, no trailing "/"
	Mode                 uint32 // wire mode: type bits and permission bits
	Size                 int64  // length in bytes
	ModTime              int64  // seconds since the epoch
	ModNsec              int32  // nanoseconds of ModTime; sent from protocol 31 on
	UID, GID             uint32 // owner and group ids
	RdevMajor, RdevMinor uint32 // device number of a device file
	Link                 string // symlink target
	Checksum             []byte // with --checksum, the whole-file checksum
	Flags                uint32 // local Flag* bits

	// Hard links. With protocol 30+ HLinkIndex is the transmission-order
	// index of this entry's leader (its own index for a leader); for older
	// protocols Dev and Ino identify the inode.
	HLinkIndex int32
	Dev, Ino   int64 // device and inode, before protocol 30

	// Sender-only: where to read the file from, and for a directory the
	// device of its transfer root (for -x when it is listed later).
	Source  string
	RootDev uint64 // device of the transfer root
}

// IsDir reports whether f is a directory.
func (f *File) IsDir() bool { return f.Mode&TypeMask == TypeDir }

// IsRegular reports whether f is a regular file.
func (f *File) IsRegular() bool { return f.Mode&TypeMask == TypeReg }

// IsLink reports whether f is a symbolic link.
func (f *File) IsLink() bool { return f.Mode&TypeMask == TypeLink }

// IsDevice reports whether f is a character or block device.
func (f *File) IsDevice() bool {
	t := f.Mode & TypeMask
	return t == TypeChar || t == TypeBlock
}

// IsSpecial reports whether f is a FIFO or a socket.
func (f *File) IsSpecial() bool {
	t := f.Mode & TypeMask
	return t == TypeFIFO || t == TypeSocket
}

// Perm returns the permission bits (including setuid, setgid and sticky).
func (f *File) Perm() uint32 { return f.Mode & 0o7777 }

// Compare orders two entries as rsync does for protocol proto.
func Compare(a, b *File, proto int) int {
	if proto < 29 {
		return strings.Compare(a.Name, b.Name)
	}
	return compareComponents(a.Name, a.IsDir(), b.Name, b.IsDir())
}

// component iteration for protocol 29+ ordering: every directory component
// (including a directory leaf) compares as "name/" and sorts after all
// non-directory leaves at the same level.
type compIter struct {
	s     string
	isDir bool
}

func newCompIter(name string, isDir bool) compIter {
	if isDir {
		if name == "." {
			name = ""
		} else if strings.HasSuffix(name, "/.") {
			name = name[:len(name)-2]
		}
	}
	return compIter{s: name, isDir: isDir}
}

// next returns the next component, whether it is a path (directory)
// component, and false when exhausted. Directory components are returned
// without their trailing slash; callers account for it.
func (it *compIter) next() (comp string, isPath, ok bool) {
	if it.s == "" {
		return "", false, false
	}
	if i := strings.IndexByte(it.s, '/'); i >= 0 {
		comp, it.s = it.s[:i], it.s[i+1:]
		return comp, true, true
	}
	comp, it.s = it.s, ""
	return comp, it.isDir, true
}

func compareComponents(a string, aDir bool, b string, bDir bool) int {
	ia, ib := newCompIter(a, aDir), newCompIter(b, bDir)
	for {
		ca, pa, oka := ia.next()
		cb, pb, okb := ib.next()
		switch {
		case !oka && !okb:
			return 0
		case !oka:
			return -1
		case !okb:
			return 1
		}
		if pa != pb {
			if pa {
				return 1
			}
			return -1
		}
		if c := compareWithSlash(ca, cb, pa); c != 0 {
			return c
		}
	}
}

// compareWithSlash compares a and b bytewise, as if both had a trailing "/"
// when slash is set.
func compareWithSlash(a, b string, slash bool) int {
	if !slash {
		return strings.Compare(a, b)
	}
	n := min(len(a), len(b))
	if c := strings.Compare(a[:n], b[:n]); c != 0 {
		return c
	}
	switch {
	case len(a) == len(b):
		return 0
	case len(a) < len(b): // a's "/" against b[n]
		return cmpByte('/', b[n])
	default:
		return cmpByte(a[n], '/')
	}
}

func cmpByte(x, y byte) int {
	switch {
	case x < y:
		return -1
	case x > y:
		return 1
	}
	return 0
}
