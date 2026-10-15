// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package rsync

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/parallelworks/xfer/rsync/internal/checksum"
	"github.com/parallelworks/xfer/rsync/internal/delta"
	"github.com/parallelworks/xfer/rsync/internal/filter"
	"github.com/parallelworks/xfer/rsync/internal/flist"
	"github.com/parallelworks/xfer/rsync/internal/token"
	"github.com/parallelworks/xfer/rsync/internal/wire"
)

// receiving is the state shared by the generator and the receiver.
type receiving struct {
	bw      *bufio.Writer // reused across files
	s       *session
	o       *Options
	files   []*flist.File
	byName  map[string]int // active entries by name, for deletion
	filters *filter.List

	root   *dtree // destination; nil in a dry run whose destination does not exist
	single string // when copying one file onto a file name: that name
	umask  uint32

	// ids maps the sender's uids and gids to ours.
	uidMap, gidMap map[uint32]uint32

	events chan recvEvent // receiver → generator

	// With incremental recursion: directories this side created before
	// their own list arrived, which are still reported as new.
	precreated map[string]bool

	mu        sync.Mutex
	sent      map[int32]bool  // files the receiver has seen once (for redo)
	names     map[[2]any]bool // sender id names already mapped
	deleted   [5]int32        // files, dirs, symlinks, devices, specials
	deletes   int
	delLimit  bool
	followers []int // hard-link followers to link after transfers

	delayed []string // --delay-updates: files waiting in the partial directory
}

// recvEvent is the receiver telling the generator something: a file to
// redo, that it finished a phase, or (with incremental recursion) that a
// transfer the generator requested is over.
type recvEvent struct {
	redo int32
	ack  bool
	done bool
	list *fileList // the list of the file redone or done
}

// receiveFiles runs the generator and the receiver until the transfer is
// done.
func (s *session) receiveFiles(files []*flist.File, users, groups map[uint32]string, dest string, filters *filter.List) error {
	rv := &receiving{
		s: s, o: s.opts, files: files, filters: filters,
		events:     make(chan recvEvent, 1024),
		sent:       map[int32]bool{},
		umask:      currentUmask(),
		precreated: map[string]bool{},
		names:      map[[2]any]bool{},
	}
	if !s.opts.ListOnly {
		if err := rv.openDest(dest); err != nil {
			return err
		}
	}
	if rv.root != nil {
		defer rv.root.Close()
	}
	if !s.inc {
		rv.byName = make(map[string]int, len(files))
		for i, f := range files {
			if f.Flags&flist.FlagCleared == 0 {
				rv.byName[f.Name] = i
			}
		}
	}
	rv.uidMap, rv.gidMap = map[uint32]uint32{}, map[uint32]uint32{}
	rv.mapIDs(users, groups)
	if s.inc {
		// Names arrive inline with the lists; the receiver goroutine
		// decodes them.
		s.onSubList = func(*fileList) { rv.mapIDs(s.dec.Users, s.dec.Groups) }
		s.onNoSend = func(ndx int32) {
			if _, l, err := s.lists.lookup(ndx); err == nil {
				rv.events <- recvEvent{done: true, list: l}
			}
		}
	}

	genErr := make(chan error, 1)
	go func() {
		if s.inc {
			genErr <- rv.generateInc()
		} else {
			genErr <- rv.generate()
		}
	}()
	recvErr := rv.receive()
	if recvErr != nil {
		close(rv.events)
		// Unblock a generator waiting on output.
		s.out.TryFlush()
		select {
		case <-genErr:
		case <-time.After(100 * time.Millisecond):
		}
		return recvErr
	}
	return <-genErr
}

// openDest resolves the destination argument against the file list.
func (rv *receiving) openDest(dest string) error {
	o := rv.o
	active := 0
	var only *flist.File
	for _, f := range rv.files {
		if f.Flags&flist.FlagCleared == 0 {
			active++
			only = f
		}
	}
	if dest == "" {
		dest = "."
	}
	fi, err := os.Stat(dest)
	exists := err == nil
	if active == 1 && !only.IsDir() && !strings.HasSuffix(dest, "/") && (!exists || !fi.IsDir()) {
		// Copying a single file to a file name.
		dir, base := path.Split(dest)
		if dir == "" {
			dir = "."
		}
		if o.MkPath && !o.DryRun {
			if err := os.MkdirAll(dir, 0o777); err != nil {
				return exitErrorf(ExitFileIO, "mkdir %q failed: %s", dir, errnoText(err))
			}
		}
		rv.single = base
		root, err := os.OpenRoot(dir)
		if err != nil {
			if o.DryRun {
				return nil
			}
			return exitErrorf(ExitFileIO, "change_dir#3 %q failed: %s", absPath(dir), errnoText(err))
		}
		rv.root = newDtree(root)
		return nil
	}
	if exists && !fi.IsDir() {
		return exitErrorf(ExitFileIO, "ERROR: destination must be a directory when copying more than 1 file")
	}
	if !exists {
		if o.DryRun {
			return nil
		}
		mkdir := os.Mkdir
		if o.MkPath {
			mkdir = os.MkdirAll
		}
		if err := mkdir(dest, 0o777); err != nil {
			return exitErrorf(ExitFileIO, "mkdir %q failed: %s", dest, errnoText(err))
		}
		if !rv.s.server && o.Verbose > 0 {
			rv.s.log.print("created directory " + dest + "\n")
		}
	}
	root, err := os.OpenRoot(dest)
	if err != nil {
		return exitErrorf(ExitFileIO, "change_dir#3 %q failed: %s", absPath(dest), errnoText(err))
	}
	rv.root = newDtree(root)
	return nil
}

// localName maps a file-list name to its path below the destination root.
func (rv *receiving) localName(f *flist.File) string {
	if rv.single != "" {
		return rv.single
	}
	name := strings.TrimLeft(f.Name, "/")
	if name == "" {
		return "."
	}
	return name
}

func (rv *receiving) lstat(name string) (fs.FileInfo, error) {
	if rv.root == nil {
		return nil, fs.ErrNotExist
	}
	return rv.root.Lstat(name)
}

