// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package rsync

import (
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"time"

	"github.com/parallelworks/xfer/rsync/internal/checksum"
	"github.com/parallelworks/xfer/rsync/internal/delta"
	"github.com/parallelworks/xfer/rsync/internal/flist"
	"github.com/parallelworks/xfer/rsync/internal/token"
	"github.com/parallelworks/xfer/rsync/internal/wire"
)

// request is a file index with its item attributes, as exchanged between
// the generator, sender and receiver.
type request struct {
	ndx       int32
	iflags    uint32
	basisType byte
	xname     string
}

const fnamecmpFname = 0x80

// readRequest reads an index and its attributes, skipping keep-alives and
// consuming deletion statistics. On the receiving side with incremental
// recursion it also takes in the file lists that arrive between requests.
func (s *session) readRequest() (request, error) {
	r := s.in
	for {
		start := s.demux.DataRead()
		ndx := r.Ndx()
		if err := r.Err(); err != nil {
			return request{}, err
		}
		if ndx == wire.NdxDelStats {
			var del [5]int32
			for i := range del {
				del[i] = r.Varint()
			}
			if s.server && s.sender {
				s.out.Lock()
				s.writeDelStats(del)
				s.out.Unlock()
			}
			s.stats.NumDeleted = int64(del[0] + del[1] + del[2] + del[3] + del[4])
			continue
		}
		if s.inc && !s.sender && (ndx == wire.NdxFlistEOF || ndx <= wire.NdxFlistOffset) {
			if err := s.recvSubList(ndx); err != nil {
				return request{}, err
			}
			s.stats.FlistSize += s.demux.DataRead() - start
			continue
		}
		if ndx < 0 {
			if ndx != wire.NdxDone {
				return request{}, fmt.Errorf("%w: unexpected index %d", wire.ErrProtocol, ndx)
			}
			return request{ndx: ndx}, nil
		}
		req := request{ndx: ndx, basisType: fnamecmpFname}
		if s.proto < 29 {
			req.iflags = itemTransfer | itemMissingData
		} else {
			req.iflags = uint32(r.Int16())
			if req.iflags&itemBasisTypeFollows != 0 {
				req.basisType = r.Byte()
			}
			if req.iflags&itemXnameFollows != 0 {
				req.xname = r.Vstring(1023)
			}
		}
		if err := r.Err(); err != nil {
			return request{}, err
		}
		if s.proto == 29 && !s.inc && int(ndx) == s.lists.total && req.iflags == itemIsNew {
			continue // protocol 29 keep-alive
		}
		return req, nil
	}
}

// writeRequest writes an index and its attributes. The caller holds the
// output lock.
func (s *session) writeRequest(req request) {
	w := s.out.W
	w.Ndx(req.ndx)
	if s.proto < 29 {
		return
	}
	w.Int16(uint16(req.iflags))
	if req.iflags&itemBasisTypeFollows != 0 {
		w.Byte(req.basisType)
	}
	if req.iflags&itemXnameFollows != 0 {
		w.Vstring(req.xname)
	}
}

func (s *session) writeDelStats(del [5]int32) {
	s.out.W.Ndx(wire.NdxDelStats)
	for _, v := range del {
		s.out.W.Varint(v)
	}
}

type sumHead struct {
	count, blength, s2length, remainder int32
}

func (s *session) readSumHead() (sumHead, error) {
	r := s.in
	h := sumHead{count: r.Int32(), blength: r.Int32(), s2length: r.Int32(), remainder: r.Int32()}
	if err := r.Err(); err != nil {
		return h, err
	}
	maxBlen := int32(1 << 17)
	if s.proto < 30 {
		maxBlen = 1 << 29
	}
	if h.count < 0 || h.blength < 0 || h.blength > maxBlen || (h.count > 0 && h.blength == 0) ||
		h.s2length < 0 || int(h.s2length) > s.xferSum.Size() || h.remainder < 0 || h.remainder > h.blength {
		return h, fmt.Errorf("%w: invalid checksum header (count=%d blength=%d s2length=%d remainder=%d)",
			wire.ErrProtocol, h.count, h.blength, h.s2length, h.remainder)
	}
	return h, nil
}

