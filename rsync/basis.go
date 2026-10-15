// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package rsync

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/parallelworks/xfer/rsync/internal/flist"
)

// basisPath is where name would be in basis directory dir.
func (rv *receiving) basisPath(dir, name string) string {
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(rv.root.Name(), dir)
	}
	return filepath.Join(dir, name)
}

// tryBasisDirs looks for a file the destination lacks in the basis
// directories. If one holds it unchanged, the file is dealt with here
// (skipped, copied or linked) and done is set. Otherwise basis is the
// first regular file found, to use as the delta basis, or "".
func (rv *receiving) tryBasisDirs(ndx int32, f *flist.File, name string) (done bool, basis string) {
	o := rv.o
	if rv.root == nil {
		return false, ""
	}
	for _, dir := range o.BasisDirs {
		p := rv.basisPath(dir, name)
		fi, err := os.Lstat(p)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if basis == "" {
			basis = p
		}
		if !rv.unchangedAt(f, p, fi) {
			continue
		}
		switch o.BasisMode {
		case BasisCompare:
			rv.itemize(ndx, f, 0)
			return true, ""
		case BasisLink:
			if !rv.sameAttrs(f, fi) {
				continue
			}
			if o.DryRun {
				rv.itemize(ndx, f, itemLocalChange)
				return true, ""
			}
			err := os.Link(p, filepath.Join(rv.root.Name(), name))
			if err == nil {
				rv.itemize(ndx, f, itemLocalChange)
				return true, ""
			}
			if !errors.Is(err, syscall.EXDEV) && !errors.Is(err, syscall.EMLINK) {
				rv.s.sendError("link %q => %q failed: %s", name, p, errnoText(err))
				return true, ""
			}
			fallthrough // another file system: copy instead
		case BasisCopy:
			if !o.DryRun {
				dst := filepath.Join(rv.root.Name(), name)
				if err := copyEntry(p, dst, fi); err != nil {
					rv.s.sendError("copy %q => %q failed: %s", p, name, errnoText(err))
					return true, ""
				}
				if nfi, err := rv.root.Lstat(name); err == nil {
					rv.setAttrs(name, f, nfi, true)
				}
			}
			rv.itemize(ndx, f, itemLocalChange)
			return true, ""
		}
	}
	return false, basis
}

// unchangedAt is the quick check against a file outside the destination.
func (rv *receiving) unchangedAt(f *flist.File, p string, fi fs.FileInfo) bool {
	o := rv.o
	if fi.Size() != f.Size {
		return false
	}
	switch {
	case o.Checksum:
		sum, err := fileChecksum(p, rv.s.fileSum)
		return err == nil && bytes.Equal(sum, f.Checksum)
	case o.SizeOnly:
		return true
	case o.IgnoreTimes:
		return false
	}
	return sameTime(fi.ModTime(), f, o.ModifyWindow)
}

// sameAttrs reports whether a file to hard-link already has the
// attributes the transfer preserves, since a link cannot change them
// without changing the basis too.
func (rv *receiving) sameAttrs(f *flist.File, fi fs.FileInfo) bool {
	o := rv.o
	if o.Perms && wireMode(fi.Mode())&0o7777 != f.Perm() {
		return false
	}
	if o.Times && !sameTime(fi.ModTime(), f, o.ModifyWindow) {
		return false
	}
	sys := statSys(fi)
	if o.Owner && isRoot() && sys.uid != rv.uid(f) {
		return false
	}
	if o.Group && sys.gid != rv.gid(f) {
		return false
	}
	return true
}
