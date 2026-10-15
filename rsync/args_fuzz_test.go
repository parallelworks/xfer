// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package rsync

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// FuzzParseArgs parses an arbitrary command line (arguments separated by
// NUL bytes). When the client accepts it, the server command line built
// from the options must parse in server mode and reproduce every option
// the server acts on.
func FuzzParseArgs(f *testing.F) {
	for _, s := range []string{
		"-avz\x00--delete\x00--exclude=x\x00-e\x00ssh\x00src/\x00host:dst",
		"--info=progress2\x00-a\x00src\x00dst",
		"-F\x00-F\x00--no-inc-recursive\x00-r\x00a\x00b",
		"-vvvhP\x00--bwlimit=1.5M\x00--partial-dir=.p\x00--delay-updates\x00a\x00b",
		"-rlptgoD\x00--no-specials\x00--modify-window=2\x00--timeout=30\x00a\x00b",
		"-aHxx\x00--numeric-ids\x00--checksum-seed=7\x00-B4096\x00a\x00b",
		"-rtz\x00--compress-level=9\x00--zc=zstd\x00--cc=xxh128\x00a\x00b",
		"-rn\x00--delete-excluded\x00--max-delete=0\x00--min-size=1k\x00--max-size=2MB-1\x00a\x00b",
		"-rLkKmOJ\x00--inplace\x00-W\x00--size-only\x00--force\x00--super\x00a\x00b",
		"-RCI\x00--no-implied-dirs\x00--safe-links\x00--copy-unsafe-links\x00-S\x00a\x00b",
		"-s\x00--remove-source-files\x00--ignore-existing\x00--existing\x00-T\x00/tmp\x00--mkpath\x00a\x00b",
		"-d\x00--list-only\x00--delete-after\x00--ignore-errors\x00-E\x00--fsync\x00host:src",
		"--old-compress\x00--skip-compress=gz/jpg\x00--stats\x00-i\x00-@-1\x00a\x00b",
		"--protocol=29\x00-a\x00--no-W\x00--del\x00--\x00-weird\x00b",
		"-f\x00- *.o\x00--filter=: /.rsync-filter\x00--include=*/\x00-M--fake\x00a\x00b",
		"--out-format=%i %n\x00--no-v\x00-q\x00-u\x00--8-bit-output\x00a\x00b",
	} {
		f.Add(s, true)
		f.Add(s, false)
	}
	f.Fuzz(func(t *testing.T, line string, push bool) {
		if len(line) > 1<<12 {
			return
		}
		args := strings.Split(line, "\x00")
		for _, a := range args {
			if strings.Contains(a, "-from") {
				return // --exclude-from/--include-from read files
			}
		}
		o, operands, err := ParseArgs(args)
		if err != nil {
			var ue *UsageError
			if o != nil || !(errors.As(err, &ue) || errors.Is(err, ErrHelp) || errors.Is(err, ErrVersion)) {
				t.Fatalf("ParseArgs(%q) = %v, %v", args, o, err)
			}
			return
		}
		if o.Server || o.Sender {
			return
		}
		// -M passes arbitrary options through; they are the user's to get right.
		o.RemoteOptions = nil

		sargs := serverArgs(o, push)
		sargs = append(sargs, ".")
		sargs = append(sargs, operands...)
		s, sops, err := ParseArgs(sargs)
		if err != nil {
			t.Fatalf("server args %q (from %q) do not parse: %v", sargs, args, err)
		}
		if !slices.Equal(sops, append([]string{"."}, operands...)) {
			t.Fatalf("server operands %q, want . %q (args %q)", sops, operands, sargs)
		}
		if !s.Server || s.Sender != !push {
			t.Fatalf("server %v sender %v for push %v", s.Server, s.Sender, push)
		}

		fail := func(name string, got, want any) {
			t.Helper()
			t.Fatalf("%s: server has %v, client %v (client %q, server %q)", name, got, want, args, sargs)
		}
		eq := func(name string, got, want any) {
			t.Helper()
			if got != want {
				fail(name, got, want)
			}
		}
		eq("verbose", s.Verbose, min(o.Verbose, 9))
		eq("quiet", s.Quiet, o.Quiet)
		eq("update", s.Update, o.Update)
		eq("dry-run", s.DryRun, o.DryRun)
		eq("links", s.Links, o.Links)
		eq("recursive", s.Recursive, o.Recursive)
		eq("times", s.Times, o.Times)
		eq("perms", s.Perms, o.Perms)
		eq("owner", s.Owner, o.Owner)
		eq("group", s.Group, o.Group)
		eq("devices", s.Devices, o.Devices)
		eq("specials", s.Specials, o.Specials)
		eq("hard-links", s.HardLinks, o.HardLinks)
		eq("checksum", s.Checksum, o.Checksum)
		eq("cvs-exclude", s.CVSExclude, o.CVSExclude)
		eq("ignore-times", s.IgnoreTimes, o.IgnoreTimes)
		eq("relative", s.Relative, o.Relative)
		eq("one-file-system", s.OneFileSystem, o.OneFileSystem)
		eq("sparse", s.Sparse, o.Sparse)
		eq("protect-args", s.ProtectArgs, o.ProtectArgs)
		eq("whole-file", s.WholeFile != nil && *s.WholeFile, o.WholeFile != nil && *o.WholeFile)
		eq("list-only", s.ListOnly, o.ListOnly && o.IsSet("list-only"))
		eq("block-size", s.BlockSize, o.BlockSize)
		eq("timeout", s.Timeout, o.Timeout)
		eq("checksum-seed", s.ChecksumSeed, o.ChecksumSeed)
		eq("inplace", s.Inplace, o.Inplace)
		eq("ignore-errors", s.IgnoreErrors, o.IgnoreErrors)
		eq("copy-unsafe-links", s.CopyUnsafeLinks, o.CopyUnsafeLinks)
		eq("safe-links", s.SafeLinks, o.SafeLinks)
		eq("numeric-ids", s.NumericIDs, o.NumericIDs)
		eq("remove-source-files", s.RemoveSourceFiles, o.RemoveSourceFiles)
		if o.Relative {
			eq("implied-dirs", s.ImpliedDirs, o.ImpliedDirs)
		}
		if o.BWLimit > 0 {
			eq("bwlimit", s.BWLimit, o.BWLimit/1024*1024)
		}
		switch o.ChecksumChoice {
		case "", "auto", "auto,auto":
		default:
			eq("checksum-choice", s.ChecksumChoice, o.ChecksumChoice)
		}
		if o.CompressChoice != "none" {
			eq("compress", s.Compress, o.Compress)
			if o.Compress {
				eq("compress-choice", s.CompressChoice, o.CompressChoice)
				eq("compress-level", s.CompressLevel, o.CompressLevel)
			}
		}
		if push {
			eq("keep-dirlinks", s.KeepDirlinks, o.KeepDirlinks)
			eq("prune-empty-dirs", s.PruneEmptyDirs, o.PruneEmptyDirs)
			eq("omit-dir-times", s.OmitDirTimes, o.OmitDirTimes)
			eq("omit-link-times", s.OmitLinkTimes, o.OmitLinkTimes)
			if !o.Perms {
				eq("executability", s.Executability, o.Executability)
			}
			eq("delete", s.Delete, o.Delete)
			eq("delete-excluded", s.DeleteExcluded, o.DeleteExcluded)
			eq("force", s.Force, o.Force)
			eq("super", s.Super, o.Super)
			eq("size-only", s.SizeOnly, o.SizeOnly)
			eq("stats", s.Stats, o.Stats)
			eq("max-delete", s.MaxDelete, o.MaxDelete)
			eq("min-size", s.MinSize, max(o.MinSize, 0))
			eq("max-size", s.MaxSize, max(o.MaxSize, 0))
			eq("modify-window", s.ModifyWindow, o.ModifyWindow)
			eq("partial", s.Partial, o.Partial)
			eq("partial-dir", s.PartialDir, o.PartialDir)
			eq("delay-updates", s.DelayUpdates, o.DelayUpdates && o.PartialDir != "")
			eq("ignore-existing", s.IgnoreExisting, o.IgnoreExisting)
			eq("existing", s.Existing, o.Existing)
			eq("temp-dir", s.TempDir, o.TempDir)
			eq("fsync", s.Fsync, o.Fsync)
			eq("mkpath", s.MkPath, o.MkPath)
		} else {
			eq("copy-links", s.CopyLinks, o.CopyLinks)
			eq("copy-dirlinks", s.CopyDirlinks, o.CopyDirlinks)
			eq("skip-compress", s.SkipCompress, o.SkipCompress)
		}
	})
}
