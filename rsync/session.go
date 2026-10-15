// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package rsync

import (
	"crypto/fips140"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/parallelworks/xfer/rsync/internal/checksum"
	"github.com/parallelworks/xfer/rsync/internal/flist"
	"github.com/parallelworks/xfer/rsync/internal/wire"
)

const (
	// ProtocolVersion is the newest rsync protocol version spoken.
	ProtocolVersion = 33
	// MinProtocolVersion is the oldest rsync protocol version spoken.
	MinProtocolVersion = 27
)

// Compatibility flags exchanged at protocol 30 and later.
const (
	cfIncRecurse        = 1 << 0
	cfSymlinkTimes      = 1 << 1
	cfSymlinkIconv      = 1 << 2
	cfSafeFlist         = 1 << 3
	cfAvoidXattrOptim   = 1 << 4
	cfChksumSeedFix     = 1 << 5
	cfInplacePartialDir = 1 << 6
	cfVarintFlistFlags  = 1 << 7
	cfID0Names          = 1 << 8
)

// clientCaps is the capability string our client sends with -e; 'i'
// (incremental recursion) is added when the options allow it. 's' (iconv
// of symlink targets) has no effect since --iconv is not supported, and 'I'
// (--inplace with --partial-dir) none since we refuse that combination.
const clientCaps = ".LsfxCIvu"

// allowIncRecurse reports whether this side can use incremental recursion
// in the given role. The receiving side decides the options that only
// matter to it. We also leave it out for -R, -H and --list-only.
func allowIncRecurse(o *Options, sender bool) bool {
	if o.NoIncRecursive || !o.Recursive || o.Relative || o.HardLinks || o.ListOnly {
		return false
	}
	if (o.Protocol != 0 && o.Protocol < 30) || sender {
		return o.Protocol == 0 || o.Protocol >= 30
	}
	switch o.Delete {
	case DeleteNone, DeleteAuto, DeleteDuring:
	default:
		return false
	}
	return !o.DelayUpdates && !o.PruneEmptyDirs
}

var (
	checksumList = []string{"xxh128", "xxh3", "xxh64", "md5", "md4"}
	compressList = []string{"zstd", "lz4", "zlibx", "zlib"}

	// fipsChecksumList omits the MD4/MD5 digests, which Go refuses in
	// FIPS 140-only mode. rsync uses these checksums to find changed data,
	// not to protect it; the transport provides security.
	fipsChecksumList = []string{"xxh128", "xxh3", "xxh64"}
)

// session is one end of an rsync protocol connection.
type session struct {
	opts   *Options
	server bool
	sender bool // this end sends files

	proto      int
	compat     uint32
	seed       int32
	xferSum    checksum.Algorithm
	fileSum    checksum.Algorithm
	seedFirst  bool
	compressor string // "" when not compressing

	raw   io.ReadWriter
	demux *wire.Demux
	in    *wire.Reader
	out   *wire.Out

	log *logger

	counter *countingConn

	// localTransfer is set when both ends run in this process.
	localTransfer bool

	// inc is set with incremental recursion. lists holds the file lists
	// in use; enc and dec carry the file-list coding state, which spans
	// every list of the session.
	inc   bool
	lists *fileLists
	enc   *flist.Encoder
	dec   *flist.Decoder
	exp   *expander // sender: directories whose lists are still to send

	// onSubList, if set, is called on the receiving side for each list
	// that arrives after the first.
	onSubList func(l *fileList)

	filesFrom []string // the client's --files-from list, when it read it

	compressFile func(f *flist.File) // sender: --skip-compress for the next file

	onSuccess func(ndx int32)
	onNoSend  func(ndx int32)

	peerStats struct{ read, written int64 }

	// setupCounts are the bytes read and written by the protocol setup,
	// endCounts those since then when the sender finished (ended is set).
	setupCounts, endCounts [2]int64
	ended                  bool

	ioErr       atomic.Int32 // ioErr* bits, set by more than one goroutine
	keepalive   atomic.Bool  // the peer accepts MSG_NOOP on our output
	started     atomic.Bool  // the protocol setup is done
	timedOut    atomic.Bool  // --timeout or --contimeout expired
	conTimedOut atomic.Bool  // --contimeout expired
	stats       Stats
	exitCode    int // non-fatal outcome, e.g. 23 for partial transfers
	peerExited  bool
}

