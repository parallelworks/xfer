// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package rsync

import (
	"fmt"
	"sync"

	"github.com/parallelworks/xfer/rsync/internal/flist"
	"github.com/parallelworks/xfer/rsync/internal/wire"
)

// fileList is one file list. Without incremental recursion there is a
// single list holding everything. With it, the first list holds the top
// level of the transfer and every directory later gets a list of its own
// with its immediate contents.
type fileList struct {
	start  int32         // global index of files[0]
	files  []*flist.File // sorted
	parent int32         // index in dirs of the directory these entries are in, or -1
	seq    int           // 0 for the first list, then 1, 2, ...

	// Generator bookkeeping (incremental recursion): transfers requested
	// and not yet finished, and transfers waiting to be redone.
	inProgress, toRedo int
}

// gap is the global index naming the list's parent directory: the index
// just before the list's first entry.
func (l *fileList) gap() int32 { return l.start - 1 }

// fileLists holds the file lists of a session that are still in use, in
// the order they were sent, and the table of every directory seen.
type fileLists struct {
	inc bool

	mu    sync.Mutex
	cond  sync.Cond
	lists []*fileList   // unfreed lists, oldest first
	dirs  []*flist.File // every directory of every list, the protocol's dir_flist
	total int           // entries in all lists ever added
	next  int32         // start of the next list
	eof   bool          // no more lists will be added
	added int           // lists ever added
	byNum map[int]*fileList

	// notify receives a value when a list is added or the lists end.
	notify chan struct{}
}

func newFileLists(inc bool) *fileLists {
	fl := &fileLists{inc: inc, byNum: map[int]*fileList{}, notify: make(chan struct{}, 1)}
	if inc {
		fl.next = 1
	}
	fl.cond.L = &fl.mu
	return fl
}

// add appends a sorted list of entries for the directory with index parent
// (-1 for none) and returns it. It records the list's directories.
func (fl *fileLists) add(files []*flist.File, parent int32) *fileList {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	l := &fileList{start: fl.next, files: files, parent: parent, seq: fl.added}
	fl.next += int32(len(files))
	if fl.inc {
		fl.next++ // the next list's gap index
	}
	for _, f := range files {
		if f.IsDir() {
			fl.dirs = append(fl.dirs, f)
		}
	}
	fl.lists = append(fl.lists, l)
	fl.byNum[fl.added] = l
	fl.added++
	fl.total += len(files)
	fl.cond.Broadcast()
	fl.poke()
	return l
}

func (fl *fileLists) poke() {
	select {
	case fl.notify <- struct{}{}:
	default:
	}
}

// setEOF records that no more lists will be added.
func (fl *fileLists) setEOF() {
	fl.mu.Lock()
	fl.eof = true
	fl.cond.Broadcast()
	fl.mu.Unlock()
	fl.poke()
}

// arrived returns the list with sequence number n, or nil and whether more
// lists may still come.
func (fl *fileLists) arrived(n int) (l *fileList, more bool) {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	if n < fl.added {
		return fl.byNum[n], true
	}
	return nil, !fl.eof
}

// done reports whether no more lists will be added.
func (fl *fileLists) done() bool {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	return fl.eof
}

// wait returns the list with sequence number n (0 for the first list),
// waiting for it to arrive, or nil once no more lists will come. The list
// must not have been freed.
func (fl *fileLists) wait(n int) *fileList {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	for n >= fl.added && !fl.eof {
		fl.cond.Wait()
	}
	if n >= fl.added {
		return nil
	}
	return fl.byNum[n]
}

// list returns the list with sequence number n if it has arrived.
func (fl *fileLists) list(n int) *fileList {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	return fl.byNum[n]
}

// count returns the number of lists added so far.
func (fl *fileLists) count() int {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	return fl.added
}

// lookup returns the entry with global index ndx and its list. The gap
// index of a list names the list's parent directory.
func (fl *fileLists) lookup(ndx int32) (*flist.File, *fileList, error) {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	for _, l := range fl.lists {
		if ndx >= l.start && ndx < l.start+int32(len(l.files)) {
			return l.files[ndx-l.start], l, nil
		}
		if fl.inc && ndx == l.gap() && l.parent >= 0 {
			return fl.dirs[l.parent], l, nil
		}
	}
	return nil, nil, fmt.Errorf("%w: invalid file index %d", wire.ErrProtocol, ndx)
}

// dir returns the directory with index i in the directory table.
func (fl *fileLists) dir(i int32) *flist.File {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	if i < 0 || int(i) >= len(fl.dirs) {
		return nil
	}
	return fl.dirs[i]
}

// numDirs returns the size of the directory table.
func (fl *fileLists) numDirs() int {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	return len(fl.dirs)
}

// oldest returns the oldest unfreed list, or nil.
func (fl *fileLists) oldest() *fileList {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	if len(fl.lists) == 0 {
		return nil
	}
	return fl.lists[0]
}

// freeOldest drops the oldest unfreed list and reports how many remain.
func (fl *fileLists) freeOldest() int {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	if len(fl.lists) > 0 {
		l := fl.lists[0]
		fl.lists[0] = nil
		fl.lists = fl.lists[1:]
		delete(fl.byNum, l.seq)
	}
	return len(fl.lists)
}

// unfreed returns the number of lists not yet freed.
func (fl *fileLists) unfreed() int {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	return len(fl.lists)
}

// entriesAfterOldest counts the entries in unfreed lists other than the
// oldest, the sender's lookahead.
func (fl *fileLists) entriesAfterOldest() int {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	n := 0
	for i := 1; i < len(fl.lists); i++ {
		n += len(fl.lists[i].files)
	}
	return n
}

// all returns every entry of every unfreed list, in index order.
func (fl *fileLists) all() []*flist.File {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	var out []*flist.File
	for _, l := range fl.lists {
		out = append(out, l.files...)
	}
	return out
}
