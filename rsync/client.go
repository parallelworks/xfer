// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package rsync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/parallelworks/xfer/rsync/internal/filter"
	"github.com/parallelworks/xfer/transport"
)

// Client runs transfers. The zero value runs remote commands with ssh (or
// the -e/--rsh option) and writes output to os.Stdout and os.Stderr.
type Client struct {
	// Transport starts the remote rsync. If nil, a transport.Command is
	// built from the --rsh option, $RSYNC_RSH, or "ssh".
	Transport transport.Transport

	// Stdout and Stderr receive progress and diagnostics.
	Stdout, Stderr io.Writer

	// Stdin is read for --files-from=-; nil means os.Stdin.
	Stdin io.Reader

	// Events, if set, receives the transfer's progress as structured
	// events, whatever the output options. It is called from the
	// transfer's goroutines, one call at a time, and should return
	// quickly: the transfer waits for it.
	Events func(Event)
}

// location is a parsed SRC or DEST operand.
type location struct {
	host string // "" for a local path
	path string
}

func parseLocation(arg string) (location, error) {
	if strings.HasPrefix(arg, "rsync://") {
		return location{}, exitErrorf(ExitUnsupported, "rsync daemon URLs are not supported: %s", arg)
	}
	colon := strings.IndexByte(arg, ':')
	slash := strings.IndexByte(arg, '/')
	if colon <= 0 || (slash >= 0 && slash < colon) {
		return location{path: arg}, nil
	}
	if strings.HasPrefix(arg[colon:], "::") {
		return location{}, exitErrorf(ExitUnsupported, "rsync daemon paths are not supported: %s", arg)
	}
	return location{host: arg[:colon], path: arg[colon+1:]}, nil
}

// Run performs the transfer described by operands (SRC... DEST, or just
// SRC to list it) with the given options.
func (c *Client) Run(ctx context.Context, o *Options, operands []string) (*Stats, error) {
	stdout, stderr := c.Stdout, c.Stderr
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	if len(operands) == 0 {
		return nil, &UsageError{"missing source argument"}
	}
	if len(operands) == 1 {
		o.ListOnly = true
		if !o.Recursive && !o.set["dirs"] {
			o.Dirs = true
		}
		operands = append(operands, "")
	}

	srcArgs, destArg := operands[:len(operands)-1], operands[len(operands)-1]
	var srcs []location
	for i, a := range srcArgs {
		loc, err := parseLocation(a)
		if err != nil {
			return nil, err
		}
		if i > 0 && loc.host == "" && srcs[0].host != "" && strings.HasPrefix(a, ":") {
			loc = location{host: srcs[0].host, path: a[1:]}
		}
		if i > 0 && loc.host != srcs[0].host {
			return nil, &UsageError{"all source arguments must be on the same host"}
		}
		srcs = append(srcs, loc)
	}
	dest := location{path: destArg}
	if !o.ListOnly || destArg != "" {
		var err error
		if dest, err = parseLocation(destArg); err != nil {
			return nil, err
		}
	}
	if o.FilesFrom != "" && len(srcs) != 1 {
		return nil, &UsageError{"--files-from requires exactly one source directory"}
	}
	var filesFrom []string
	if _, remote := filesFromRemote(o.FilesFrom); o.FilesFrom != "" && !remote {
		var err error
		if filesFrom, err = openFilesFrom(o.FilesFrom, c.Stdin, o.From0); err != nil {
			return nil, err
		}
	}
	if srcs[0].host != "" && dest.host != "" {
		return nil, &UsageError{"The source and destination cannot both be remote."}
	}

	push := srcs[0].host == ""
	host := srcs[0].host
	var remotePaths []string
	if push {
		host = dest.host
		remotePaths = []string{dest.path}
	} else {
		for _, l := range srcs {
			remotePaths = append(remotePaths, l.path)
		}
	}

	log := newLogger(o, stdout, stderr)
	log.setEvents(c.Events)
	start := time.Now()

	var conn transport.Conn
	var serverDone chan error
	local := host == ""
	if local {
		var err error
		conn, serverDone, err = startLocalServer(ctx, o, push, remotePaths, stderr, &localServer{reportDeletes: c.Events != nil})
		if err != nil {
			return nil, err
		}
	} else {
		t := c.Transport
		if t == nil {
			shell := o.Rsh
			if shell == "" {
				shell = os.Getenv("RSYNC_RSH")
			}
			if shell == "" {
				shell = "ssh"
			}
			words, err := transport.ParseShell(shell)
			if err != nil {
				return nil, &UsageError{err.Error()}
			}
			t = &transport.Command{Shell: words, Stderr: stderr}
		}
		argv, _, err := remoteCommand(o, push, remotePaths)
		if err != nil {
			return nil, err
		}
		if cmd, ok := t.(*transport.Command); ok && log.lv.debug[debugCmd] > 0 {
			words := cmd.Argv(host, argv)
			log.print(fmt.Sprintf("opening connection using: %s  (%d args)\n", strings.Join(words, " "), len(words)))
		}
		conn, err = t.Start(ctx, host, argv)
		if err != nil {
			return nil, exitErrorf(ExitStartClient, "failed to start remote command: %v", err)
		}
	}
	defer conn.Close()

	s := newSession(o, conn, false, push, log)
	s.localTransfer = local
	s.filesFrom = filesFrom
	err := s.runTimed(func() error { return s.runClient(conn, srcs, dest, push) })
	if s.timedOut.Load() {
		conn.Close()
		c.wait(conn, serverDone)
		return &s.stats, err
	}
	waitErr := c.wait(conn, serverDone)
	stats := &s.stats
	// As rsync reports them: a sending client counts its own traffic until
	// the goodbye; a receiving client shows the remote sender's counts.
	switch r, w := s.ioCounts(); {
	case push && s.ended:
		stats.BytesSent, stats.BytesReceived = s.endCounts[1], s.endCounts[0]
	case !push && s.peerStats.read+s.peerStats.written > 0:
		stats.BytesSent, stats.BytesReceived = s.peerStats.read, s.peerStats.written
	default:
		stats.BytesSent, stats.BytesReceived = w, r
	}
	stats.Protocol = s.proto

	if err != nil && ctx.Err() != nil {
		return stats, &ExitError{Code: ExitSignal}
	}
	if err != nil {
		if s.peerExited || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			// The remote side failed and has explained why on stderr;
			// its exit status is the better result.
			if code := exitStatus(waitErr); code != 0 {
				return stats, &ExitError{Code: code, Err: fmt.Errorf("remote rsync exited with status %d", code)}
			}
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				err = exitErrorf(ExitStreamIO, "connection unexpectedly closed (%d bytes received so far) [%s]", stats.BytesReceived, role(push))
			}
		}
		return stats, err
	}
	if code := exitStatus(waitErr); code != 0 && waitErr != nil {
		// The remote rsync has reported its errors as they happened.
		return stats, &ExitError{Code: code}
	}
	if !o.ListOnly {
		writeSummary(stdout, stats, o, log.lv, time.Now().Unix()-start.Unix())
	}
	code := max(s.exitCode, codeForIOError(s.ioErr.Load()))
	if code != 0 {
		return stats, &ExitError{Code: code, Err: errors.New(exitMessage(code))}
	}
	return stats, nil
}