// mapIDs maps the sender's ids to local ids by name, as rsync does unless
// --numeric-ids is given. With incremental recursion it is called again as
// names arrive; ids already mapped are skipped.
func (rv *receiving) mapIDs(users, groups map[uint32]string) {
	if rv.o.NumericIDs {
		return
	}
	root := isRoot()
	for id, name := range users {
		if !root || rv.names[[2]any{"u", id}] {
			continue
		}
		rv.names[[2]any{"u", id}] = true
		if u, err := user.Lookup(name); err == nil {
			if n, err := strconv.ParseUint(u.Uid, 10, 32); err == nil {
				rv.mu.Lock()
				rv.uidMap[id] = uint32(n)
				rv.mu.Unlock()
			}
		}
	}
	for id, name := range groups {
		if rv.names[[2]any{"g", id}] {
			continue
		}
		rv.names[[2]any{"g", id}] = true
		if g, err := user.LookupGroup(name); err == nil {
			if n, err := strconv.ParseUint(g.Gid, 10, 32); err == nil {
				rv.mu.Lock()
				rv.gidMap[id] = uint32(n)
				rv.mu.Unlock()
			}
		}
	}
}

func (rv *receiving) uid(f *flist.File) uint32 {
	rv.mu.Lock()
	defer rv.mu.Unlock()
	if v, ok := rv.uidMap[f.UID]; ok {
		return v
	}
	return f.UID
}

func (rv *receiving) gid(f *flist.File) uint32 {
	rv.mu.Lock()
	defer rv.mu.Unlock()
	if v, ok := rv.gidMap[f.GID]; ok {
		return v
	}
	return f.GID
}

// generator ----------------------------------------------------------------

func (rv *receiving) generate() error {
	o := rv.o
	if o.Delete != DeleteNone && rv.deleteMode() == DeleteBefore {
		rv.deletePass()
	}
	for i, f := range rv.files {
		if f.Flags&flist.FlagCleared != 0 {
			continue
		}
		if _, err := rv.genFile(int32(i), f, false); err != nil {
			return err
		}
	}
	return rv.phases(true)
}

// phases runs the generator's part of the transfer phases after the file
// lists were walked: the end of phase 0 (unless incremental recursion
// already ended it with the NDX_DONE of its last list), the redo phase and
// the goodbye.
func (rv *receiving) phases(endPhase0 bool) error {
	s, o := rv.s, rv.o
	w := s.out
	earlyDelay := !o.DelayUpdates
	deleting := o.Delete != DeleteNone
	mode := rv.deleteMode()
	earlyDelete := !(mode == DeleteDelay || mode == DeleteAfter)

	done := func() error {
		w.Lock()
		w.W.Ndx(wire.NdxDone)
		err := w.W.Flush()
		w.Unlock()
		return err
	}
	// waitAck processes redo requests until the receiver acknowledges
	// the end of a phase.
	waitAck := func() error {
		for ev := range rv.events {
			if ev.ack {
				return nil
			}
			if ev.done {
				continue
			}
			f, _, err := s.lists.lookup(ev.redo)
			if err != nil {
				return err
			}
			if _, err := rv.genFile(ev.redo, f, true); err != nil {
				return err
			}
			w.Flush()
		}
		return errReceiverStopped
	}
	delStats := func() {
		if deleting {
			w.Lock()
			s.writeDelStats(rv.deletedStats())
			w.Unlock()
		}
	}

	if endPhase0 {
		if err := done(); err != nil { // end of phase 0
			return err
		}
	}
	if err := waitAck(); err != nil { // receiver finished phase 0; redos sent
		return err
	}
	if err := done(); err != nil { // end of the redo phase
		return err
	}
	if s.proto >= 29 && earlyDelay {
		if err := done(); err != nil {
			return err
		}
	}
	if s.proto >= 31 && earlyDelete {
		delStats()
		if earlyDelay {
			if err := done(); err != nil {
				return err
			}
		}
	}
	if err := waitAck(); err != nil { // receiver finished the redo phase
		return err
	}
	rv.linkFollowers()
	if s.proto >= 29 {
		if !earlyDelay {
			done()
			if s.proto >= 31 && earlyDelete {
				done()
			}
		}
		if err := waitAck(); err != nil { // receiver's loop ended
			return err
		}
	}
	if deleting && !earlyDelete {
		rv.deletePass()
	}
	if s.proto >= 31 {
		if !earlyDelete {
			delStats()
			if err := done(); err != nil {
				return err
			}
		}
		if err := waitAck(); err != nil { // receiver read the goodbye echo
			return err
		}
	}
	rv.touchUpDirs()
	return done()
}

var errReceiverStopped = errors.New("receiver stopped")

func (rv *receiving) deleteMode() DeleteMode {
	m := rv.o.Delete
	if m == DeleteAuto {
		if rv.s.proto >= 30 {
			return DeleteDuring
		}
		return DeleteBefore
	}
	return m
}

// genFile decides what to do with one entry and, for a regular file that
// needs updating, sends the request to the sender. redo is set in the
// second pass, after a checksum mismatch.
func (rv *receiving) genFile(ndx int32, f *flist.File, redo bool) (requested bool, err error) {
	s, o := rv.s, rv.o
	if o.ListOnly {
		if !(f.IsDir() && !o.ImpliedDirs && f.Flags&flist.FlagImpliedDir != 0) && !s.server {
			s.log.print(listLine(f, o.HumanReadable))
		}
		return false, nil
	}
	name := rv.localName(f)
	fi, statErr := rv.lstat(name)
	exists := statErr == nil

	switch {
	case f.IsDir():
		return false, rv.genDir(ndx, f, name, fi, exists)
	case f.IsLink():
		return false, rv.genSymlink(ndx, f, name, fi, exists)
	case f.IsDevice(), f.IsSpecial():
		return false, rv.genSpecial(ndx, f, name, fi, exists)
	}

	// Regular file.
	if o.HardLinks && f.Flags&flist.FlagHlinked != 0 && !rv.isHlinkLeader(ndx, f) {
		rv.followers = append(rv.followers, int(ndx))
		return false, nil
	}
	if !redo {
		if o.Existing && !exists {
			return false, nil
		}
		if o.IgnoreExisting && exists && !fi.IsDir() {
			return false, nil
		}
		if (o.MaxSize > 0 && f.Size > o.MaxSize) || (o.MinSize > 0 && f.Size < o.MinSize) {
			return false, nil
		}
		if o.Update && exists && fi.Mode().IsRegular() && fi.ModTime().Unix()-f.ModTime > int64(o.ModifyWindow/time.Second) {
			return false, nil
		}
	}
	if exists && !fi.Mode().IsRegular() {
		if err := rv.deleteForReplace(name, fi); err != nil {
			s.sendError("delete of %q failed: %s", name, errnoText(err))
			return false, nil
		}
		exists = false
	}

	var iflags uint32
	if exists && !redo {
		if rv.unchanged(f, name, fi) {
			iflags = rv.itemFlags(f, fi, 0)
			rv.setAttrs(name, f, fi, false)
			rv.itemize(ndx, f, iflags)
			if o.RemoveSourceFiles {
				s.out.SendMsgInt(wire.MsgSuccess, ndx)
			}
			return false, nil
		}
	}

	iflags = itemTransfer
	if o.Checksum {
		iflags |= itemReportChange
	}
	if exists {
		iflags = rv.itemFlags(f, fi, iflags)
	} else {
		iflags |= itemIsNew
	}

	head := sumHead{}
	var sums []byte
	if !rv.wholeFile() && !o.DryRun {
		basis := rv.openBasis(name, exists)
		if basis != nil {
			if bfi, err := basis.Stat(); err == nil && bfi.Size() > 0 {
				head, sums, err = rv.makeSums(basis, bfi.Size(), redo)
				if err != nil {
					s.sendError("failed to read basis %q: %v", name, err)
					head, sums = sumHead{}, nil
				}
			}
			basis.Close()
		}
	}

	w := s.out
	w.Lock()
	defer w.Unlock()
	s.writeRequest(request{ndx: ndx, iflags: iflags})
	if !o.DryRun {
		s.writeSumHead(head)
		w.W.Write(sums)
	}
	return !o.DryRun, w.W.Err()
}

