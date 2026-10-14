// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package rsync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/parallelworks/xfer/rsync/internal/filter"
	"github.com/parallelworks/xfer/rsync/internal/flist"
	"github.com/parallelworks/xfer/rsync/internal/wire"
	"github.com/parallelworks/xfer/transport"
)

// Serve runs the server half of a transfer: what "rsync --server ..." does
// on the remote end of a remote-shell connection. args are the arguments
// after the program name, as the client sent them; the protocol runs over
// conn (the command's standard input and output), and diagnostics that
// cannot travel in the protocol go to stderr.
//
// A program that accepts SSH sessions, such as an agent embedding this
// package, can call Serve when a session's command line starts with
// "rsync --server", without installing rsync.
func Serve(ctx context.Context, args []string, conn io.ReadWriter, stderr io.Writer) error {
	return serve(ctx, args, conn, stderr, false)
}

func serve(ctx context.Context, args []string, conn io.ReadWriter, stderr io.Writer, local bool) error {
	o, operands, err := ParseArgs(args)
	if err != nil {
		return err
	}
	if !o.Server {
		return &UsageError{"rsync server mode requires --server"}
	}
	if o.ProtectArgs && !local {
		more, err := readProtectedArgs(conn)
		if err != nil {
			return err
		}
		all := append(slices.Clone(args), more...)
		if o, operands, err = ParseArgs(all); err != nil {
			return err
		}
	}
	clientInfo := o.Rsh // -e carries the client's capabilities in server mode
	if len(operands) > 0 && operands[0] == "." {
		operands = operands[1:]
	}

	log := newLogger(o, io.Discard, stderr)
	log.format = "" // a server prints nothing on its own
	s := newSession(o, conn, true, o.Sender, log)
	s.localTransfer = local

	stop := context.AfterFunc(ctx, func() {
		if c, ok := conn.(io.Closer); ok {
			c.Close()
		}
	})
	defer stop()

	err = s.runTimed(func() error { return s.runServer(clientInfo, operands) })
	if s.timedOut.Load() {
		return err
	}
	if err != nil {
		s.fatal(err)
		return err
	}
	if code := max(s.exitCode, codeForIOError(s.ioErr.Load())); code != 0 {
		return &ExitError{Code: code, Err: errors.New(exitMessage(code))}
	}
	return nil
}

func (s *session) runServer(clientInfo string, paths []string) error {
	o := s.opts
	if err := s.handshake(clientInfo); err != nil {
		return err
	}
	if s.sender {
		filters, err := s.recvFilterList()
		if err != nil {
			return err
		}
		if o.CVSExclude {
			filters = filter.NewList(append(filter.CVSRules(), filters.Rules()...))
		}
		files, err := s.sendFileList(paths, localRules(o, filters))
		if err != nil {
			return err
		}
		if len(files) == 0 {
			return s.out.Flush()
		}
		return s.sendFiles()
	}
	filters := filter.NewList(nil)
	if receiverWantsList(o, s.proto) {
		var err error
		if filters, err = s.recvFilterList(); err != nil {
			return err
		}
	}
	files, users, groups, err := s.recvFileList()
	if err != nil {
		return err
	}
	dest := "."
	if len(paths) > 0 {
		dest = paths[0]
	}
	return s.receiveFiles(files, users, groups, dest, localRules(o, filters))
}

// fatal reports a fatal error to the client and, for protocol 31, tells it
// we are exiting.
func (s *session) fatal(err error) {
	if s.peerExited || !s.out.Mux.Enabled() {
		return
	}
	code := ExitCode(err)
	msg := fmt.Sprintf("rsync error: %v (code %d) [%s]\n", err, code, map[bool]string{true: "sender", false: "receiver"}[s.sender])
	tag := byte(wire.MsgError)
	if s.proto < 30 {
		tag = wire.MsgErrorXfer
	}
	s.out.SendMsg(tag, []byte(msg))
	if s.proto >= 31 {
		s.out.SendMsgInt(wire.MsgErrorExit, int32(code))
	}
	s.out.Flush()
}

