// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package rsync

import (
	"errors"
	"golang.org/x/sys/unix"
	"io/fs"
	"os"
	"path"
	"strings"
	"sync"
	"time"
)

// dtree is the destination tree. It offers the os.Root operations the
// receiver uses, but resolves names through a small cache of open
// subdirectories: os.Root walks a path one component at a time, so without
// the cache every operation on dir/file costs a directory open and close.
// The cache is safe for concurrent use by the generator and receiver.
type dtree struct {
	root *os.Root

	mu   sync.Mutex
	dirs map[string]*dirEnt
	top  dirEnt // the root directory itself
	tick uint64
}

// dirCacheSize bounds the open subdirectories. Files arrive grouped by
// directory, so a few entries cover nearly all lookups.
const dirCacheSize = 16

type dirEnt struct {
	r       *os.Root
	f       *os.File // the directory itself, opened on first use
	refs    int
	used    uint64
	evicted bool
}

func newDtree(root *os.Root) *dtree {
	return &dtree{root: root, dirs: make(map[string]*dirEnt), top: dirEnt{r: root}}
}

// at returns the root to resolve name's base name in, and a release
// function to call when done with it.
func (t *dtree) at(name string) (*os.Root, string, func()) {
	e, base, done := t.entry(name)
	return e.r, base, done
}

// entry is at, returning the cache entry.
func (t *dtree) entry(name string) (*dirEnt, string, func()) {
	dir, base := path.Split(name)
	dir = strings.TrimSuffix(dir, "/")
	if base == "" || base == "." || base == ".." {
		return &dirEnt{r: t.root}, name, func() {}
	}
	if dir == "" || dir == "." {
		return &t.top, base, func() {}
	}
	t.mu.Lock()
	e := t.dirs[dir]
	if e == nil {
		t.mu.Unlock()
		r, err := t.root.OpenRoot(dir)
		if err != nil {
			return &dirEnt{r: t.root}, name, func() {} // let the operation report the error
		}
		t.mu.Lock()
		if e = t.dirs[dir]; e != nil {
			r.Close() // another goroutine got there first
		} else {
			e = &dirEnt{r: r}
			t.dirs[dir] = e
		}
	}
	t.tick++
	e.used = t.tick
	e.refs++
	t.evictLocked()
	t.mu.Unlock()
	return e, base, func() {
		t.mu.Lock()
		e.refs--
		if e.refs == 0 && e.evicted {
			e.close()
		}
		t.mu.Unlock()
	}
}

// dirFD runs fn with a descriptor for the directory holding name and
// name's base name, which fn may use with *at system calls.
func (t *dtree) dirFD(name string, fn func(fd int, base string) error) error {
	e, base, done := t.entry(name)
	defer done()
	if strings.Contains(base, "/") || base == "" || base == "." || base == ".." {
		return errors.ErrUnsupported // not resolved through a cached directory
	}
	t.mu.Lock()
	f := e.f
	t.mu.Unlock()
	if f == nil {
		nf, err := e.r.Open(".")
		if err != nil {
			return err
		}
		t.mu.Lock()
		if e.f == nil {
			e.f = nf
		} else {
			nf.Close()
		}
		f = e.f
		t.mu.Unlock()
	}
	sc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ferr error
	if err := sc.Control(func(fd uintptr) { ferr = fn(int(fd), base) }); err != nil {
		return err
	}
	return ferr
}

func (e *dirEnt) close() {
	e.r.Close()
	if e.f != nil {
		e.f.Close()
	}
}

func (t *dtree) evictLocked() {
	for len(t.dirs) > dirCacheSize {
		var oldest string
		var min uint64 = ^uint64(0)
		for k, e := range t.dirs {
			if e.used < min {
				oldest, min = k, e.used
			}
		}
		t.dropLocked(oldest)
	}
}

func (t *dtree) dropLocked(k string) {
	e := t.dirs[k]
	delete(t.dirs, k)
	e.evicted = true
	if e.refs == 0 {
		e.close()
	}
}