func newSession(opts *Options, raw io.ReadWriter, server, sender bool, log *logger) *session {
	counter := &countingConn{rw: raw}
	raw = counter
	s := &session{opts: opts, server: server, sender: sender, raw: raw, log: log, counter: counter}
	var in io.Reader = raw
	if sender {
		in = newReadAhead(raw)
	}
	s.demux = wire.NewDemux(in)
	s.in = wire.NewReader(s.demux)
	s.out = wire.NewOut(raw)
	s.demux.BeforeBlock = s.out.TryFlush
	s.demux.Handle = s.handleMsg
	return s
}

// handshake runs the protocol setup: version exchange, compatibility
// flags, algorithm negotiation and the checksum seed. clientInfo is the
// capability string a server received with -e.
func (s *session) handshake(clientInfo string) error {
	ours := ProtocolVersion
	if s.opts.Protocol != 0 {
		ours = s.opts.Protocol
	}
	w, r := s.out.W, s.in
	w.Int32(int32(ours))
	if err := w.Flush(); err != nil {
		return err
	}
	remote := int(r.Int32())
	if err := r.Err(); err != nil {
		return fmt.Errorf("reading protocol version: %w", err)
	}
	if remote < 20 || remote > 40 {
		return &ExitError{Code: ExitProtocol, Err: errors.New("protocol version mismatch -- is your shell clean?\n(see the rsync manpage for an explanation)")}
	}
	if remote < MinProtocolVersion {
		return &ExitError{Code: ExitProtocol, Err: fmt.Errorf("remote rsync speaks protocol %d; at least %d is required", remote, MinProtocolVersion)}
	}
	s.proto = min(ours, remote)
	w.Protocol, r.Protocol = s.proto, s.proto

	if s.proto >= 30 {
		if s.server {
			for _, c := range []struct {
				letter byte
				flag   uint32
			}{{'L', cfSymlinkTimes}, {'s', cfSymlinkIconv}, {'f', cfSafeFlist}, {'x', cfAvoidXattrOptim}, {'C', cfChksumSeedFix}, {'I', cfInplacePartialDir}, {'v', cfVarintFlistFlags}, {'u', cfID0Names}} {
				if strings.IndexByte(clientInfo, c.letter) >= 0 {
					s.compat |= c.flag
				}
			}
			if strings.IndexByte(clientInfo, 'i') >= 0 && allowIncRecurse(s.opts, s.sender) {
				s.compat |= cfIncRecurse
			}
			w.Varint(int32(s.compat))
		} else {
			w.Flush()
			s.compat = uint32(r.Varint())
			if s.compat&cfIncRecurse != 0 && !allowIncRecurse(s.opts, s.sender) {
				//lint:ignore ST1005 the message matches rsync's
				return &ExitError{Code: ExitSyntax, Err: errors.New("Incompatible options specified for inc-recursive connection.")}
			}
		}
		s.inc = s.compat&cfIncRecurse != 0
	}
	s.seedFirst = s.compat&cfChksumSeedFix != 0

	if err := s.negotiate(); err != nil {
		return err
	}

	if s.server {
		seed := s.opts.ChecksumSeed
		if seed == 0 {
			seed = randomSeed()
		}
		s.seed = seed
		w.Int32(seed)
	} else {
		w.Flush()
		s.seed = r.Int32()
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if err := r.Err(); err != nil {
		return err
	}

	// Multiplexing starts right after setup. The server always
	// multiplexes its output; the client's output is multiplexed from
	// protocol 30 on. Input mirrors the peer's output.
	if s.server || s.proto >= 30 {
		s.out.Mux.Enable()
	}
	if !s.server || s.proto >= 30 {
		s.demux.Enable()
	}
	// Peers of protocol 30 and later accept MSG_NOOP as a keep-alive.
	s.keepalive.Store(s.proto >= 30 && s.out.Mux.Enabled())
	s.started.Store(true)
	s.setupCounts = [2]int64{s.counter.read.Load(), s.counter.written.Load()}
	return nil
}

// negotiate picks the checksum and compression algorithms.
func (s *session) negotiate() error {
	o := s.opts
	negotiated := s.proto >= 30 && s.compat&cfVarintFlistFlags != 0

	xferChoice, fileChoice, _ := strings.Cut(o.ChecksumChoice, ",")
	if xferChoice == "auto" {
		xferChoice = ""
	}
	if fileChoice == "auto" {
		fileChoice = ""
	}
	compressChoice := ""
	if o.Compress {
		compressChoice = o.CompressChoice
	}

	fips := o.FIPS || fips140.Enabled()
	ourChecksums := checksumList
	if fips {
		ourChecksums = fipsChecksumList
		for _, c := range []string{xferChoice, fileChoice} {
			if c != "" && !slices.Contains(fipsChecksumList, c) {
				return &ExitError{Code: ExitUnsupported, Err: fmt.Errorf("FIPS mode: checksum %q is not allowed", c)}
			}
		}
		if xferChoice == "" && !negotiated {
			return &ExitError{Code: ExitUnsupported, Err: fmt.Errorf("FIPS mode: the remote rsync (protocol %d) cannot negotiate a non-MD4/MD5 checksum; rsync 3.2.0 or newer is required", s.proto)}
		}
	}

	sendChecksums := negotiated && xferChoice == ""
	sendCompress := negotiated && o.Compress && compressChoice == ""
	w, r := s.out.W, s.in
	if sendChecksums {
		list := slices.Clone(ourChecksums)
		if s.server && !fips {
			list = append(list, "none")
		}
		w.Vstring(strings.Join(list, " "))
	}
	if sendCompress {
		list := slices.Clone(compressList)
		if s.server {
			list = append(list, "none")
		}
		w.Vstring(strings.Join(list, " "))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if sendChecksums {
		peer := strings.Fields(r.Vstring(255))
		xferChoice = pick(ourChecksums, peer)
		if xferChoice == "" && r.Err() == nil {
			return &ExitError{Code: ExitUnsupported, Err: errors.New("failed to negotiate a checksum choice")}
		}
	}
	if sendCompress {
		peer := strings.Fields(r.Vstring(255))
		compressChoice = pick(compressList, peer)
		if compressChoice == "" && r.Err() == nil {
			return &ExitError{Code: ExitUnsupported, Err: errors.New("failed to negotiate a compression choice")}
		}
	}
	if err := r.Err(); err != nil {
		return err
	}

	switch {
	case xferChoice != "":
		alg, err := checksum.Parse(xferChoice)
		if err != nil {
			return &ExitError{Code: ExitUnsupported, Err: err}
		}
		s.xferSum = alg
	case s.proto >= 30:
		s.xferSum = checksum.MD5
	default:
		s.xferSum = checksum.MD4Old
	}
	s.fileSum = s.xferSum
	if fileChoice != "" {
		alg, err := checksum.Parse(fileChoice)
		if err != nil {
			return &ExitError{Code: ExitUnsupported, Err: err}
		}
		s.fileSum = alg
	}

	if o.Compress {
		if compressChoice == "" {
			compressChoice = "zlib"
		}
		switch compressChoice {
		case "none":
		case "zlib", "zlibx", "zstd", "lz4":
			s.compressor = compressChoice
		default:
			return &ExitError{Code: ExitUnsupported, Err: fmt.Errorf("unsupported compression %q", compressChoice)}
		}
	}
	return nil
}

// pick returns the first of ours that the peer also listed.
func pick(ours, peer []string) string {
	for _, name := range ours {
		for _, p := range peer {
			if p == name || (name == "xxh64" && p == "xxhash") {
				return name
			}
		}
	}
	return ""
}

// flistConfig returns the file-list settings implied by the session.
func (s *session) flistConfig() flist.Config {
	o := s.opts
	return flist.Config{
		Protocol:         s.proto,
		VarintFlags:      s.compat&cfVarintFlistFlags != 0,
		SafeFlist:        s.proto >= 31 || s.compat&cfSafeFlist != 0,
		ID0Names:         s.compat&cfID0Names != 0,
		PreserveUID:      o.Owner,
		PreserveGID:      o.Group,
		PreserveLinks:    o.Links,
		PreserveDevices:  o.Devices,
		PreserveSpecials: o.Specials,
		HardLinks:        o.HardLinks,
		NumericIDs:       o.NumericIDs,
		Relative:         o.Relative,
		AlwaysChecksum:   o.Checksum,
		ChecksumLen:      s.fileSum.Size(),
		IncRecurse:       s.inc,
	}
}

// handleMsg processes out-of-band messages from the peer.
func (s *session) handleMsg(tag byte, payload []byte) error {
	switch tag {
	case wire.MsgInfo:
		s.log.remote(false, payload)
	case wire.MsgErrorXfer:
		s.log.remote(true, payload)
		if s.exitCode == 0 {
			s.exitCode = ExitPartial
		}
	case wire.MsgError, wire.MsgWarning, wire.MsgLog, wire.MsgClient, wire.MsgErrorSocket:
		s.log.remote(true, payload)
	case wire.MsgIOError:
		if len(payload) == 4 {
			s.ioErr.Or(int32(le32(payload)) & ioErrValidMask)
		}
	case wire.MsgBlockStats:
		// Protocol 33: the receiving server's count of 4 KiB blocks written.
		if len(payload) == 8 {
			s.stats.BlocksTouched = int64(binary.LittleEndian.Uint64(payload))
		}
	case wire.MsgNoop, wire.MsgIOTimeout:
	case wire.MsgDeleted:
		name := strings.TrimSuffix(string(payload), "\x00")
		isDir := len(payload) > 0 && payload[len(payload)-1] == 0
		s.log.deleted(name, isDir)
	case wire.MsgNoSend:
		if len(payload) == 4 && s.onNoSend != nil {
			s.onNoSend(int32(le32(payload)))
		}
	case wire.MsgSuccess:
		if len(payload) == 4 && s.onSuccess != nil {
			s.onSuccess(int32(le32(payload)))
		}
	case wire.MsgErrorExit:
		code := 0
		if len(payload) == 4 {
			code = int(le32(payload))
		}
		s.peerExited = true
		if len(payload) == 4 && s.proto >= 31 {
			if s.sender {
				s.out.SendMsg(wire.MsgErrorExit, nil)
			} else {
				s.out.SendMsgInt(wire.MsgErrorExit, int32(code))
			}
		}
		if code == 0 {
			code = ExitStreamIO
		}
		return &ExitError{Code: code, Err: errPeerExited}
	default:
		return fmt.Errorf("%w: unexpected message tag %d", wire.ErrProtocol, tag)
	}
	return nil
}

var errPeerExited = errors.New("remote side exited with an error")

func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

// sendError reports a non-fatal transfer error to the user: locally when we
// are the client, and through the protocol when we are the server.
func (s *session) sendError(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	if s.exitCode == 0 {
		s.exitCode = ExitPartial
	}
	if s.server && s.out.Mux.Enabled() {
		s.out.SendMsg(wire.MsgErrorXfer, []byte("rsync: "+msg))
		return
	}
	s.log.errorf("%s", msg)
}

// info prints an informational line: locally on the client, or as an
// MSG_INFO for the client to print when we are the server.
func (s *session) info(line string) {
	if s.server {
		if s.out.Mux.Enabled() {
			s.out.SendMsg(wire.MsgInfo, []byte(line))
		}
		return
	}
	s.log.print(line)
}

// peerIsServerSender reports whether the other end is a server sending
// files, which sends its statistics at the end.
func (s *session) peerIsServerSender() bool { return !s.server && !s.sender }