// wholeFile reports whether files are sent without the delta algorithm.
func (rv *receiving) wholeFile() bool {
	if rv.o.WholeFile != nil {
		return *rv.o.WholeFile
	}
	return rv.s.localTransfer
}

// makeSums computes the block checksums of the basis file.
func (rv *receiving) makeSums(file *os.File, size int64, redo bool) (sumHead, []byte, error) {
	s := rv.s
	blen, s2len := sumSizes(size, rv.o.BlockSize, s.proto, s.xferSum.Size(), redo)
	block := checksum.NewBlock(s.xferSum, s.seed, s.seedFirst)
	sums, err := delta.Generate(bufio.NewReaderSize(file, 256<<10), size, blen, s2len, block.Sum)
	if err != nil {
		return sumHead{}, nil, err
	}
	head := sumHead{count: int32(sums.Count()), blength: int32(blen), s2length: int32(s2len), remainder: int32(sums.Remainder)}
	out := make([]byte, 0, sums.Count()*(4+s2len))
	for i := range sums.Count() {
		w := sums.Weak[i]
		out = append(out, byte(w), byte(w>>8), byte(w>>16), byte(w>>24))
		out = append(out, sums.StrongSum(i)...)
	}
	return head, out, nil
}

// sumSizes picks the block length and strong checksum length for a basis
// of size bytes, following rsync's heuristic so the result is familiar.
func sumSizes(size int64, blockSize, proto, sumLen int, redo bool) (blen, s2len int) {
	const blockSizeDefault = 700
	maxS2 := min(16, sumLen)
	switch {
	case blockSize > 0:
		blen = blockSize
	case size <= blockSizeDefault*blockSizeDefault:
		blen = blockSizeDefault
	default:
		maxB := int64(1 << 17)
		if proto < 30 {
			maxB = 1 << 29
		}
		c := int64(1)
		for l := size; l>>2 != 0; l >>= 2 {
			c <<= 1
		}
		if c >= maxB || c <= 0 {
			blen = int(maxB)
		} else {
			var b int64
			for c >= 8 {
				b |= c
				if size < b*b {
					b &^= c
				}
				c >>= 1
			}
			blen = int(max(b, blockSizeDefault))
		}
	}
	if redo {
		return blen, maxS2
	}
	b := 10
	for l := size; l>>1 != 0; l >>= 1 {
		b += 2
	}
	for c := blen; c>>1 != 0 && b > 0; c >>= 1 {
		b--
	}
	s2len = (b + 1 - 32 + 7) / 8
	s2len = max(2, min(s2len, maxS2))
	return blen, s2len
}

// unchanged runs the quick check.
func (rv *receiving) unchanged(f *flist.File, name string, fi fs.FileInfo) bool {
	o := rv.o
	if fi.Size() != f.Size {
		return false
	}
	switch {
	case o.Checksum:
		sum, err := rv.destChecksum(name)
		return err == nil && bytes.Equal(sum, f.Checksum)
	case o.SizeOnly:
		return true
	case o.IgnoreTimes:
		return false
	}
	return sameTime(fi.ModTime(), f, o.ModifyWindow)
}

func (rv *receiving) destChecksum(name string) ([]byte, error) {
	if rv.root == nil {
		return nil, fs.ErrNotExist
	}
	return fileChecksum(path.Join(rv.root.Name(), name), rv.s.fileSum)
}

func sameTime(t time.Time, f *flist.File, window time.Duration) bool {
	switch {
	case window == 0:
		return t.Unix() == f.ModTime
	case window < 0:
		return t.Unix() == f.ModTime && int32(t.Nanosecond()) == f.ModNsec
	}
	d := t.Unix() - f.ModTime
	if d < 0 {
		d = -d
	}
	return d <= int64(window/time.Second)
}

// itemFlags compares an existing destination with its file-list entry.
func (rv *receiving) itemFlags(f *flist.File, fi fs.FileInfo, iflags uint32) uint32 {
	o := rv.o
	if f.IsRegular() && fi.Size() != f.Size {
		iflags |= itemReportSize
	}
	keepTime := o.Times && !(f.IsDir() && o.OmitDirTimes) && !(f.IsLink() && o.OmitLinkTimes)
	if keepTime {
		if !sameTime(fi.ModTime(), f, o.ModifyWindow) {
			iflags |= itemReportTime
		}
	} else if iflags&(itemTransfer|itemLocalChange) != 0 {
		iflags |= itemReportTime
	}
	if !f.IsLink() {
		if o.Perms && wireMode(fi.Mode())&0o7777 != f.Perm() {
			iflags |= itemReportPerms
		} else if !o.Perms && o.Executability && f.IsRegular() &&
			(fi.Mode().Perm()&0o111 != 0) != (f.Mode&0o111 != 0) {
			iflags |= itemReportPerms
		}
	}
	sys := statSys(fi)
	if o.Owner && isRoot() && sys.uid != rv.uid(f) {
		iflags |= itemReportOwner
	}
	if o.Group && sys.gid != rv.gid(f) {
		iflags |= itemReportGroup
	}
	return iflags
}