func (s *session) writeSumHead(h sumHead) {
	w := s.out.W
	w.Int32(h.count)
	w.Int32(h.blength)
	w.Int32(h.s2length)
	w.Int32(h.remainder)
}

func (s *session) tokenKind() token.Kind {
	switch s.compressor {
	case "zlib":
		return token.Zlib
	case "zlibx":
		return token.Zlibx
	case "zstd":
		return token.Zstd
	case "lz4":
		return token.LZ4
	}
	return token.Simple
}

// sendFiles runs the sender's half of the transfer phase.
func (s *session) sendFiles() error {
	maxPhase := 1
	if s.proto >= 29 {
		maxPhase = 2
	}
	tw := token.NewWriter(s.tokenKind(), s.out.W, s.proto, s.opts.CompressLevel)
	if fc, ok := tw.(token.FileCompressor); ok {
		skip := newSkipCompress(s.opts)
		s.compressFile = func(f *flist.File) { fc.CompressFile(!skip.skip(f.Name)) }
	}
	block := checksum.NewBlock(s.xferSum, s.seed, s.seedFirst)
	fileHash := checksum.NewFile(s.xferSum, s.seed)
	// A local transfer reports from the receiving side, as rsync does.
	s.log.sending = !s.localTransfer
	s.log.localSender = s.localTransfer

	if s.opts.RemoveSourceFiles {
		s.onSuccess = func(ndx int32) {
			if f, _, err := s.lists.lookup(ndx); err == nil {
				s.removeSource(f)
			}
		}
	}

	phase := 0
	for {
		if s.inc {
			if err := s.sendLookahead(); err != nil {
				return err
			}
		}
		req, err := s.readRequest()
		if err != nil {
			return err
		}
		if req.ndx == wire.NdxDone {
			// With incremental recursion each list is finished by an
			// NDX_DONE; only the one for the last list ends the phase.
			if s.inc && s.lists.unfreed() > 0 && s.lists.freeOldest() > 0 {
				s.out.Lock()
				s.out.W.Ndx(wire.NdxDone)
				s.out.Unlock()
				continue
			}
			phase++
			if phase > maxPhase {
				break
			}
			s.out.Lock()
			s.out.W.Ndx(wire.NdxDone)
			s.out.Unlock()
			continue
		}
		f, _, err := s.lists.lookup(req.ndx)
		if err != nil {
			return err
		}
		s.countCreated(f, req.iflags)
		if req.iflags&itemTransfer == 0 {
			if !s.server && s.log.shouldLog(f, req.iflags, req.xname) {
				s.log.item(f, req.iflags, req.xname)
			}
			if s.proto >= 29 {
				s.out.Lock()
				s.writeRequest(req)
				s.out.Unlock()
			}
			continue
		}
		if phase == 2 {
			return fmt.Errorf("%w: got transfer request in phase 2", wire.ErrProtocol)
		}
		if s.opts.DryRun {
			if !s.server {
				s.log.item(f, req.iflags, req.xname)
			}
			s.out.Lock()
			s.writeRequest(req)
			s.out.Unlock()
			continue
		}
		head, err := s.readSumHead()
		if err != nil {
			return err
		}
		sums, err := s.readSums(head)
		if err != nil {
			return err
		}
		if err := s.sendFile(f, req, head, sums, block, fileHash, tw); err != nil {
			return err
		}
	}

	if s.log.lv.debug[debugDeltasum] > 0 {
		m := s.matchStats
		s.info(fmt.Sprintf("total: matches=%d  hash_hits=%d  false_alarms=%d data=%d\n",
			m.Matches, m.HashHits, m.FalseAlarms, s.stats.LiteralData))
	}
	s.out.Lock()
	if s.proto >= 30 && s.ioErr.Load() != 0 {
		s.out.W.Flush()
		s.out.Mux.WriteMsg(wire.MsgIOError, le32bytes(uint32(s.ioErr.Load())))
	}
	s.out.W.Ndx(wire.NdxDone)
	if s.server {
		s.writeStats()
	}
	err := s.out.W.Flush()
	s.out.Unlock()
	if err != nil {
		return err
	}
	// rsync's byte counts end here, before the goodbye.
	s.endCounts[0], s.endCounts[1] = s.ioCounts()
	s.ended = true
	return s.readFinalGoodbye()
}

