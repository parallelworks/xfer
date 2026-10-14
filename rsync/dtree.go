// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package rsync

import (
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
	tick uint64
}

// dirCacheSize bounds the open subdirectories. Files arrive grouped by
// directory, so a few entries cover nearly all lookups.
const dirCacheSize = 16

type dirEnt struct {
	r       *os.Root
	refs    int
	used    uint64
	evicted bool
}

func newDtree(root *os.Root) *dtree {
	return &dtree{root: root, dirs: make(map[string]*dirEnt)}
}

// at returns the root to resolve name's base name in, and a release
// function to call when done with it.
func (t *dtree) at(name string) (*os.Root, string, func()) {
	dir, base := path.Split(name)
	dir = strings.TrimSuffix(dir, "/")
	if dir == "" || dir == "." || base == "" || base == "." || base == ".." {
		return t.root, name, func() {}
	}
	t.mu.Lock()
	e := t.dirs[dir]
	if e == nil {
		t.mu.Unlock()
		r, err := t.root.OpenRoot(dir)
		if err != nil {
			return t.root, name, func() {} // let the operation report the error
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
	return e.r, base, func() {
		t.mu.Lock()
		e.refs--
		if e.refs == 0 && e.evicted {
			e.r.Close()
		}
		t.mu.Unlock()
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
		e.r.Close()
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
	r, base, done := t.at(name)
	defer done()
	return r.Open(base)
}

func (t *dtree) OpenFile(name string, flag int, perm fs.FileMode) (*os.File, error) {
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
	if od, nd := path.Dir(oldname), path.Dir(newname); od == nd {
		r, base, done := t.at(newname)
		defer done()
		if r != t.root {
			return r.Rename(path.Base(oldname), base)
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