// itemize reports a non-transfer outcome: on the wire for protocol 29+, so
// the sender's side logs it, otherwise locally.
func (rv *receiving) itemize(ndx int32, f *flist.File, iflags uint32) {
	s := rv.s
	iflags &= 0xffff
	log := s.log
	want := iflags&(significantItemFlags|itemReportXattr) != 0 || rv.o.Verbose > 1 || log.hasI > 1
	if s.proto < 29 {
		if !s.server && log.shouldLog(f, iflags, "") {
			log.item(f, iflags, "")
		}
		return
	}
	if !want {
		return
	}
	s.out.Lock()
	s.writeRequest(request{ndx: ndx, iflags: iflags})
	s.out.Unlock()
}

func (rv *receiving) genDir(ndx int32, f *flist.File, name string, fi fs.FileInfo, exists bool) error {
	s, o := rv.s, rv.o
	if rv.precreated[name] {
		// Made early so its parent's time could be set; still new.
		delete(rv.precreated, name)
		if exists && fi.IsDir() {
			if !o.DryRun && rv.root != nil {
				rv.setAttrs(name, f, fi, true)
			}
			rv.itemize(ndx, f, itemLocalChange|itemIsNew)
			return nil
		}
	}
	if exists && !fi.IsDir() {
		if err := rv.deleteForReplace(name, fi); err != nil {
			s.sendError("delete of %q failed: %s", name, errnoText(err))
			return nil
		}
		exists = false
	}
	var iflags uint32
	if !exists {
		iflags = itemLocalChange | itemIsNew
		if !o.DryRun && rv.root != nil && name != "." {
			if err := rv.root.Mkdir(name, rv.newPerm(f)|0o700); err != nil && !errors.Is(err, fs.ErrExist) {
				s.sendError("recv_generator: mkdir %q failed: %s", name, errnoText(err))
				return nil
			}
			s.stats.NumCreated++
		}
	} else {
		iflags = rv.itemFlags(f, fi, 0)
	}
	if !o.DryRun && rv.root != nil {
		if nfi, err := rv.root.Lstat(name); err == nil {
			rv.setAttrs(name, f, nfi, false)
		}
	}
	rv.itemize(ndx, f, iflags)
	if !s.inc && o.Delete != DeleteNone && rv.deleteMode() == DeleteDuring && f.Flags&flist.FlagContentDir != 0 {
		rv.deleteInDir(name, f.Name, rv.inList)
	}
	return nil
}

// inList reports whether a wire name is in the (single) file list.
func (rv *receiving) inList(name string) bool {
	_, ok := rv.byName[name]
	return ok
}

func (rv *receiving) genSymlink(ndx int32, f *flist.File, name string, fi fs.FileInfo, exists bool) error {
	s, o := rv.s, rv.o
	if !o.Links {
		if !s.server || s.proto < 29 {
			s.info("skipping non-regular file \"" + f.Name + "\"\n")
		}
		return nil
	}
	if o.SafeLinks && unsafeLink(f.Link, f.Name) {
		s.info("ignoring unsafe symlink \"" + f.Name + "\" -> \"" + f.Link + "\"\n")
		return nil
	}
	if exists && fi.Mode()&fs.ModeSymlink != 0 {
		if target, err := rv.root.Readlink(name); err == nil && target == f.Link {
			iflags := rv.itemFlags(f, fi, 0)
			if !o.DryRun {
				rv.setLinkTime(name, f, fi)
			}
			rv.itemize(ndx, f, iflags)
			return nil
		}
	}
	iflags := uint32(itemLocalChange | itemReportChange)
	if !exists {
		iflags |= itemIsNew
	}
	if !o.DryRun && rv.root != nil {
		if exists {
			if err := rv.deleteForReplace(name, fi); err != nil {
				s.sendError("delete of %q failed: %s", name, errnoText(err))
				return nil
			}
		}
		if err := rv.root.Symlink(f.Link, name); err != nil {
			s.sendError("symlink %q -> %q failed: %s", name, f.Link, errnoText(err))
			return nil
		}
		s.stats.NumCreated++
		if o.Owner || o.Group {
			rv.chown(nil, name, f, nil, true)
		}
		rv.setLinkTime(name, f, nil)
	}
	rv.itemize(ndx, f, iflags)
	return nil
}

func (rv *receiving) setLinkTime(name string, f *flist.File, fi fs.FileInfo) {
	o := rv.o
	if !o.Times || o.OmitLinkTimes {
		return
	}
	if fi != nil && sameTime(fi.ModTime(), f, o.ModifyWindow) {
		return
	}
	lchtimes(rv.root, name, time.Unix(f.ModTime, int64(f.ModNsec)))
}

func (rv *receiving) genSpecial(ndx int32, f *flist.File, name string, fi fs.FileInfo, exists bool) error {
	s, o := rv.s, rv.o
	if (f.IsDevice() && !(o.Devices && isRoot())) || (f.IsSpecial() && !o.Specials) {
		if !s.server {
			s.info("skipping non-regular file \"" + f.Name + "\"\n")
		}
		return nil
	}
	if exists && wireMode(fi.Mode())&flist.TypeMask == f.Mode&flist.TypeMask {
		sys := statSys(fi)
		if !f.IsDevice() || (devMajor(sys.rdev) == f.RdevMajor && devMinor(sys.rdev) == f.RdevMinor) {
			iflags := rv.itemFlags(f, fi, 0)
			if !o.DryRun {
				rv.setAttrs(name, f, fi, false)
			}
			rv.itemize(ndx, f, iflags)
			return nil
		}
	}
	iflags := uint32(itemLocalChange | itemReportChange)
	if !exists {
		iflags |= itemIsNew
	}
	if !o.DryRun && rv.root != nil {
		if exists {
			if err := rv.deleteForReplace(name, fi); err != nil {
				s.sendError("delete of %q failed: %s", name, errnoText(err))
				return nil
			}
		}
		if err := mknod(rv.root, name, f); err != nil {
			s.sendError("mknod %q failed: %s", name, errnoText(err))
			return nil
		}
		s.stats.NumCreated++
		if nfi, err := rv.root.Lstat(name); err == nil {
			rv.setAttrs(name, f, nfi, true)
		}
	}
	rv.itemize(ndx, f, iflags)
	return nil
}

