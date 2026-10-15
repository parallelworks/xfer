// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package rsync

import (
	"github.com/parallelworks/xfer/rsync/internal/flist"
	"github.com/parallelworks/xfer/rsync/internal/wire"
)

// Incremental recursion, sending side: after the first list, every
// directory gets a list of its contents, sent in depth-first pre-order
// while files are being transferred.

// minLookahead is how many entries the sender keeps listed beyond the
// oldest list still in use, so the receiving side always has work.
const minLookahead = 1000

// dirNode is a directory whose list is still to be sent.
type dirNode struct {
	f    *flist.File
	ndx  int32         // index in the directory table
	dups []*flist.File // directories of the same name whose contents join this list
}

// expander walks the directory tree in the order the lists are sent.
type expander struct {
	sc    *scanner
	lists *fileLists
	stack []*frame
}

type frame struct {
	nodes []*dirNode
	i     int
	leave func() // leaves the filter scope of the directory the nodes are in
}

func newExpander(sc *scanner, lists *fileLists, first *fileList) *expander {
	e := &expander{sc: sc, lists: lists}
	e.push(first, 0, nil)
	return e
}

// push adds a frame for the subdirectories of list l, whose directories
// start at index base of the directory table.
func (e *expander) push(l *fileList, base int32, leave func()) {
	var nodes []*dirNode
	ndx := base
	for _, f := range l.files {
		if !f.IsDir() {
			continue
		}
		i := ndx
		ndx++
		if f.Name == "." {
			continue // its contents are already listed
		}
		if n := len(nodes); n > 0 && nodes[n-1].f.Name == f.Name {
			// A duplicate from another source argument shares the list.
			nodes[n-1].dups = append(nodes[n-1].dups, f)
			continue
		}
		nodes = append(nodes, &dirNode{f: f, ndx: i})
	}
	e.stack = append(e.stack, &frame{nodes: nodes, leave: leave})
}

// next returns the next directory to list, or nil when all are done.
func (e *expander) next() *dirNode {
	for len(e.stack) > 0 {
		top := e.stack[len(e.stack)-1]
		if top.i < len(top.nodes) {
			n := top.nodes[top.i]
			top.i++
			return n
		}
		if top.leave != nil {
			top.leave()
		}
		e.stack = e.stack[:len(e.stack)-1]
	}
	return nil
}

// sendNextList sends the list of the next directory, or NDX_FLIST_EOF when
// none is left. It reports the number of entries sent.
func (s *session) sendNextList() (int, error) {
	n := s.exp.next()
	if n == nil {
		s.exp.sc.close()
		s.out.Lock()
		s.out.W.Ndx(wire.NdxFlistEOF)
		s.out.Unlock()
		s.lists.setEOF()
		return 0, nil
	}
	sc := s.exp.sc
	var leave func()
	if n.f.Flags&flist.FlagContentDir != 0 {
		sc.rootDev = n.f.RootDev
		leave = sc.walkDir(n.f.Source, n.f.Name)
		for _, d := range n.dups {
			if l := sc.walkDir(d.Source, n.f.Name); l != nil {
				l() // GNU keeps only the last duplicate's scope; ours are equal
			}
		}
	}
	files := sc.take()

	s.out.Lock()
	s.out.W.Ndx(wire.NdxFlistOffset - n.ndx)
	s.enc.StartList(s.lists.next)
	err := s.encodeList(files)
	s.out.Unlock()
	if err != nil {
		return 0, err
	}
	flist.Sort(files, s.proto)
	base := int32(s.lists.numDirs())
	l := s.lists.add(files, n.ndx)
	s.countFiles(files)
	s.exp.push(l, base, leave)
	return len(files), nil
}

// sendLookahead sends lists until enough entries are listed ahead of the
// oldest list in use. At least one list beyond it (or the end marker) is
// required: the generator does not finish the list it is working on until
// it has seen the next one.
func (s *session) sendLookahead() error {
	for !s.lists.done() && (s.lists.unfreed() < 2 || s.lists.entriesAfterOldest() < minLookahead) {
		if _, err := s.sendNextList(); err != nil {
			return err
		}
	}
	return nil
}

// sendFirstExtra follows a first list: with no directories to list the
// end marker follows at once, and after a first list of one entry lists are sent
// until another entry is listed, because a receiver reads that far before
// it chooses its destination. Like rsync, it leaves the output buffered:
// the lists go out together when the sender waits for the receiver.
func (s *session) sendFirstExtra(files []*flist.File) error {
	more := len(files) == 1 || len(s.exp.stack[0].nodes) == 0
	for more && !s.lists.done() {
		n, err := s.sendNextList()
		if err != nil {
			return err
		}
		if n > 0 {
			break
		}
	}
	return nil
}