// readProtectedArgs reads NUL-terminated arguments sent on standard input
// with --protect-args. It reads one byte at a time so that it never
// consumes protocol data that follows.
func readProtectedArgs(r io.Reader) ([]string, error) {
	var args []string
	var cur []byte
	var b [1]byte
	first := true
	for {
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return nil, fmt.Errorf("reading protected args: %w", err)
		}
		if b[0] != 0 {
			cur = append(cur, b[0])
			if len(cur) > 1<<16 {
				return nil, fmt.Errorf("%w: protected argument too long", wire.ErrProtocol)
			}
			continue
		}
		if len(cur) == 0 {
			return args, nil
		}
		if !first { // the first word is the program name
			args = append(args, string(cur))
		}
		first = false
		cur = cur[:0]
	}
}

// sendFilterList sends the client's rules to the server.
func (s *session) sendFilterList(l *filter.List, clientIsSender bool) error {
	rules, err := l.WireRules(s.proto, clientIsSender, s.opts.DeleteExcluded)
	if err != nil {
		return &ExitError{Code: ExitProtocol, Err: err}
	}
	s.out.Lock()
	defer s.out.Unlock()
	w := s.out.W
	for _, r := range rules {
		if r == "" {
			continue
		}
		w.Int32(int32(len(r)))
		w.Write([]byte(r))
	}
	w.Int32(0)
	return w.Flush()
}

// recvFilterList reads the client's rules.
func (s *session) recvFilterList() (*filter.List, error) {
	var rules []filter.Rule
	r := s.in
	for {
		n := r.Int32()
		if err := r.Err(); err != nil {
			return nil, err
		}
		if n == 0 {
			break
		}
		if n < 0 || n >= 5120 {
			return nil, fmt.Errorf("%w: filter rule overflow (%d bytes)", wire.ErrProtocol, n)
		}
		b := make([]byte, n)
		r.ReadFull(b)
		if err := r.Err(); err != nil {
			return nil, err
		}
		rs, err := filter.ParseWireRule(string(b), s.proto)
		if err != nil {
			return nil, &ExitError{Code: ExitProtocol, Err: err}
		}
		rules = append(rules, rs...)
	}
	return filter.NewList(rules), nil
}

// sendFileList builds and sends the file list, returning it sorted.
func (s *session) sendFileList(paths []string, filters *filter.List) ([]*flist.File, error) {
	start := time.Now()
	sc := s.newScanner(filters)
	files := sc.scanArgs(paths)
	s.stats.FlistBuildTime = time.Since(start)

	xferStart := time.Now()
	s.out.Lock()
	before := s.counter.written.Load() + int64(s.out.W.Buffered())
	cfg := s.flistConfig()
	s.enc = flist.NewEncoder(s.out.W, cfg)
	s.enc.LookupUser, s.enc.LookupGroup = userName, groupName
	s.lists = newFileLists(s.inc)
	s.enc.StartList(s.lists.next)
	if err := s.encodeList(files); err != nil {
		s.out.Unlock()
		return nil, err
	}
	err := s.out.W.Flush()
	s.out.Unlock()
	if err != nil {
		return nil, err
	}
	s.stats.FlistXferTime = time.Since(xferStart)
	s.stats.FlistSize = s.counter.written.Load() - before

	flist.Sort(files, s.proto)
	l := s.lists.add(files, s.firstParent(files))
	s.countFiles(files)
	if s.opts.ListOnly && !s.server {
		for _, f := range files {
			s.log.print(listLine(f, s.opts.HumanReadable))
		}
	}
	if s.inc {
		s.exp = newExpander(sc, s.lists, l)
		if err := s.sendFirstExtra(files); err != nil {
			return nil, err
		}
	}
	return files, nil
}

// encodeList writes the entries of one file list and its end marker. The
// caller holds the output lock.
func (s *session) encodeList(files []*flist.File) error {
	for _, f := range files {
		if err := s.enc.Encode(f); err != nil {
			return err
		}
	}
	ioErr := s.ioErr.Load()
	if s.opts.IgnoreErrors {
		ioErr = 0
	}
	if s.enc.End(ioErr) {
		s.out.W.Flush()
		s.out.Mux.WriteMsg(wire.MsgIOError, le32bytes(uint32(ioErr)))
	}
	return s.out.W.Err()
}