func role(push bool) string {
	if push {
		return "sender"
	}
	return "receiver"
}

func exitMessage(code int) string {
	switch code {
	case ExitPartial:
		return "some files/attrs were not transferred (see previous errors)"
	case ExitVanished:
		return "some files vanished before they could be transferred"
	case ExitDeleteLimit:
		return "the --max-delete limit stopped deletions"
	}
	return "rsync error"
}

func (c *Client) wait(conn transport.Conn, serverDone chan error) error {
	if serverDone != nil {
		conn.CloseWrite()
		return <-serverDone
	}
	return conn.Wait()
}

// exitStatus extracts a remote exit code from a transport's Wait error.
func exitStatus(err error) int {
	if err == nil {
		return 0
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	var xe *exec.ExitError
	if errors.As(err, &xe) {
		return xe.ExitCode()
	}
	var se interface{ ExitStatus() int }
	if errors.As(err, &se) {
		return se.ExitStatus()
	}
	return ExitStreamIO
}

// runClient drives the client's half of the protocol.
func (s *session) runClient(conn io.ReadWriter, srcs []location, dest location, push bool) error {
	o := s.opts
	if o.ProtectArgs && !s.localTransfer {
		_, rest, _ := remoteCommand(o, push, remotePathsOf(srcs, dest, push))
		if err := writeProtectedArgs(conn, rest); err != nil {
			return err
		}
	}
	if err := s.handshake(""); err != nil {
		return err
	}
	s.log.symlinkTimes = s.compat&cfSymlinkTimes != 0
	filters, err := buildFilters(o)
	if err != nil {
		return err
	}
	flistMsg := func(dir string) {
		if s.log.lv.info[infoFlist] == 0 {
			return
		}
		if s.inc {
			s.log.print(dir + " incremental file list\n")
		} else if dir == "sending" {
			s.log.print("building file list ... ")
		} else {
			s.log.print("receiving file list ... ")
		}
	}
	flistDone := func() {
		if s.log.lv.info[infoFlist] > 0 && !s.inc {
			s.log.print("done\n")
		}
	}
	if push {
		flistMsg("sending")
		if receiverWantsList(o, s.proto) {
			if err := s.sendFilterList(filters, true); err != nil {
				return err
			}
		}
		var paths []string
		for _, l := range srcs {
			paths = append(paths, l.path)
		}
		if o.FilesFrom != "" {
			names := s.filesFrom
			if _, remote := filesFromRemote(o.FilesFrom); remote {
				if names, err = s.recvNames(); err != nil {
					return err
				}
			}
			paths = filesFromPaths(paths[0], names)
		}
		s.onListSent = flistDone
		if _, err := s.sendFileList(paths, localRules(o, filters)); err != nil {
			return err
		}
		return s.sendFiles()
	}
	flistMsg("receiving")
	if err := s.sendFilterList(filters, false); err != nil {
		return err
	}
	if _, remote := filesFromRemote(o.FilesFrom); o.FilesFrom != "" && !remote {
		go s.forwardNames(s.filesFrom)
	}
	files, users, groups, err := s.recvFileList()
	if err != nil {
		return err
	}
	flistDone()
	if len(files) == 0 {
		return nil
	}
	return s.receiveFiles(files, users, groups, dest.path, localRules(o, filters))
}

func remotePathsOf(srcs []location, dest location, push bool) []string {
	if push {
		return []string{dest.path}
	}
	var p []string
	for _, l := range srcs {
		p = append(p, l.path)
	}
	return p
}

func receiverWantsList(o *Options, proto int) bool {
	return o.PruneEmptyDirs || (o.Delete != DeleteNone && (!o.DeleteExcluded || proto >= 29))
}

// buildFilters turns the filter options into a rule list.
func buildFilters(o *Options) (*filter.List, error) {
	var rules []filter.Rule
	if o.CVSExclude {
		rules = append(rules, filter.CVSRules()...)
	}
	for _, f := range o.Filters {
		rs, err := filter.ParseRule(f)
		if err != nil {
			return nil, &UsageError{err.Error()}
		}
		rules = append(rules, rs...)
	}
	return filter.NewList(rules), nil
}

// localRules adds the rules each side applies on its own without sending
// them: a relative --partial-dir is neither transferred nor deleted.
//
// Backups made beside their files (-b without --backup-dir) are protected
// from deletion, and so is a backup directory inside the destination.
func localRules(o *Options, l *filter.List) *filter.List {
	var extra []string
	if o.PartialDir != "" && !filepath.IsAbs(o.PartialDir) {
		extra = append(extra, "- "+strings.TrimSuffix(o.PartialDir, "/")+"/")
	}
	if o.backups() {
		if o.BackupDir == "" {
			extra = append(extra, "P *"+o.BackupSuffix())
		} else if !filepath.IsAbs(o.BackupDir) {
			extra = append(extra, "P /"+strings.Trim(path.Clean(o.BackupDir), "/")+"/")
		}
	}
	if len(extra) == 0 {
		return l
	}
	// First, so the user's rules cannot override them.
	var rules []filter.Rule
	for _, r := range extra {
		if rs, err := filter.ParseRule(r); err == nil {
			rules = append(rules, rs...)
		}
	}
	return filter.NewList(append(rules, l.Rules()...))
}

// remoteCommand builds the command that starts the server half. With
// --protect-args only the leading options go on the command line, and the
// remaining arguments are returned in stdinArgs to be sent on the remote
// command's standard input.
func remoteCommand(o *Options, push bool, paths []string) (argv, stdinArgs []string, err error) {
	rsyncPath := o.RsyncPath
	if rsyncPath == "" {
		rsyncPath = "rsync"
	}
	argv, err = transport.ParseShell(rsyncPath)
	if err != nil || len(argv) == 0 {
		return nil, nil, &UsageError{fmt.Sprintf("invalid --rsync-path %q", rsyncPath)}
	}
	args := serverArgs(o, push)
	args = append(args, ".")
	args = append(args, paths...)
	if !o.ProtectArgs {
		return append(argv, args...), nil, nil
	}
	// --server, --sender and the short-option cluster stay.
	n := 0
	for n < len(args) && (args[n] == "--server" || args[n] == "--sender") {
		n++
	}
	if n < len(args) && strings.HasPrefix(args[n], "-") && !strings.HasPrefix(args[n], "--") {
		n++
	}
	return append(argv, args[:n]...), args[n:], nil
}

// serverArgs lists the options the server needs, in rsync's order. push is
// set when the client sends files.
func serverArgs(o *Options, push bool) []string {
	args := []string{"--server"}
	if !push {
		args = append(args, "--sender")
	}
	var b strings.Builder
	b.WriteByte('-')
	if o.ProtectArgs {
		b.WriteByte('s')
	}
	for range min(o.Verbose, 9) {
		b.WriteByte('v')
	}
	flag := func(on bool, c byte) {
		if on {
			b.WriteByte(c)
		}
	}
	flag(o.Quiet, 'q')
	flag(o.Update, 'u')
	flag(o.DryRun, 'n')
	flag(o.Links, 'l')
	flag(o.Dirs && !o.Recursive && (o.set["dirs"] || o.ListOnly || (o.Delete != DeleteNone && push)), 'd')
	if push {
		flag(o.KeepDirlinks, 'K')
		flag(o.PruneEmptyDirs, 'm')
		flag(o.OmitDirTimes, 'O')
		flag(o.OmitLinkTimes, 'J')
	} else {
		flag(o.CopyLinks, 'L')
		flag(o.CopyDirlinks, 'k')
	}
	flag(o.WholeFile != nil && *o.WholeFile, 'W')
	flag(o.HardLinks, 'H')
	flag(o.Owner, 'o')
	flag(o.Group, 'g')
	flag(o.Devices, 'D')
	flag(o.Times, 't')
	flag(o.Perms, 'p')
	flag(!o.Perms && o.Executability && push, 'E')
	flag(o.Recursive, 'r')
	flag(o.Checksum, 'c')
	flag(o.CVSExclude, 'C')
	flag(o.IgnoreTimes, 'I')
	flag(o.Relative, 'R')
	for range o.OneFileSystem {
		b.WriteByte('x')
	}
	flag(o.Sparse, 'S')
	flag(o.Compress && (o.CompressChoice == "" || o.CompressChoice == "zlib"), 'z')
	if o.Protocol == 0 || o.Protocol >= 30 {
		caps := clientCaps
		if allowIncRecurse(o, push) {
			caps = "." + "i" + caps[1:]
		}
		b.WriteString("e" + caps)
	}
	if b.Len() > 1 {
		args = append(args, b.String())
	}

	if o.ListOnly && o.set["list-only"] {
		args = append(args, "--list-only")
	}
	if o.Dirs && !o.Recursive && o.Delete != DeleteNone && push {
		args = append(args, "--no-r")
	}
	if o.Compress && o.CompressLevel != 0 {
		args = append(args, "--compress-level="+strconv.Itoa(o.CompressLevel))
	}
	if o.Devices && !o.Specials {
		args = append(args, "--no-specials")
	} else if o.Specials && !o.Devices {
		args = append(args, "--specials")
	}
	if push {
		if f := newLogger(o, io.Discard, io.Discard); f.format != "" {
			switch {
			case f.hasI > 1:
				args = append(args, "--log-format=%i%I")
			case f.hasI > 0:
				args = append(args, "--log-format=%i")
			case strings.Contains(f.format, "%o"):
				args = append(args, "--log-format=%o")
			case o.Verbose == 0:
				args = append(args, "--log-format=X")
			}
		}
	}
	if o.BlockSize > 0 {
		args = append(args, "-B"+strconv.Itoa(o.BlockSize))
	}
	if o.Timeout > 0 {
		args = append(args, "--timeout="+strconv.Itoa(int(o.Timeout/time.Second)))
	}
	if o.BWLimit > 0 {
		args = append(args, "--bwlimit="+strconv.FormatInt(o.BWLimit/1024, 10))
	}
	if o.ChecksumChoice != "" && o.ChecksumChoice != "auto" && o.ChecksumChoice != "auto,auto" {
		args = append(args, "--checksum-choice="+o.ChecksumChoice)
	}
	if o.Compress {
		switch o.CompressChoice {
		case "zlibx":
			args = append(args, "--new-compress")
		case "zlib":
			args = append(args, "--old-compress")
		case "", "none":
		default:
			args = append(args, "--compress-choice="+o.CompressChoice)
		}
	}
	if push {
		switch {
		case o.MaxDelete > 0:
			args = append(args, "--max-delete="+strconv.Itoa(o.MaxDelete))
		case o.MaxDelete < 0:
			args = append(args, "--max-delete=-1")
		}
		if o.MinSize > 0 {
			args = append(args, "--min-size="+strconv.FormatInt(o.MinSize, 10))
		}
		if o.MaxSize > 0 {
			args = append(args, "--max-size="+strconv.FormatInt(o.MaxSize, 10))
		}
		switch o.Delete {
		case DeleteBefore:
			args = append(args, "--delete-before")
		case DeleteDelay:
			args = append(args, "--delete-delay")
		case DeleteDuring:
			args = append(args, "--delete-during")
		case DeleteAfter:
			args = append(args, "--delete-after")
		case DeleteAuto:
			if !o.DeleteExcluded {
				args = append(args, "--delete")
			}
		}
		if o.DeleteExcluded {
			args = append(args, "--delete-excluded")
		}
		if o.Force {
			args = append(args, "--force")
		}
		if o.Super {
			args = append(args, "--super")
		} else if o.set["super"] {
			args = append(args, "--no-super")
		}
		if o.SizeOnly {
			args = append(args, "--size-only")
		}
		if o.Preallocate {
			args = append(args, "--preallocate")
		}
		if o.Stats {
			args = append(args, "--stats")
		}
	} else {
		if o.SkipCompress != "" || o.set["skip-compress"] {
			args = append(args, "--skip-compress="+o.SkipCompress)
		}
		if o.OpenNoatime {
			args = append(args, "--open-noatime")
		}
	}
	if push && o.ModifyWindow != 0 {
		if o.ModifyWindow < 0 {
			args = append(args, "-@-1")
		} else {
			args = append(args, "--modify-window="+strconv.Itoa(int(o.ModifyWindow/time.Second)))
		}
	}
	if o.ChecksumSeed != 0 {
		args = append(args, "--checksum-seed="+strconv.Itoa(int(o.ChecksumSeed)))
	}
	if push {
		opt := [...]string{BasisCompare: "--compare-dest=", BasisCopy: "--copy-dest=", BasisLink: "--link-dest="}[o.BasisMode]
		for _, d := range o.BasisDirs {
			args = append(args, opt+d)
		}
	}
	if o.FilesFrom != "" {
		if p, remote := filesFromRemote(o.FilesFrom); remote {
			args = append(args, "--files-from="+p)
			if o.From0 {
				args = append(args, "--from0")
			}
		} else if !push {
			// We read the list and forward it to the sender.
			args = append(args, "--files-from=-", "--from0")
		}
	}
	if push && o.backups() {
		args = append(args, "--backup")
		if o.BackupDir != "" {
			args = append(args, "--backup-dir="+o.BackupDir)
		}
		if o.Suffix != "" || o.set["suffix"] {
			args = append(args, "--suffix="+o.Suffix)
		}
	}
	if push {
		switch {
		case o.PartialDir != "":
			args = append(args, "--partial-dir", o.PartialDir)
			if o.DelayUpdates {
				args = append(args, "--delay-updates")
			}
		case o.Partial:
			args = append(args, "--partial")
		}
	}
	if o.IgnoreErrors {
		args = append(args, "--ignore-errors")
	}
	if o.CopyUnsafeLinks {
		args = append(args, "--copy-unsafe-links")
	}
	if o.SafeLinks {
		args = append(args, "--safe-links")
	}
	if o.NumericIDs {
		args = append(args, "--numeric-ids")
	}
	if push {
		if o.IgnoreExisting {
			args = append(args, "--ignore-existing")
		}
		if o.Existing {
			args = append(args, "--existing")
		}
		if o.TempDir != "" {
			args = append(args, "--temp-dir", o.TempDir)
		}
		if o.Fsync {
			args = append(args, "--fsync")
		}
	}
	if o.Inplace {
		args = append(args, "--inplace")
	}
	if o.Relative && !o.ImpliedDirs {
		args = append(args, "--no-implied-dirs")
	}
	if o.RemoveSourceFiles {
		args = append(args, "--remove-source-files")
	}
	if push && o.MkPath {
		args = append(args, "--mkpath")
	}
	args = append(args, o.RemoteOptions...)
	return args
}

// writeProtectedArgs sends the arguments of a --protect-args transfer on
// the remote's standard input.
func writeProtectedArgs(w io.Writer, args []string) error {
	var b []byte
	b = append(b, "rsync\x00"...)
	for _, a := range args {
		if a == "" {
			a = "."
		}
		b = append(b, a...)
		b = append(b, 0)
	}
	b = append(b, 0)
	_, err := w.Write(b)
	return err
}