// newPerm is the permission a newly created entry gets.
func (rv *receiving) newPerm(f *flist.File) fs.FileMode {
	if rv.o.Perms {
		return goMode(f.Perm())
	}
	return goMode(f.Perm() &^ rv.umask & 0o777)
}

// setAttrs brings the permissions, ownership and modification time of an
// existing entry in line with f. isNew is set for entries just created.
func (rv *receiving) setAttrs(name string, f *flist.File, fi fs.FileInfo, isNew bool) {
	rv.setAttrsOf(nil, name, f, fi, isNew)
}

// setAttrsOf is setAttrs for a file that may be open as fh, in which case
// the attributes are set through its descriptor.
func (rv *receiving) setAttrsOf(fh *os.File, name string, f *flist.File, fi fs.FileInfo, isNew bool) {
	o := rv.o
	if rv.root == nil || o.DryRun {
		return
	}
	if o.Owner || o.Group {
		rv.chown(fh, name, f, fi, false)
	}
	cur := wireMode(fi.Mode()) & 0o7777
	want := cur
	switch {
	case o.Perms:
		want = f.Perm()
	case isNew:
		want = f.Perm() &^ rv.umask & 0o777
	case o.Executability && f.IsRegular():
		if f.Mode&0o111 != 0 && cur&0o111 == 0 {
			want = cur | (cur&0o444)>>2
		} else if f.Mode&0o111 == 0 && cur&0o111 != 0 {
			want = cur &^ 0o111
		}
	}
	if f.IsDir() {
		// Keep directories writable until touchUpDirs restores them.
		want |= 0o700
		if !o.Perms && !isNew {
			want = cur | 0o700
		}
	}
	if want != cur {
		if fh != nil {
			fh.Chmod(goMode(want))
		} else {
			rv.root.Chmod(name, goMode(want))
		}
	}
	if o.Times && !f.IsDir() && !sameTime(fi.ModTime(), f, -1) {
		mt := time.Unix(f.ModTime, int64(f.ModNsec))
		if fh == nil || futimes(fh, mt) != nil {
			rv.root.Chtimes(name, mt, mt)
		}
	}
}

func (rv *receiving) chown(fh *os.File, name string, f *flist.File, fi fs.FileInfo, link bool) {
	uid, gid := -1, -1
	var sys sysInfo
	if fi != nil {
		sys = statSys(fi)
	}
	if rv.o.Owner && isRoot() {
		if u := rv.uid(f); fi == nil || u != sys.uid {
			uid = int(u)
		}
	}
	if rv.o.Group {
		if g := rv.gid(f); fi == nil || g != sys.gid {
			gid = int(g)
		}
	}
	if uid == -1 && gid == -1 {
		return
	}
	// Failures are expected for groups we are not in.
	if fh != nil {
		fh.Chown(uid, gid)
	} else {
		rv.root.Lchown(name, uid, gid)
	}
}

// deleteForReplace removes an entry that is in the way of a different type.
func (rv *receiving) deleteForReplace(name string, fi fs.FileInfo) error {
	if rv.o.DryRun || rv.root == nil {
		return nil
	}
	if rv.o.backups() {
		return rv.backup(name, fi)
	}
	if fi.IsDir() {
		if rv.o.Force || rv.o.Delete != DeleteNone {
			return rv.root.RemoveAll(name)
		}
		return rv.root.Remove(name)
	}
	return rv.root.Remove(name)
}

// isHlinkLeader reports whether a hard-linked file is the one whose data
// is transferred.
func (rv *receiving) isHlinkLeader(ndx int32, f *flist.File) bool {
	if rv.s.proto >= 30 {
		return f.Flags&flist.FlagHlinkFirst != 0
	}
	for i, g := range rv.files {
		if g.Flags&flist.FlagHlinked != 0 && g.Dev == f.Dev && g.Ino == f.Ino && g.Flags&flist.FlagCleared == 0 {
			return int32(i) == ndx
		}
	}
	return true
}

// linkFollowers hard-links each follower to its leader once data is in
// place.
func (rv *receiving) linkFollowers() {
	if len(rv.followers) == 0 || rv.o.DryRun || rv.root == nil {
		return
	}
	leaderOf := map[[2]int64]*flist.File{}
	if rv.s.proto < 30 {
		for i, f := range rv.files {
			if f.Flags&flist.FlagHlinked != 0 && rv.isHlinkLeader(int32(i), f) {
				leaderOf[[2]int64{f.Dev, f.Ino}] = f
			}
		}
	}
	for _, i := range rv.followers {
		f := rv.files[i]
		var leader *flist.File
		if rv.s.proto >= 30 {
			leader = rv.s.dec.Leader(f.HLinkIndex)
		} else {
			leader = leaderOf[[2]int64{f.Dev, f.Ino}]
		}
		if leader == nil {
			continue
		}
		name, target := rv.localName(f), rv.localName(leader)
		if fi, err := rv.root.Lstat(name); err == nil {
			if tfi, err := rv.root.Lstat(target); err == nil && os.SameFile(fi, tfi) {
				continue
			}
			rv.root.Remove(name)
		}
		if err := rv.root.Link(target, name); err != nil {
			rv.s.sendError("link %q => %q failed: %s", name, target, errnoText(err))
			continue
		}
		if !rv.s.server && rv.s.log.format != "" {
			rv.s.log.item(f, itemLocalChange|itemXnameFollows, leader.Name)
		}
	}
}

// touchUpDirs sets directory permissions and times once their contents are
// final.
func (rv *receiving) touchUpDirs() {
	if rv.o.DryRun || rv.root == nil || rv.s.inc {
		return // with incremental recursion, done as each list finishes
	}
	for i := len(rv.files) - 1; i >= 0; i-- {
		f := rv.files[i]
		if f.IsDir() && f.Flags&flist.FlagCleared == 0 {
			rv.touchUpDir(f)
		}
	}
}

// touchUpDir sets a directory's final permissions and time.
func (rv *receiving) touchUpDir(f *flist.File) {
	o := rv.o
	if o.DryRun || rv.root == nil {
		return
	}
	name := rv.localName(f)
	fi, err := rv.root.Lstat(name)
	if err != nil || !fi.IsDir() {
		return
	}
	cur := wireMode(fi.Mode()) & 0o7777
	want := cur
	switch {
	case o.Perms:
		want = f.Perm()
	default:
		if rv.s.stats.NumCreated > 0 && cur&0o700 == 0o700 && f.Perm()&0o700 != 0o700 {
			// Directory we made writable on creation.
			want = (f.Perm() &^ rv.umask) & 0o7777
		}
	}
	if want != cur {
		rv.root.Chmod(name, goMode(want))
	}
	if o.Times && !o.OmitDirTimes && !sameTime(fi.ModTime(), f, -1) {
		mt := time.Unix(f.ModTime, int64(f.ModNsec))
		rv.root.Chtimes(name, mt, mt)
	}
}

