// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package rsync

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"syscall"
)

// backupPath is where the backup of name goes: beside it with the suffix,
// or below --backup-dir, which is relative to the destination unless it is
// absolute (abs is set).
func (rv *receiving) backupPath(name string) (p string, abs bool) {
	o := rv.o
	suffix := o.BackupSuffix()
	switch {
	case o.BackupDir == "":
		return name + suffix, false
	case filepath.IsAbs(o.BackupDir):
		return filepath.Join(o.BackupDir, name) + suffix, true
	case !filepath.IsLocal(o.BackupDir):
		// Relative to the destination but outside it.
		return filepath.Join(rv.root.Name(), o.BackupDir, name) + suffix, true
	}
	return path.Join(o.BackupDir, name) + suffix, false
}

// backupExisting backs up the entry at name, if there is one, before it is
// replaced.
func (rv *receiving) backupExisting(name string) error {
	fi, err := rv.root.Lstat(name)
	if err != nil {
		return nil
	}
	return rv.backup(name, fi)
}

// backup moves the entry at name out of the way into its backup. A
// directory's contents are backed up one by one and the directory is then
// removed if that left it empty.
func (rv *receiving) backup(name string, fi fs.FileInfo) error {
	if fi.IsDir() {
		d, err := rv.root.Open(name)
		if err != nil {
			return err
		}
		entries, err := d.ReadDir(-1)
		d.Close()
		if err != nil {
			return err
		}
		for _, e := range entries {
			child := path.Join(name, e.Name())
			if cfi, err := rv.root.Lstat(child); err == nil {
				if err := rv.backup(child, cfi); err != nil {
					return err
				}
			}
		}
		if err := rv.root.Remove(name); err != nil && rv.o.BackupDir != "" {
			return err
		}
		return nil
	}
	p, abs := rv.backupPath(name)
	if !abs {
		if dir := path.Dir(p); dir != "." {
			if err := rv.root.MkdirAll(dir, 0o777); err != nil {
				return err
			}
		}
		if bfi, err := rv.root.Lstat(p); err == nil && bfi.IsDir() {
			rv.root.RemoveAll(p)
		}
		return rv.root.Rename(name, p)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o777); err != nil {
		return err
	}
	if bfi, err := os.Lstat(p); err == nil && bfi.IsDir() {
		os.RemoveAll(p)
	}
	src := filepath.Join(rv.root.Name(), name)
	err := os.Rename(src, p)
	if !errors.Is(err, syscall.EXDEV) {
		return err
	}
	// Another file system: copy, then remove the original.
	if err := copyEntry(src, p, fi); err != nil {
		return err
	}
	return rv.root.Remove(name)
}

// backupCopy backs up name by copying it, for --inplace, which updates the
// file itself.
func (rv *receiving) backupCopy(name string) error {
	fi, err := rv.root.Lstat(name)
	if err != nil {
		return nil
	}
	p, abs := rv.backupPath(name)
	if !abs {
		if dir := path.Dir(p); dir != "." {
			if err := rv.root.MkdirAll(dir, 0o777); err != nil {
				return err
			}
		}
		p = filepath.Join(rv.root.Name(), p)
	} else if err := os.MkdirAll(filepath.Dir(p), 0o777); err != nil {
		return err
	}
	return copyEntry(filepath.Join(rv.root.Name(), name), p, fi)
}

// copyEntry copies a regular file or symlink from src to dst, keeping its
// permissions and modification time.
func copyEntry(src, dst string, fi fs.FileInfo) error {
	os.Remove(dst)
	if fi.Mode()&fs.ModeSymlink != 0 {
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)
	}
	if !fi.Mode().IsRegular() {
		return nil // devices and specials are not worth keeping
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chtimes(dst, fi.ModTime(), fi.ModTime())
}