// firstParent is the directory-table index of the first list's parent: the
// "." entry of a transfer root with incremental recursion, else none.
func (s *session) firstParent(files []*flist.File) int32 {
	if s.inc {
		for _, f := range files {
			if f.Flags&flist.FlagCleared == 0 {
				if f.Name == "." {
					return 0
				}
				break
			}
		}
	}
	return -1
}

// recvFileList reads the sender's first file list and returns it sorted
// and cleaned, with the sender's uid and gid names.
func (s *session) recvFileList() (files []*flist.File, users, groups map[uint32]string, err error) {
	before := s.counter.read.Load()
	s.dec = flist.NewDecoder(s.in, s.flistConfig())
	s.lists = newFileLists(s.inc)
	s.dec.StartList(s.lists.next)
	files, err = s.decodeList()
	if err != nil {
		return nil, nil, nil, err
	}
	trailerErr, err := s.dec.ReadTrailer()
	if err != nil {
		return nil, nil, nil, err
	}
	if !s.opts.IgnoreErrors {
		s.ioErr.Or(trailerErr & ioErrValidMask)
	}
	s.stats.FlistSize = s.counter.read.Load() - before
	s.lists.add(files, s.firstParent(files))
	s.countFiles(files)
	return files, s.dec.Users, s.dec.Groups, nil
}

// decodeList reads the entries of one file list up to its end marker and
// returns them sorted and cleaned.
func (s *session) decodeList() ([]*flist.File, error) {
	var files []*flist.File
	for {
		f, e, err := s.dec.Decode()
		if err != nil {
			return nil, err
		}
		if f == nil {
			if !s.opts.IgnoreErrors {
				s.ioErr.Or(e & ioErrValidMask)
			}
			break
		}
		files = append(files, f)
	}
	flist.Sort(files, s.proto)
	flist.Clean(files)
	return files, nil
}

// recvSubList handles an index announcing a further file list (incremental
// recursion): NdxFlistEOF, or the list of a directory's contents.
func (s *session) recvSubList(ndx int32) error {
	if ndx == wire.NdxFlistEOF {
		s.lists.setEOF()
		return nil
	}
	dirNdx := wire.NdxFlistOffset - ndx
	d := s.lists.dir(dirNdx)
	if d == nil || s.lists.done() {
		return fmt.Errorf("%w: invalid directory index %d", wire.ErrProtocol, dirNdx)
	}
	s.dec.StartList(s.lists.next)
	files, err := s.decodeList()
	if err != nil {
		return err
	}
	prefix := d.Name + "/"
	for _, f := range files {
		if !strings.HasPrefix(f.Name, prefix) || strings.Contains(f.Name[len(prefix):], "/") {
			return fmt.Errorf("%w: invalid path %q from sender in the list of %q", wire.ErrProtocol, f.Name, d.Name)
		}
	}
	l := s.lists.add(files, dirNdx)
	s.countFiles(files)
	if s.onSubList != nil {
		s.onSubList(l)
	}
	return nil
}

func (s *session) countFiles(files []*flist.File) {
	st := &s.stats
	for _, f := range files {
		if f.Flags&flist.FlagCleared != 0 {
			continue
		}
		st.NumFiles++
		switch {
		case f.IsDir():
			st.NumDirs++
		case f.IsLink():
			st.NumLinks++
			st.TotalSize += f.Size
		case f.IsDevice():
			st.NumDevices++
		case f.IsSpecial():
			st.NumSpecials++
		default:
			st.NumRegFiles++
			st.TotalSize += f.Size
		}
	}
}

var (
	idNames sync.Map // "u1000" -> name
)

func userName(id uint32) string {
	key := "u" + strconv.FormatUint(uint64(id), 10)
	if v, ok := idNames.Load(key); ok {
		return v.(string)
	}
	name := ""
	if u, err := user.LookupId(strconv.FormatUint(uint64(id), 10)); err == nil {
		name = u.Username
	}
	idNames.Store(key, name)
	return name
}