// ioCounts returns the bytes read and written since the protocol setup,
// which rsync leaves out of its counts.
func (s *session) ioCounts() (read, written int64) {
	return s.demux.Consumed() - s.setupCounts[0], s.counter.written.Load() - s.setupCounts[1]
}

func le32bytes(v uint32) []byte { return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)} }

// readSums reads the block checksums announced by head.
func (s *session) readSums(h sumHead) (*delta.Sums, error) {
	sums := &delta.Sums{
		BlockLen:  int(h.blength),
		Remainder: int(h.remainder),
		StrongLen: int(h.s2length),
		Weak:      make([]uint32, h.count),
		Strong:    make([]byte, int(h.count)*int(h.s2length)),
	}
	r := s.in
	for i := range sums.Weak {
		sums.Weak[i] = uint32(r.Int32())
		r.ReadFull(sums.Strong[i*sums.StrongLen : (i+1)*sums.StrongLen])
	}
	return sums, r.Err()
}

// sendFile transmits one file as a delta against the receiver's sums.
func (s *session) sendFile(f *flist.File, req request, head sumHead, sums *delta.Sums,
	block *checksum.Block, fileHash hash.Hash, tw token.Writer) error {
	file, err := openSource(f.Source, s.opts.OpenNoatime)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			s.ioErr.Or(ioErrVanished)
			s.sendWarningVanished(f)
		} else {
			s.ioErr.Or(ioErrGeneral)
			s.sendError("send_files failed to open %q: %s", absPath(f.Source), errnoText(err))
		}
		if s.proto >= 30 {
			s.out.SendMsgInt(wire.MsgNoSend, req.ndx)
		}
		return nil
	}
	defer file.Close()

	s.out.Lock()
	defer s.out.Unlock()
	s.writeRequest(req)
	s.writeSumHead(head)

	if !s.server {
		if s.log.logs() && !s.logAfter() {
			s.log.item(f, req.iflags, req.xname)
		}
		rem, total, more := s.lists.toCheck(req.ndx)
		s.log.progress.begin(f.Name, f.Size, rem, total, more)
	}
	fileHash.Reset()
	if s.compressFile != nil {
		s.compressFile(f)
	}
	em := &sendEmitter{s: s, tw: tw, h: fileHash}
	var src io.Reader = file
	if s.opts.BWLimit > 0 {
		src = newRateLimiter(file, s.opts.BWLimit)
	}
	err = delta.MatchWith(src, sums, block.Sum, em, delta.Options{Inplace: s.opts.Inplace, Stats: &s.matchStats})
	if err == nil {
		err = tw.End()
	}
	sum := fileHash.Sum(nil)
	if err != nil {
		// Read errors make the receiver discard the file by sending a
		// checksum that cannot match.
		if em.writeErr != nil {
			return em.writeErr
		}
		s.ioErr.Or(ioErrGeneral)
		s.sendError("read errors mapping %q: %v", absPath(f.Source), err)
		if err := tw.End(); err != nil {
			return err
		}
		allZero := true
		for _, b := range sum {
			allZero = allZero && b == 0
		}
		clear(sum)
		if allZero {
			sum[len(sum)-1] = 1
		}
	}
	s.out.W.Write(sum)
	s.stats.NumTransferred++
	s.stats.TotalTransferredSize += f.Size
	if !s.server {
		s.log.progress.end()
		if s.log.logs() && s.logAfter() {
			s.log.item(f, req.iflags, req.xname)
		}
	}
	return s.out.W.Err()
}

func (s *session) sendWarningVanished(f *flist.File) {
	msg := "file has vanished: \"" + s.log.safe(absPath(f.Source)) + "\"\n"
	if s.server && s.out.Mux.Enabled() {
		tag := byte(wire.MsgWarning)
		if s.proto < 30 {
			tag = wire.MsgInfo
		}
		s.out.SendMsg(tag, []byte(msg))
		return
	}
	s.log.errorf("%s", msg)
}