// receiver ----------------------------------------------------------------

func (rv *receiving) ack() {
	select {
	case rv.events <- recvEvent{ack: true}:
	default:
		rv.events <- recvEvent{ack: true}
	}
}

func (rv *receiving) receive() error {
	s, o := rv.s, rv.o
	maxPhase := 1
	if s.proto >= 29 {
		maxPhase = 2
	}
	tr := token.NewReader(s.tokenKind(), s.in, s.proto)
	defer tr.Close()
	fileHash := checksum.NewFile(s.xferSum, s.seed)

	phase := 0
	for {
		req, err := s.readRequest()
		if err != nil {
			return err
		}
		if req.ndx == wire.NdxDone {
			// With incremental recursion the sender echoes one NDX_DONE
			// per finished list; the one for the last list ends phase 0.
			if s.inc && s.lists.unfreed() > 0 && s.lists.freeOldest() > 0 {
				continue
			}
			phase++
			if phase == 2 {
				// The redo phase is over: every update is complete.
				rv.finishDelayed()
			}
			if phase > maxPhase {
				break
			}
			rv.ack()
			continue
		}
		f, l, err := s.lists.lookup(req.ndx)
		if err != nil {
			return err
		}
		if req.iflags&itemTransfer == 0 {
			if !s.server && s.log.shouldLog(f, req.iflags, req.xname) {
				s.log.item(f, req.iflags, req.xname)
			}
			continue
		}
		if phase == 2 {
			return fmt.Errorf("%w: got transfer request in phase 2", wire.ErrProtocol)
		}
		if o.DryRun {
			if !s.server {
				s.log.item(f, req.iflags, req.xname)
			}
			s.stats.NumTransferred++
			s.stats.TotalTransferredSize += f.Size
			continue
		}
		redo, err := rv.receiveFile(req, f, l, tr, fileHash)
		if err != nil {
			return err
		}
		if s.inc && !redo {
			rv.events <- recvEvent{done: true, list: l}
		}
	}

	if !s.server && s.peerIsServerSender() {
		s.readStats()
	}
	rv.ack()
	if s.proto >= 31 {
		req, err := s.readRequest()
		if err != nil {
			return err
		}
		if req.ndx != wire.NdxDone {
			return fmt.Errorf("%w: expected goodbye, got index %d", wire.ErrProtocol, req.ndx)
		}
		rv.ack()
	}
	return nil
}