func groupName(id uint32) string {
	key := "g" + strconv.FormatUint(uint64(id), 10)
	if v, ok := idNames.Load(key); ok {
		return v.(string)
	}
	name := ""
	if g, err := user.LookupGroupId(strconv.FormatUint(uint64(id), 10)); err == nil {
		name = g.Name
	}
	idNames.Store(key, name)
	return name
}

// startLocalServer runs the server half of a local transfer in this
// process. The halves talk over OS pipes, whose kernel buffering lets both
// sides write before reading, as the protocol requires.
func startLocalServer(ctx context.Context, o *Options, push bool, paths []string, stderr io.Writer) (transport.Conn, chan error, error) {
	args := serverArgs(o, push)
	if o.WholeFile != nil && !*o.WholeFile {
		args = append(args, "--no-W") // a local server would otherwise default to whole files
	}
	args = append(args, ".")
	args = append(args, paths...)
	c2sR, c2sW, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	s2cR, s2cW, err := os.Pipe()
	if err != nil {
		c2sR.Close()
		c2sW.Close()
		return nil, nil, err
	}
	transport.GrowPipe(c2sR)
	transport.GrowPipe(s2cR)
	done := make(chan error, 1)
	go func() {
		err := serve(ctx, args, &pipeConn{r: c2sR, w: s2cW}, stderr, true)
		s2cW.Close()
		c2sR.Close()
		done <- err
	}()
	return &localConn{r: s2cR, w: c2sW, done: done}, done, nil
}

type pipeConn struct {
	r, w *os.File
}

func (p *pipeConn) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *pipeConn) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *pipeConn) Close() error {
	p.r.Close()
	return p.w.Close()
}

type localConn struct {
	r, w *os.File
	done chan error

	closeOnce sync.Once
}

func (c *localConn) Read(b []byte) (int, error)  { return c.r.Read(b) }
func (c *localConn) Write(b []byte) (int, error) { return c.w.Write(b) }
func (c *localConn) CloseWrite() error           { return c.w.Close() }
func (c *localConn) Wait() error                 { return <-c.done }
func (c *localConn) Close() error {
	c.closeOnce.Do(func() {
		c.w.Close()
		c.r.Close()
	})
	return nil
}

// Stdio is the standard streams of a command.
type Stdio struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

type stdioConn struct {
	io.Reader
	io.Writer
}

// Main runs rsync with command-line arguments args (without the program
// name) and returns the process exit code. It is the whole rsync command:
// a program can mount it as a subcommand.
func Main(ctx context.Context, args []string, stdio Stdio) int {
	o, operands, err := ParseArgs(args)
	switch {
	case errors.Is(err, ErrHelp):
		io.WriteString(stdio.Stdout, usage)
		return 0
	case errors.Is(err, ErrVersion):
		fmt.Fprintf(stdio.Stdout, "rsync (xfer) %s, protocol version %d\n", Version, ProtocolVersion)
		return 0
	case err != nil:
		fmt.Fprintf(stdio.Stderr, "rsync: %v\nrsync error: syntax or usage error (code 1)\n", err)
		return ExitSyntax
	}
	if o.Server {
		for _, s := range []any{stdio.Stdin, stdio.Stdout} {
			if f, ok := s.(*os.File); ok {
				transport.GrowPipe(f)
			}
		}
		err := Serve(ctx, args, stdioConn{stdio.Stdin, stdio.Stdout}, stdio.Stderr)
		if err != nil && !bytes.Contains([]byte(err.Error()), []byte("remote side exited")) {
			fmt.Fprintf(stdio.Stderr, "rsync: %v\n", err)
		}
		return ExitCode(err)
	}
	c := &Client{Stdout: stdio.Stdout, Stderr: stdio.Stderr}
	_, err = c.Run(ctx, o, operands)
	if err != nil {
		code := ExitCode(err)
		if ee, ok := err.(*ExitError); ok && ee.Err == nil {
			fmt.Fprintf(stdio.Stderr, "rsync error: %s (code %d)\n", codeText(code), code)
			return code
		}
		msg := err.Error()
		if !strings.HasPrefix(msg, "rsync") {
			msg = "rsync: " + msg
		}
		fmt.Fprintf(stdio.Stderr, "%s\nrsync error: %s (code %d)\n", msg, codeText(code), code)
		return code
	}
	return 0
}