// logAfter reports whether transfer lines are printed after the data moved,
// which is when the format includes transfer statistics.
func (s *session) logAfter() bool {
	// Events report a file once it is done.
	return s.log.format == "" || containsAny(s.log.format, "%b", "%c", "%C")
}

// sendEmitter turns the delta into tokens, keeping the file checksum and
// statistics current.
type sendEmitter struct {
	s        *session
	tw       token.Writer
	h        hash.Hash
	writeErr error
}

func (e *sendEmitter) Literal(p []byte) error {
	e.h.Write(p)
	e.s.stats.LiteralData += int64(len(p))
	e.s.log.progress.update(int64(len(p)))
	if err := e.tw.Literal(p); err != nil {
		e.writeErr = err
		return err
	}
	return nil
}

func (e *sendEmitter) Match(i int, data []byte) error {
	e.h.Write(data)
	e.s.stats.MatchedData += int64(len(data))
	e.s.log.progress.update(int64(len(data)))
	if err := e.tw.Match(int32(i), data); err != nil {
		e.writeErr = err
		return err
	}
	return nil
}

// writeStats sends the server sender's totals to the client receiver.
func (s *session) writeStats() {
	w := s.out.W
	put := func(v int64) {
		if s.proto >= 30 {
			w.Varlong(v, 3)
		} else {
			w.Longint(v)
		}
	}
	// Only what has reached the connection counts: not the NDX_DONE
	// buffered with these stats.
	r, wr := s.ioCounts()
	put(r)
	put(wr)
	put(s.stats.TotalSize)
	if s.proto >= 29 {
		put(s.stats.FlistBuildTime.Milliseconds())
		put(s.stats.FlistXferTime.Milliseconds())
	}
}

// readStats reads the server sender's totals (client receiver).
func (s *session) readStats() {
	r := s.in
	get := func() int64 {
		if s.proto >= 30 {
			return r.Varlong(3)
		}
		return r.Longint()
	}
	s.peerStats.read = get()
	s.peerStats.written = get()
	s.stats.TotalSize = get()
	if s.proto >= 29 {
		s.stats.FlistBuildTime = time.Duration(get()) * time.Millisecond
		s.stats.FlistXferTime = time.Duration(get()) * time.Millisecond
	}
}

// readFinalGoodbye waits for the receiving side's last NDX_DONE.
func (s *session) readFinalGoodbye() error {
	if s.proto < 29 {
		if v := s.in.Int32(); s.in.Err() == nil && v != wire.NdxDone {
			return fmt.Errorf("%w: invalid final goodbye %d", wire.ErrProtocol, v)
		}
		return s.in.Err()
	}
	req, err := s.readRequest()
	if err != nil {
		return err
	}
	if s.proto >= 31 && req.ndx == wire.NdxDone {
		s.out.Lock()
		s.out.W.Ndx(wire.NdxDone)
		err := s.out.W.Flush()
		s.out.Unlock()
		if err != nil {
			return err
		}
		if req, err = s.readRequest(); err != nil {
			return err
		}
	}
	if req.ndx != wire.NdxDone {
		return fmt.Errorf("%w: invalid final goodbye index %d", wire.ErrProtocol, req.ndx)
	}
	return nil
}

// removeSource deletes a sent file for --remove-source-files, if it is
// unchanged since the file list was built.
func (s *session) removeSource(f *flist.File) {
	fi, err := os.Lstat(f.Source)
	if err != nil {
		return
	}
	if fi.Size() != f.Size || fi.ModTime().Unix() != f.ModTime || fi.IsDir() {
		s.sendError("Skipping sender remove for changed file: %s", f.Name)
		return
	}
	if err := os.Remove(f.Source); err != nil {
		s.sendError("sender failed to remove %s: %s", f.Name, errnoText(err))
		return
	}
	if s.opts.Verbose > 1 && !s.server {
		s.log.print("sender removed " + f.Name + "\n")
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
	}
	return false
}