// receiveFile reconstructs one file from the sender's tokens.
func (rv *receiving) receiveFile(req request, f *flist.File, l *fileList, tr token.Reader, fileHash interface {
	io.Writer
	Sum([]byte) []byte
	Reset()
}) (redo bool, err error) {
	s, o := rv.s, rv.o
	head, err := s.readSumHead()
	if err != nil {
		return false, err
	}
	name := rv.localName(f)

	// The basis is the current destination file. With --inplace it is
	// also the output: the sender only references blocks we have not yet
	// overwritten.
	var basis *os.File
	inplace := o.Inplace && rv.root != nil
	if head.count > 0 && rv.root != nil && !inplace {
		basis = rv.openBasis(name, true)
		if basis != nil {
			defer basis.Close()
		}
	}

	var (
		tmpName string
		tmp     *os.File
		openErr error
	)
	if inplace {
		tmpName = name
		if o.backups() {
			if err := rv.backupCopy(name); err != nil {
				s.sendError("backup of %q failed: %s", name, errnoText(err))
			}
		}
		tmp, openErr = rv.root.OpenFile(name, os.O_RDWR|os.O_CREATE, 0o600)
		if openErr == nil {
			basis = tmp
			defer func() {
				if tmp != nil {
					tmp.Close()
				}
			}()
		} else {
			s.sendError("open %q failed: %s", name, errnoText(openErr))
		}
	} else {
		tmpName, tmp, openErr = rv.createTemp(name)
		if openErr == nil {
			defer func() {
				if tmp != nil {
					tmp.Close()
					rv.root.Remove(tmpName)
				}
			}()
		} else {
			s.sendError("mkstemp %q failed: %s", name, errnoText(openErr))
		}
	}

	if !s.server {
		if s.log.format != "" && !s.logAfter() {
			s.log.item(f, req.iflags, req.xname)
		}
		s.log.progress.begin(f.Name, f.Size, max(0, s.lists.total-int(req.ndx)-1), s.lists.total)
	}

	fileHash.Reset()
	var bw *bufio.Writer
	ow := &offsetWriter{f: tmp, sparse: o.Sparse && !inplace}
	if tmp != nil {
		if rv.bw == nil {
			rv.bw = bufio.NewWriterSize(ow, 256<<10)
		}
		bw = rv.bw
		bw.Reset(ow)
	}
	var writeErr error
	write := func(p []byte) {
		fileHash.Write(p)
		s.log.progress.update(int64(len(p)))
		if bw != nil && writeErr == nil {
			_, writeErr = bw.Write(p)
		}
	}
	// skip accounts for a block already in place without rewriting it.
	skip := func(p []byte) {
		fileHash.Write(p)
		s.log.progress.update(int64(len(p)))
		if bw != nil && writeErr == nil {
			if writeErr = bw.Flush(); writeErr == nil {
				ow.off += int64(len(p))
			}
		}
	}
	// fail keeps what has arrived of an interrupted transfer with --partial.
	var received int64
	fail := func(err error) error {
		if o.Partial && !inplace && tmp != nil && received > 0 && writeErr == nil && bw.Flush() == nil {
			tmp.Close()
			tmp = nil
			rv.savePartial(tmpName, name, f)
		}
		return err
	}
	blockBuf := make([]byte, head.blength)
	for {
		lit, blk, done, err := tr.Next()
		if err != nil {
			return false, fail(err)
		}
		if done {
			break
		}
		if lit != nil {
			s.stats.LiteralData += int64(len(lit))
			received += int64(len(lit))
			write(lit)
			continue
		}
		if blk < 0 || blk >= head.count {
			return false, fail(fmt.Errorf("%w: invalid block index %d (count %d)", wire.ErrProtocol, blk, head.count))
		}
		n := int(head.blength)
		if blk == head.count-1 && head.remainder != 0 {
			n = int(head.remainder)
		}
		data := blockBuf[:n]
		if basis == nil {
			return false, fmt.Errorf("%w: block reference without a basis file for %s", wire.ErrProtocol, f.Name)
		}
		off := int64(blk) * int64(head.blength)
		if _, err := basis.ReadAt(data, off); err != nil {
			writeErr = cmpErr(writeErr, fmt.Errorf("reading basis: %w", err))
			clear(data)
		}
		tr.SeeMatch(data)
		s.stats.MatchedData += int64(n)
		received += int64(n)
		if inplace && bw != nil && off == ow.off+int64(bw.Buffered()) {
			skip(data)
		} else {
			write(data)
		}
	}
	sum := make([]byte, s.xferSum.Size())
	s.in.ReadFull(sum)
	if err := s.in.Err(); err != nil {
		return false, fail(err)
	}
	if !s.server {
		s.log.progress.end()
	}
	if tmp == nil {
		return false, nil // data discarded
	}
	if writeErr == nil {
		writeErr = bw.Flush()
	}
	if writeErr == nil && (inplace || ow.sparse) {
		writeErr = tmp.Truncate(ow.off)
	}
	if writeErr == nil && o.Fsync {
		writeErr = tmp.Sync()
	}
	if writeErr != nil {
		s.sendError("write failed on %q: %v", name, writeErr)
		return false, nil
	}
	ok := bytes.Equal(sum, fileHash.Sum(nil))
	if !ok {
		rv.mu.Lock()
		second := rv.sent[req.ndx]
		rv.sent[req.ndx] = true
		rv.mu.Unlock()
		if second {
			s.sendError("ERROR: %s failed verification -- update discarded.", f.Name)
		} else {
			if o.Verbose > 0 || s.log.hasI > 0 {
				s.info("WARNING: " + f.Name + " failed verification -- update discarded (will try again).\n")
			}
			rv.events <- recvEvent{redo: req.ndx, list: l}
			return true, nil
		}
		return false, nil
	}

	// Finish: attributes, then rename into place.
	fi, err := tmp.Stat()
	if err == nil {
		rv.setAttrsOf(tmp, tmpName, f, fi, true)
	}
	tmp.Close()
	tmp = nil
	if o.DelayUpdates {
		p, _ := rv.partialPath(name)
		rv.root.Mkdir(path.Dir(p), 0o700)
		if err := rv.root.Rename(tmpName, p); err != nil {
			rv.root.Remove(tmpName)
			s.sendError("rename %q -> %q: %s", tmpName, p, errnoText(err))
			return false, nil
		}
		rv.delayed = append(rv.delayed, name)
	} else {
		if !inplace {
			if o.backups() {
				if err := rv.backupExisting(name); err != nil {
					s.sendError("backup of %q failed: %s", name, errnoText(err))
				}
			}
			if err := rv.root.Rename(tmpName, name); err != nil {
				rv.root.Remove(tmpName)
				s.sendError("rename %q -> %q: %s", tmpName, name, errnoText(err))
				return false, nil
			}
		}
		if o.PartialDir != "" {
			rv.removePartial(name)
		}
	}
	s.stats.NumTransferred++
	s.stats.TotalTransferredSize += f.Size
	if !s.server && s.log.format != "" && s.logAfter() {
		s.log.item(f, req.iflags, req.xname)
	}
	if o.RemoveSourceFiles && s.proto >= 30 || o.RemoveSourceFiles && !s.server {
		s.out.SendMsgInt(wire.MsgSuccess, req.ndx)
	}
	return false, nil
}

func cmpErr(old, new error) error {
	if old != nil {
		return old
	}
	return new
}