func codeText(code int) string {
	switch code {
	case ExitSyntax:
		return "syntax or usage error"
	case ExitProtocol:
		return "protocol incompatibility"
	case ExitFileSelect:
		return "errors selecting input/output files, dirs"
	case ExitUnsupported:
		return "requested action not supported"
	case ExitStartClient:
		return "error starting client-server protocol"
	case ExitFileIO:
		return "error in file IO"
	case ExitStreamIO:
		return "error in rsync protocol data stream"
	case ExitPartial:
		return "some files/attrs were not transferred (see previous errors)"
	case ExitVanished:
		return "some files vanished before they could be transferred"
	case ExitDeleteLimit:
		return "the --max-delete limit stopped deletions"
	case ExitTimeout:
		return "timeout in data send/receive"
	case ExitConTimeout:
		return "timeout waiting for daemon connection"
	case ExitSignal:
		return "received SIGINT, SIGTERM, or SIGHUP"
	}
	return "unexplained error"
}

// Version is the version of this implementation.
var Version = "0.1.0-dev"

const usage = `xfer rsync: a fast, versatile, remote (and local) file-copying tool,
compatible with rsync protocol versions 27 to 31.

Usage: rsync [OPTION]... SRC [SRC]... DEST
  or   rsync [OPTION]... SRC [SRC]... [USER@]HOST:DEST
  or   rsync [OPTION]... [USER@]HOST:SRC [DEST]

Common options:
 -v, --verbose               increase verbosity
 -q, --quiet                 suppress non-error messages
 -c, --checksum              skip based on checksum, not mod-time & size
 -a, --archive               archive mode is -rlptgoD (no -A,-X,-U,-N,-H)
 -r, --recursive             recurse into directories
 -R, --relative              use relative path names
 -u, --update                skip files that are newer on the receiver
 -d, --dirs                  transfer directories without recursing
 -l, --links                 copy symlinks as symlinks
 -L, --copy-links            transform symlink into referent file/dir
 -H, --hard-links            preserve hard links
 -p, --perms                 preserve permissions
 -E, --executability         preserve executability
 -o, --owner                 preserve owner (super-user only)
 -g, --group                 preserve group
 -D                          same as --devices --specials
 -t, --times                 preserve modification times
 -n, --dry-run               perform a trial run with no changes made
 -W, --whole-file            copy files whole (w/o delta-xfer algorithm)
 -x, --one-file-system       don't cross filesystem boundaries
 -e, --rsh=COMMAND           specify the remote shell to use
     --rsync-path=PROGRAM    specify the rsync to run on remote machine
     --existing              skip creating new files on receiver
     --ignore-existing       skip updating files that exist on receiver
     --remove-source-files   sender removes synchronized files (non-dir)
     --delete                delete extraneous files from dest dirs
     --delete-before/-during/-delay/-after, --delete-excluded
     --max-delete=NUM        don't delete more than NUM files
     --max-size=SIZE, --min-size=SIZE
     --size-only             skip files that match in size
 -I, --ignore-times          don't skip files that match size and time
 -z, --compress              compress file data during the transfer
     --compress-choice=STR   choose the compression algorithm (zstd, zlibx, zlib)
     --checksum-choice=STR   choose the checksum algorithm
 -f, --filter=RULE           add a file-filtering RULE
 -F                          same as --filter='dir-merge /.rsync-filter'
     --exclude=PATTERN, --include=PATTERN, --exclude-from=FILE, --include-from=FILE
 -C, --cvs-exclude           auto-ignore files in the same way CVS does
     --stats                 give some file-transfer stats
 -h, --human-readable        output numbers in a human-readable format
     --progress              show progress during transfer
 -P                          same as --partial --progress
 -i, --itemize-changes       output a change-summary for all updates
     --out-format=FORMAT     output updates using the specified FORMAT
     --list-only             list the files instead of copying them
     --bwlimit=RATE          limit socket I/O bandwidth
     --mkpath                create destination's missing path components
 -s, --protect-args          no space-splitting; wildcard chars only
     --numeric-ids           don't map uid/gid values by user/group name
     --fips                  use only FIPS-acceptable algorithms
     --version               print the version
     --help                  show this help
`