// forget drops cached directories at or below name, which is about to be
// removed or replaced.
func (t *dtree) forget(name string) {
	name = strings.TrimSuffix(path.Clean(name), "/")
	t.mu.Lock()
	for k := range t.dirs {
		if name == "." || k == name || strings.HasPrefix(k, name+"/") {
			t.dropLocked(k)
		}
	}
	t.mu.Unlock()
}

// Close closes the tree and every cached directory.
func (t *dtree) Close() error {
	t.forget(".")
	if t.top.f != nil {
		t.top.f.Close()
	}
	return t.root.Close()
}

// Name returns the path the tree was opened with.
func (t *dtree) Name() string { return t.root.Name() }

func (t *dtree) Lstat(name string) (fs.FileInfo, error) {
	r, base, done := t.at(name)
	defer done()
	return r.Lstat(base)
}

func (t *dtree) Stat(name string) (fs.FileInfo, error) {
	r, base, done := t.at(name)
	defer done()
	return r.Stat(base)
}

func (t *dtree) Open(name string) (*os.File, error) {
	return t.OpenFile(name, os.O_RDONLY, 0)
}

// OpenFile opens name without following a final symlink, through the
// cached directory when it can; a symlink is then opened through os.Root,
// which keeps it inside the tree.
func (t *dtree) OpenFile(name string, flag int, perm fs.FileMode) (*os.File, error) {
	var f *os.File
	err := t.dirFD(name, func(fd int, base string) error {
		var err error
		f, err = openatFast(fd, base, flag|unix.O_NOFOLLOW, perm)
		return err
	})
	if err == nil {
		return f, nil
	}
	if !errors.Is(err, errors.ErrUnsupported) && !errors.Is(err, unix.ELOOP) {
		return nil, err
	}
	r, base, done := t.at(name)
	defer done()
	return r.OpenFile(base, flag, perm)
}

func (t *dtree) Chmod(name string, mode fs.FileMode) error {
	r, base, done := t.at(name)
	defer done()
	return r.Chmod(base, mode)
}

func (t *dtree) Chtimes(name string, atime, mtime time.Time) error {
	r, base, done := t.at(name)
	defer done()
	return r.Chtimes(base, atime, mtime)
}

func (t *dtree) Lchown(name string, uid, gid int) error {
	r, base, done := t.at(name)
	defer done()
	return r.Lchown(base, uid, gid)
}

func (t *dtree) Mkdir(name string, perm fs.FileMode) error {
	r, base, done := t.at(name)
	defer done()
	return r.Mkdir(base, perm)
}

func (t *dtree) MkdirAll(name string, perm fs.FileMode) error {
	return t.root.MkdirAll(name, perm)
}

func (t *dtree) Readlink(name string) (string, error) {
	r, base, done := t.at(name)
	defer done()
	return r.Readlink(base)
}

func (t *dtree) Symlink(target, name string) error {
	r, base, done := t.at(name)
	defer done()
	return r.Symlink(target, base)
}

func (t *dtree) Remove(name string) error {
	t.forget(name)
	r, base, done := t.at(name)
	defer done()
	return r.Remove(base)
}

func (t *dtree) RemoveAll(name string) error {
	t.forget(name)
	return t.root.RemoveAll(name)
}

// Rename and Link use the cached directory when both names share it, the
// common case of a temp file renamed into place.
func (t *dtree) Rename(oldname, newname string) error {
	t.forget(oldname)
	t.forget(newname)
	if path.Dir(oldname) == path.Dir(newname) {
		ob := path.Base(oldname)
		err := t.dirFD(newname, func(fd int, base string) error {
			return renameat(fd, ob, base)
		})
		if !errors.Is(err, errors.ErrUnsupported) {
			if err != nil {
				return &os.LinkError{Op: "rename", Old: oldname, New: newname, Err: err}
			}
			return nil
		}
	}
	return t.root.Rename(oldname, newname)
}

func (t *dtree) Link(oldname, newname string) error {
	if path.Dir(oldname) == path.Dir(newname) {
		r, base, done := t.at(newname)
		defer done()
		if r != t.root {
			return r.Link(path.Base(oldname), base)
		}
	}
	return t.root.Link(oldname, newname)
}