// createTemp makes the temporary file a transfer is written to: ".name.XXXXXX"
// next to the destination, or in --temp-dir.
func (rv *receiving) createTemp(name string) (string, *os.File, error) {
	if rv.root == nil {
		return "", nil, fs.ErrNotExist
	}
	dir, base := path.Split(name)
	if len(base) > 246 {
		base = base[:246]
	}
	prefix := "."
	if strings.HasPrefix(base, ".") {
		prefix = ""
	}
	if rv.o.TempDir != "" {
		dir, prefix = strings.TrimSuffix(rv.o.TempDir, "/")+"/", ""
	}
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	for range 100 {
		var r [6]byte
		rand.Read(r[:])
		for i := range r {
			r[i] = letters[int(r[i])%len(letters)]
		}
		tmpName := dir + prefix + base + "." + string(r[:])
		f, err := rv.root.OpenFile(tmpName, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return tmpName, f, err
	}
	return "", nil, fs.ErrExist
}

// deletion ------------------------------------------------------------------

// deletePass deletes extraneous files below every content directory.
func (rv *receiving) deletePass() {
	for _, f := range rv.files {
		if f.IsDir() && f.Flags&flist.FlagContentDir != 0 && f.Flags&flist.FlagCleared == 0 {
			rv.deleteInDir(rv.localName(f), f.Name, rv.inList)
		}
	}
}

// deleteInDir removes entries of the local directory name (wireName in the
// file list) that the sender did not list.
func (rv *receiving) deleteInDir(name, wireName string, keep func(wireName string) bool) {
	s, o := rv.s, rv.o
	if rv.root == nil || rv.single != "" {
		return
	}
	if s.ioErr.Load()&ioErrGeneral != 0 && !o.IgnoreErrors {
		if !rv.delLimit {
			s.info("IO error encountered -- skipping file deletion\n")
			rv.delLimit = true
		}
		return
	}
	d, err := rv.root.Open(name)
	if err != nil {
		return
	}
	entries, err := d.ReadDir(-1)
	d.Close()
	if err != nil {
		return
	}
	prefix := ""
	if wireName != "." && wireName != "" {
		prefix = strings.TrimSuffix(wireName, "/.") + "/"
	}
	for _, e := range entries {
		wn := prefix + e.Name()
		if keep(wn) {
			continue
		}
		isDir := e.IsDir()
		if !o.DeleteExcluded && rv.filters.Excluded(wn, isDir, filter.ReceiverSide) {
			continue // protected
		}
		if o.MaxDelete != 0 && (o.MaxDelete < 0 || rv.deletes >= o.MaxDelete) {
			if !rv.delLimit {
				rv.delLimit = true
				s.ioErr.Or(ioErrDelLimit)
				s.info(fmt.Sprintf("Deletions stopped due to --max-delete limit (%d skipped)\n", 1))
			}
			continue
		}
		local := path.Join(name, e.Name())
		rv.deleteEntry(local, wn, e)
	}
}

func (rv *receiving) deleteEntry(local, wn string, e fs.DirEntry) {
	s, o := rv.s, rv.o
	kind := 0
	switch t := e.Type(); {
	case t.IsDir():
		kind = 1
	case t&fs.ModeSymlink != 0:
		kind = 2
	case t&fs.ModeDevice != 0:
		kind = 3
	case t&(fs.ModeNamedPipe|fs.ModeSocket) != 0:
		kind = 4
	}
	if !o.DryRun {
		var err error
		if o.backups() {
			var fi fs.FileInfo
			if fi, err = rv.root.Lstat(local); err == nil {
				err = rv.backup(local, fi)
			}
		} else if kind == 1 {
			err = rv.root.RemoveAll(local)
		} else {
			err = rv.root.Remove(local)
		}
		if err != nil {
			s.sendError("delete_file: unlink %q failed: %s", wn, errnoText(err))
			return
		}
	}
	rv.deletes++
	rv.mu.Lock()
	rv.deleted[kind]++
	rv.mu.Unlock()
	if s.server {
		if s.proto >= 29 && (o.Verbose > 0 || s.log.hasI > 0) {
			payload := []byte(wn)
			if kind == 1 {
				payload = append(payload, 0)
			}
			s.out.SendMsg(wire.MsgDeleted, payload)
		}
	} else {
		s.log.deleted(wn, kind == 1)
	}
}

func (rv *receiving) deletedStats() [5]int32 {
	rv.mu.Lock()
	defer rv.mu.Unlock()
	return rv.deleted
}

// offsetWriter writes sequentially at off, leaving earlier and later file
// contents in place (an --inplace update reads its basis from them). With
// sparse set it writes into a new file and skips blocks of zeros, leaving
// holes; the file must be truncated to off at the end.
type offsetWriter struct {
	f      *os.File
	off    int64
	sparse bool
}

const sparseBlock = 4096

func (w *offsetWriter) Write(p []byte) (int, error) {
	if !w.sparse {
		n, err := w.f.WriteAt(p, w.off)
		w.off += int64(n)
		return n, err
	}
	total := len(p)
	for len(p) > 0 {
		// Up to the next block boundary, so holes are block-aligned.
		n := min(len(p), sparseBlock-int(w.off%sparseBlock))
		if allZero(p[:n]) {
			w.off += int64(n)
			p = p[n:]
			continue
		}
		// Write the run of blocks up to the next block of zeros.
		for n < len(p) {
			m := min(len(p)-n, sparseBlock)
			if allZero(p[n : n+m]) {
				break
			}
			n += m
		}
		if _, err := w.f.WriteAt(p[:n], w.off); err != nil {
			return total - len(p), err
		}
		w.off += int64(n)
		p = p[n:]
	}
	return total, nil
}

func allZero(p []byte) bool {
	for len(p) >= 8 {
		if p[0]|p[1]|p[2]|p[3]|p[4]|p[5]|p[6]|p[7] != 0 {
			return false
		}
		p = p[8:]
	}
	for _, b := range p {
		if b != 0 {
			return false
		}
	}
	return true
}

// partialPath is where --partial-dir keeps an interrupted transfer of
// name: beside the file for a relative directory, which is then below the
// destination root, or in an absolute directory outside it (abs is set).
func (rv *receiving) partialPath(name string) (p string, abs bool) {
	pd := rv.o.PartialDir
	if pd == "" {
		return "", false
	}
	dir, base := path.Split(name)
	if filepath.IsAbs(pd) {
		return filepath.Join(pd, base), true
	}
	return path.Join(dir, pd, base), false
}

// openBasis opens the file to use as the delta basis for name: a partial
// file left by an interrupted transfer, else the destination file itself
// when it exists.
func (rv *receiving) openBasis(name string, exists bool) *os.File {
	if rv.root == nil {
		return nil
	}
	if p, abs := rv.partialPath(name); p != "" {
		var f *os.File
		var err error
		if abs {
			f, err = os.Open(p)
		} else {
			f, err = rv.root.Open(p)
		}
		if err == nil {
			if fi, err := f.Stat(); err == nil && fi.Mode().IsRegular() {
				return f
			}
			f.Close()
		}
	}
	if !exists {
		return nil
	}
	f, _ := rv.root.Open(name)
	return f
}

// finishDelayed moves the files --delay-updates held back into place and
// removes the partial directories that leaves empty.
func (rv *receiving) finishDelayed() {
	dirs := map[string]bool{}
	for _, name := range rv.delayed {
		p, _ := rv.partialPath(name)
		if rv.o.backups() {
			if err := rv.backupExisting(name); err != nil {
				rv.s.sendError("backup of %q failed: %s", name, errnoText(err))
			}
		}
		if err := rv.root.Rename(p, name); err != nil {
			rv.s.sendError("rename %q -> %q: %s", p, name, errnoText(err))
			continue
		}
		dirs[path.Dir(p)] = true
	}
	for d := range dirs {
		rv.root.Remove(d)
	}
	rv.delayed = nil
}

// savePartial keeps the temp file of an interrupted transfer, at the
// destination name or in the partial directory, as the basis for the next
// attempt.
func (rv *receiving) savePartial(tmpName, name string, f *flist.File) {
	rv.root.Chmod(tmpName, fs.FileMode(f.Mode).Perm())
	p, abs := rv.partialPath(name)
	var err error
	switch {
	case p == "":
		err = rv.root.Rename(tmpName, name)
	case abs:
		if err = os.MkdirAll(filepath.Dir(p), 0o700); err == nil {
			err = os.Rename(filepath.Join(rv.root.Name(), tmpName), p)
		}
	default:
		rv.root.Mkdir(path.Dir(p), 0o700)
		err = rv.root.Rename(tmpName, p)
	}
	if err != nil {
		rv.root.Remove(tmpName)
	}
}

// removePartial deletes the partial file once name is complete, and a
// relative partial directory with it when that leaves it empty.
func (rv *receiving) removePartial(name string) {
	p, abs := rv.partialPath(name)
	if abs {
		os.Remove(p)
	} else if rv.root.Remove(p) == nil {
		rv.root.Remove(path.Dir(p))
	}
}
