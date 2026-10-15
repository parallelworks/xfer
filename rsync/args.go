// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package rsync

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// UsageError is returned by ParseArgs for command lines rsync would reject.
type UsageError struct{ msg string }

// Error returns the usage message.
func (e *UsageError) Error() string { return e.msg }

func usagef(format string, args ...any) error {
	return &UsageError{fmt.Sprintf(format, args...)}
}

// ErrHelp and ErrVersion are returned by ParseArgs when -h/--help or
// --version is given. Callers print usage or version and exit 0.
var (
	ErrHelp    = errors.New("rsync: help requested")
	ErrVersion = errors.New("rsync: version requested")
)

type argKind int

const (
	argNone argKind = iota
	argString
)

type option struct {
	long  []string // first is canonical
	short rune
	arg   argKind
	// neg lists extra negation names beyond "no-<long>"; noNeg disables
	// negation entirely.
	noNeg bool
	set   func(o *Options, v string) error
	unset func(o *Options)
}

func boolOpt(long string, short rune, p func(o *Options) *bool) option {
	return option{
		long: []string{long}, short: short,
		set:   func(o *Options, _ string) error { *p(o) = true; return nil },
		unset: func(o *Options) { *p(o) = false },
	}
}

func strOpt(long string, short rune, p func(o *Options) *string) option {
	return option{
		long: []string{long}, short: short, arg: argString, noNeg: true,
		set: func(o *Options, v string) error { *p(o) = v; return nil },
	}
}

func unsupported(long string, short rune, arg argKind) option {
	return option{
		long: []string{long}, short: short, arg: arg, noNeg: true,
		set: func(*Options, string) error {
			return usagef("--%s is not supported by this rsync", long)
		},
	}
}

func basisOpt(long string, mode BasisMode) option {
	return option{long: []string{long}, arg: argString, noNeg: true, set: func(o *Options, v string) error {
		if len(o.BasisDirs) > 0 && o.BasisMode != mode {
			return usagef("You may not mix --compare-dest, --copy-dest, and --link-dest.")
		}
		if len(o.BasisDirs) >= 20 {
			return usagef("ERROR: at most 20 --%s args may be specified", long)
		}
		o.BasisDirs, o.BasisMode = append(o.BasisDirs, v), mode
		return nil
	}}
}

// ignored options are accepted for compatibility and have no effect.
func ignored(long string, short rune, arg argKind) option {
	return option{long: []string{long}, short: short, arg: arg,
		set: func(*Options, string) error { return nil }, unset: func(*Options) {}}
}

var optionTable = []option{
	{long: []string{"help"}, noNeg: true, set: func(*Options, string) error { return ErrHelp }},
	{long: []string{"version"}, short: 'V', noNeg: true, set: func(*Options, string) error { return ErrVersion }},
	{long: []string{"verbose"}, short: 'v',
		set: func(o *Options, _ string) error { o.Verbose++; return nil }, unset: func(o *Options) { o.Verbose = 0 }},
	{long: []string{"quiet"}, short: 'q', noNeg: true, set: func(o *Options, _ string) error { o.Quiet = true; return nil }},
	{long: []string{"info"}, arg: argString, noNeg: true, set: func(o *Options, v string) error {
		var scratch [numInfo]int
		if err := applyWords(v, infoNames(), scratch[:], "info"); err != nil {
			return err
		}
		o.Info = append(o.Info, v)
		return nil
	}},
	{long: []string{"debug"}, arg: argString, noNeg: true, set: func(o *Options, v string) error {
		var scratch [numDebug]int
		if err := applyWords(v, debugNames(), scratch[:], "debug"); err != nil {
			return err
		}
		o.Debug = append(o.Debug, v)
		return nil
	}},
	ignored("motd", 0, argNone),
	boolOpt("msgs2stderr", 0, func(o *Options) *bool { return &o.Msgs2Stderr }),
	boolOpt("stats", 0, func(o *Options) *bool { return &o.Stats }),
	{long: []string{"human-readable"}, short: 'h',
		set: func(o *Options, _ string) error { o.HumanReadable++; return nil }, unset: func(o *Options) { o.HumanReadable = 0 }},
	boolOpt("dry-run", 'n', func(o *Options) *bool { return &o.DryRun }),
	{long: []string{"archive"}, short: 'a', noNeg: true, set: func(o *Options, _ string) error {
		o.Recursive, o.Links, o.Perms, o.Times, o.Group, o.Owner, o.Devices, o.Specials = true, true, true, true, true, true, true, true
		return nil
	}},
	boolOpt("recursive", 'r', func(o *Options) *bool { return &o.Recursive }),
	boolOpt("dirs", 'd', func(o *Options) *bool { return &o.Dirs }),
	boolOpt("perms", 'p', func(o *Options) *bool { return &o.Perms }),
	boolOpt("executability", 'E', func(o *Options) *bool { return &o.Executability }),
	unsupported("acls", 'A', argNone),
	unsupported("xattrs", 'X', argNone),
	boolOpt("times", 't', func(o *Options) *bool { return &o.Times }),
	unsupported("atimes", 'U', argNone),
	boolOpt("open-noatime", 0, func(o *Options) *bool { return &o.OpenNoatime }),
	unsupported("crtimes", 'N', argNone),
	boolOpt("omit-dir-times", 'O', func(o *Options) *bool { return &o.OmitDirTimes }),
	boolOpt("omit-link-times", 'J', func(o *Options) *bool { return &o.OmitLinkTimes }),
	{long: []string{"modify-window"}, short: '@', arg: argString, noNeg: true, set: func(o *Options, v string) error {
		// rsync keeps the window in an int; larger values are rejected.
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			return usagef("invalid --modify-window value %q", v)
		}
		if n < 0 { // rsync: -1 means nanosecond precision is ignored entirely
			o.ModifyWindow = -1
		} else {
			o.ModifyWindow = time.Duration(n) * time.Second
		}
		return nil
	}},
	boolOpt("super", 0, func(o *Options) *bool { return &o.Super }),
	unsupported("fake-super", 0, argNone),
	boolOpt("owner", 'o', func(o *Options) *bool { return &o.Owner }),
	boolOpt("group", 'g', func(o *Options) *bool { return &o.Group }),
	{long: []string{"D"}, short: 'D', set: func(o *Options, _ string) error {
		o.Devices, o.Specials = true, true
		return nil
	}, unset: func(o *Options) { o.Devices, o.Specials = false, false }},
	boolOpt("devices", 0, func(o *Options) *bool { return &o.Devices }),
	boolOpt("specials", 0, func(o *Options) *bool { return &o.Specials }),
	unsupported("copy-devices", 0, argNone),
	unsupported("write-devices", 0, argNone),
	boolOpt("links", 'l', func(o *Options) *bool { return &o.Links }),
	boolOpt("copy-links", 'L', func(o *Options) *bool { return &o.CopyLinks }),
	boolOpt("copy-unsafe-links", 0, func(o *Options) *bool { return &o.CopyUnsafeLinks }),
	boolOpt("safe-links", 0, func(o *Options) *bool { return &o.SafeLinks }),
	unsupported("munge-links", 0, argNone),
	boolOpt("copy-dirlinks", 'k', func(o *Options) *bool { return &o.CopyDirlinks }),
	boolOpt("keep-dirlinks", 'K', func(o *Options) *bool { return &o.KeepDirlinks }),
	boolOpt("hard-links", 'H', func(o *Options) *bool { return &o.HardLinks }),
	boolOpt("relative", 'R', func(o *Options) *bool { return &o.Relative }),
	{long: []string{"implied-dirs", "i-d"},
		set:   func(o *Options, _ string) error { o.ImpliedDirs = true; return nil },
		unset: func(o *Options) { o.ImpliedDirs = false }},
	{long: []string{"chmod"}, arg: argString, noNeg: true, set: func(o *Options, v string) error {
		o.Chmod = append(o.Chmod, v)
		return nil
	}},
	boolOpt("ignore-times", 'I', func(o *Options) *bool { return &o.IgnoreTimes }),
	boolOpt("size-only", 0, func(o *Options) *bool { return &o.SizeOnly }),
	{long: []string{"one-file-system"}, short: 'x',
		set: func(o *Options, _ string) error { o.OneFileSystem++; return nil }, unset: func(o *Options) { o.OneFileSystem = 0 }},
	boolOpt("update", 'u', func(o *Options) *bool { return &o.Update }),
	{long: []string{"existing", "ignore-non-existing"}, noNeg: true, set: func(o *Options, _ string) error { o.Existing = true; return nil }},
	boolOpt("ignore-existing", 0, func(o *Options) *bool { return &o.IgnoreExisting }),
	{long: []string{"max-size"}, arg: argString, noNeg: true, set: func(o *Options, v string) error {
		n, err := ParseSize(v, 1)
		o.MaxSize = n
		return err
	}},
	{long: []string{"min-size"}, arg: argString, noNeg: true, set: func(o *Options, v string) error {
		n, err := ParseSize(v, 1)
		o.MinSize = n
		return err
	}},
	ignored("max-alloc", 0, argString),
	boolOpt("sparse", 'S', func(o *Options) *bool { return &o.Sparse }),
	boolOpt("preallocate", 0, func(o *Options) *bool { return &o.Preallocate }),
	boolOpt("inplace", 0, func(o *Options) *bool { return &o.Inplace }),
	unsupported("append", 0, argNone),
	unsupported("append-verify", 0, argNone),
	{long: []string{"del"}, noNeg: true, set: func(o *Options, _ string) error { o.Delete = DeleteDuring; return nil }},
	{long: []string{"delete"}, noNeg: true, set: func(o *Options, _ string) error {
		if o.Delete == DeleteNone {
			o.Delete = DeleteAuto
		}
		return nil
	}},
	{long: []string{"delete-before"}, noNeg: true, set: func(o *Options, _ string) error { o.Delete = DeleteBefore; return nil }},
	{long: []string{"delete-during"}, noNeg: true, set: func(o *Options, _ string) error { o.Delete = DeleteDuring; return nil }},
	{long: []string{"delete-delay"}, noNeg: true, set: func(o *Options, _ string) error { o.Delete = DeleteDelay; return nil }},
	{long: []string{"delete-after"}, noNeg: true, set: func(o *Options, _ string) error { o.Delete = DeleteAfter; return nil }},
	{long: []string{"delete-excluded"}, noNeg: true, set: func(o *Options, _ string) error {
		o.DeleteExcluded = true
		if o.Delete == DeleteNone {
			o.Delete = DeleteAuto
		}
		return nil
	}},
	unsupported("delete-missing-args", 0, argNone),
	unsupported("ignore-missing-args", 0, argNone),
	{long: []string{"remove-source-files", "remove-sent-files"}, noNeg: true, set: func(o *Options, _ string) error {
		o.RemoveSourceFiles = true
		return nil
	}},
	boolOpt("force", 0, func(o *Options) *bool { return &o.Force }),
	boolOpt("ignore-errors", 0, func(o *Options) *bool { return &o.IgnoreErrors }),
	{long: []string{"max-delete"}, arg: argString, noNeg: true, set: func(o *Options, v string) error {
		n, err := strconv.Atoi(v)
		if err != nil {
			return usagef("invalid --max-delete value %q", v)
		}
		if n == 0 {
			n = -1
		}
		o.MaxDelete = n
		return nil
	}},
	{long: []string{"filter"}, short: 'f', arg: argString, noNeg: true, set: func(o *Options, v string) error {
		o.Filters = append(o.Filters, v)
		return nil
	}},
	{long: []string{"F"}, short: 'F', noNeg: true, set: func(o *Options, _ string) error {
		// -F once adds a dir-merge of .rsync-filter; twice also hides
		// the .rsync-filter files themselves.
		if o.set["F"] {
			o.Filters = append(o.Filters, "- .rsync-filter")
		} else {
			o.Filters = append(o.Filters, ": /.rsync-filter")
		}
		return nil
	}},
	{long: []string{"exclude"}, arg: argString, noNeg: true, set: func(o *Options, v string) error {
		o.Filters = append(o.Filters, "- "+v)
		return nil
	}},
	{long: []string{"include"}, arg: argString, noNeg: true, set: func(o *Options, v string) error {
		o.Filters = append(o.Filters, "+ "+v)
		return nil
	}},
	{long: []string{"exclude-from"}, arg: argString, noNeg: true, set: func(o *Options, v string) error {
		return o.addRulesFrom(v, "- ")
	}},
	{long: []string{"include-from"}, arg: argString, noNeg: true, set: func(o *Options, v string) error {
		return o.addRulesFrom(v, "+ ")
	}},
	{long: []string{"cvs-exclude"}, short: 'C', noNeg: true, set: func(o *Options, _ string) error { o.CVSExclude = true; return nil }},
	{long: []string{"inc-recursive", "i-r"},
		set:   func(o *Options, _ string) error { o.NoIncRecursive = false; return nil },
		unset: func(o *Options) { o.NoIncRecursive = true }},
	{long: []string{"whole-file"}, short: 'W',
		set:   func(o *Options, _ string) error { t := true; o.WholeFile = &t; return nil },
		unset: func(o *Options) { f := false; o.WholeFile = &f }},
	boolOpt("checksum", 'c', func(o *Options) *bool { return &o.Checksum }),
	{long: []string{"checksum-choice", "cc"}, arg: argString, noNeg: true, set: func(o *Options, v string) error {
		o.ChecksumChoice = v
		return nil
	}},
	{long: []string{"block-size"}, short: 'B', arg: argString, noNeg: true, set: func(o *Options, v string) error {
		n, err := ParseSize(v, 1)
		if err != nil {
			return err
		}
		if n < 0 || n > 1<<17 {
			return usagef("--block-size=%s is out of range", v)
		}
		o.BlockSize = int(n)
		return nil
	}},
	basisOpt("compare-dest", BasisCompare),
	basisOpt("copy-dest", BasisCopy),
	basisOpt("link-dest", BasisLink),
	unsupported("fuzzy", 'y', argNone),
	{long: []string{"compress"}, short: 'z',
		set:   func(o *Options, _ string) error { o.Compress = true; return nil },
		unset: func(o *Options) { o.Compress = false }},
	{long: []string{"old-compress"}, noNeg: true, set: func(o *Options, _ string) error {
		o.Compress, o.CompressChoice = true, "zlib"
		return nil
	}},
	{long: []string{"new-compress"}, noNeg: true, set: func(o *Options, _ string) error {
		o.Compress, o.CompressChoice = true, "zlibx"
		return nil
	}},
	{long: []string{"compress-choice", "zc"}, arg: argString, noNeg: true, set: func(o *Options, v string) error {
		o.CompressChoice = v
		o.Compress = v != "none"
		return nil
	}},
	strOpt("skip-compress", 0, func(o *Options) *string { return &o.SkipCompress }),
	{long: []string{"compress-level", "zl"}, arg: argString, noNeg: true, set: func(o *Options, v string) error {
		n, err := strconv.Atoi(v)
		if err != nil {
			return usagef("invalid --compress-level value %q", v)
		}
		o.CompressLevel = n
		return nil
	}},
	ignored("compress-threads", 0, argString),
	ignored("zt", 0, argString),
	boolOpt("progress", 0, func(o *Options) *bool { return &o.Progress }),
	{long: []string{"P"}, short: 'P', noNeg: true, set: func(o *Options, _ string) error {
		o.Progress, o.Partial = true, true
		return nil
	}},
	boolOpt("partial", 0, func(o *Options) *bool { return &o.Partial }),
	{long: []string{"partial-dir"}, arg: argString, noNeg: true, set: func(o *Options, v string) error {
		o.PartialDir, o.Partial = v, true
		return nil
	}},
	boolOpt("delay-updates", 0, func(o *Options) *bool { return &o.DelayUpdates }),
	ignored("delay-symlinks", 0, argNone),
	boolOpt("prune-empty-dirs", 'm', func(o *Options) *bool { return &o.PruneEmptyDirs }),
	unsupported("log-file", 0, argString),
	unsupported("log-file-format", 0, argString),
	{long: []string{"out-format", "log-format"}, arg: argString, noNeg: true, set: func(o *Options, v string) error {
		o.OutFormat = v
		return nil
	}},
	{long: []string{"itemize-changes"}, short: 'i',
		set: func(o *Options, _ string) error { o.ItemizeChanges++; return nil }, unset: func(o *Options) { o.ItemizeChanges = 0 }},
	{long: []string{"bwlimit"}, arg: argString, set: func(o *Options, v string) error {
		n, err := ParseSize(v, 1024)
		o.BWLimit = n
		return err
	}, unset: func(o *Options) { o.BWLimit = 0 }},
	boolOpt("backup", 'b', func(o *Options) *bool { return &o.Backup }),
	{long: []string{"backup-dir"}, arg: argString, noNeg: true, set: func(o *Options, v string) error {
		o.BackupDir, o.Backup = v, true
		return nil
	}},
	strOpt("suffix", 0, func(o *Options) *string { return &o.Suffix }),
	boolOpt("list-only", 0, func(o *Options) *bool { return &o.ListOnly }),
	unsupported("read-batch", 0, argString),
	unsupported("write-batch", 0, argString),
	unsupported("only-write-batch", 0, argString),
	strOpt("files-from", 0, func(o *Options) *string { return &o.FilesFrom }),
	boolOpt("from0", '0', func(o *Options) *bool { return &o.From0 }),
	unsupported("old-args", 0, argNone),
	{long: []string{"secluded-args", "protect-args"}, short: 's',
		set:   func(o *Options, _ string) error { o.ProtectArgs = true; return nil },
		unset: func(o *Options) { o.ProtectArgs = false }},
	ignored("trust-sender", 0, argNone),
	boolOpt("numeric-ids", 0, func(o *Options) *bool { return &o.NumericIDs }),
	unsupported("usermap", 0, argString),
	unsupported("groupmap", 0, argString),
	unsupported("chown", 0, argString),
	{long: []string{"timeout"}, arg: argString, set: func(o *Options, v string) error {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n < 0 {
			return usagef("invalid --timeout value %q", v)
		}
		o.Timeout = time.Duration(n) * time.Second
		return nil
	}, unset: func(o *Options) { o.Timeout = 0 }},
	{long: []string{"contimeout"}, arg: argString, set: func(o *Options, v string) error {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n < 0 {
			return usagef("invalid --contimeout value %q", v)
		}
		o.ConTimeout = time.Duration(n) * time.Second
		return nil
	}, unset: func(o *Options) { o.ConTimeout = 0 }},
	boolOpt("fsync", 0, func(o *Options) *bool { return &o.Fsync }),
	unsupported("stop-after", 0, argString),
	unsupported("stop-at", 0, argString),
	strOpt("rsh", 'e', func(o *Options) *string { return &o.Rsh }),
	strOpt("rsync-path", 0, func(o *Options) *string { return &o.RsyncPath }),
	strOpt("temp-dir", 'T', func(o *Options) *string { return &o.TempDir }),
	unsupported("iconv", 0, argString),
	ignored("no-iconv", 0, argNone),
	ignored("ipv4", '4', argNone),
	ignored("ipv6", '6', argNone),
	boolOpt("8-bit-output", '8', func(o *Options) *bool { return &o.EightBitOutput }),
	boolOpt("mkpath", 0, func(o *Options) *bool { return &o.MkPath }),
	ignored("qsort", 0, argNone),
	unsupported("copy-as", 0, argString),
	unsupported("address", 0, argString),
	unsupported("port", 0, argString),
	unsupported("sockopts", 0, argString),
	unsupported("password-file", 0, argString),
	ignored("blocking-io", 0, argNone),
	ignored("outbuf", 0, argString),
	{long: []string{"remote-option"}, short: 'M', arg: argString, noNeg: true, set: func(o *Options, v string) error {
		o.RemoteOptions = append(o.RemoteOptions, v)
		return nil
	}},
	{long: []string{"protocol"}, arg: argString, noNeg: true, set: func(o *Options, v string) error {
		n, err := strconv.Atoi(v)
		if err != nil {
			return usagef("invalid --protocol value %q", v)
		}
		o.Protocol = n
		return nil
	}},
	{long: []string{"checksum-seed"}, arg: argString, noNeg: true, set: func(o *Options, v string) error {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			return usagef("invalid --checksum-seed value %q", v)
		}
		o.ChecksumSeed = int32(n)
		return nil
	}},
	boolOpt("fips", 0, func(o *Options) *bool { return &o.FIPS }),
	{long: []string{"server"}, noNeg: true, set: func(o *Options, _ string) error { o.Server = true; return nil }},
	{long: []string{"sender"}, noNeg: true, set: func(o *Options, _ string) error { o.Sender = true; return nil }},
	unsupported("daemon", 0, argNone),
	unsupported("config", 0, argString),
}

var (
	longIndex  = map[string]*option{}
	shortIndex = map[rune]*option{}
	negIndex   = map[string]*option{}
)

func init() {
	for i := range optionTable {
		opt := &optionTable[i]
		for _, l := range opt.long {
			longIndex[l] = opt
			if !opt.noNeg && opt.unset != nil {
				negIndex["no-"+l] = opt
			}
		}
		if opt.short != 0 {
			shortIndex[opt.short] = opt
			if !opt.noNeg && opt.unset != nil {
				negIndex["no-"+string(opt.short)] = opt
			}
		}
	}
	// rsync spells a few negations specially.
	negIndex["no-D"] = longIndex["D"]
	negIndex["no-z"] = longIndex["compress"]
	negIndex["no-compress"] = longIndex["compress"]
	negIndex["no-W"] = longIndex["whole-file"]
	negIndex["no-v"] = longIndex["verbose"]
	negIndex["no-h"] = longIndex["human-readable"]
	negIndex["no-i"] = longIndex["itemize-changes"]
}

// ParseArgs parses an rsync command line, without the program name, and
// returns the options and the remaining SRC... DEST operands.
//
// It accepts the same syntax as rsync: bundled short options (-avz),
// "--name=value" and "--name value", "-Bvalue" and "-B value", "--no-NAME"
// negations, and "--" to end options. A "-" file name for --exclude-from
// and the like reads standard input.
func ParseArgs(args []string) (*Options, []string, error) {
	return parseArgs(args, os.Stdin)
}

// parseArgs is ParseArgs reading "-" files from stdin.
func parseArgs(args []string, stdin io.Reader) (*Options, []string, error) {
	o := &Options{ImpliedDirs: true, set: map[string]bool{}, stdin: stdin}
	var operands []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			operands = append(operands, args[i+1:]...)
			return o, operands, o.finish()
		case strings.HasPrefix(a, "--"):
			name, val, hasVal := strings.Cut(a[2:], "=")
			if opt, ok := longIndex[name]; ok {
				if opt.arg == argNone {
					if hasVal {
						return nil, nil, usagef("option --%s does not take an argument", name)
					}
				} else if !hasVal {
					if i+1 >= len(args) {
						return nil, nil, usagef("option --%s requires an argument", name)
					}
					i++
					val = args[i]
				}
				if err := o.apply(opt, val); err != nil {
					return nil, nil, err
				}
				continue
			}
			if opt, ok := negIndex[name]; ok && !hasVal {
				opt.unset(o)
				o.set[opt.long[0]] = true
				continue
			}
			return nil, nil, usagef("unknown option --%s", name)
		case len(a) > 1 && a[0] == '-':
			for j, r := range a[1:] {
				opt, ok := shortIndex[r]
				if !ok {
					return nil, nil, usagef("unknown option -%c", r)
				}
				if opt.arg == argNone {
					if err := o.apply(opt, ""); err != nil {
						return nil, nil, err
					}
					continue
				}
				val := a[1+j+len(string(r)):]
				if val == "" {
					if i+1 >= len(args) {
						return nil, nil, usagef("option -%c requires an argument", r)
					}
					i++
					val = args[i]
				}
				if err := o.apply(opt, val); err != nil {
					return nil, nil, err
				}
				break
			}
		case o.Server:
			// A client puts every option before the "." that starts
			// the server's operands; what follows are paths, even
			// ones that begin with '-'.
			operands = append(operands, args[i:]...)
			return o, operands, o.finish()
		default:
			operands = append(operands, a)
		}
	}
	return o, operands, o.finish()
}

func (o *Options) apply(opt *option, val string) error {
	if err := opt.set(o, val); err != nil {
		return err
	}
	o.set[opt.long[0]] = true
	return nil
}

// finish applies rsync's option implications once everything is parsed.
func (o *Options) finish() error {
	if o.FilesFrom != "" {
		// The list names what to send: paths are relative, named
		// directories are sent, and -a no longer means -r.
		if !o.set["relative"] {
			o.Relative = true
		}
		if !o.set["dirs"] {
			o.Dirs = true
		}
		if o.Recursive && !o.set["recursive"] {
			o.Recursive = false
		}
	}
	if o.Relative && !o.set["implied-dirs"] {
		o.ImpliedDirs = true
	}
	if o.ListOnly && !o.Recursive && !o.set["dirs"] {
		o.Dirs = true
	}
	if o.Inplace && o.PartialDir != "" {
		return usagef("--inplace cannot be used with --partial-dir")
	}
	if o.Inplace && o.DelayUpdates {
		return usagef("--inplace cannot be used with --delay-updates")
	}
	if o.backups() && o.BackupDir == "" && o.BackupSuffix() == "" {
		return usagef("--suffix cannot be empty without --backup-dir")
	}
	if strings.Contains(o.Suffix, "/") {
		return usagef("--suffix cannot contain slashes: %s", o.Suffix)
	}
	if o.DelayUpdates {
		// Updates wait in the partial directory until the end.
		if o.PartialDir == "" {
			o.PartialDir = ".~tmp~"
		} else if strings.HasPrefix(o.PartialDir, "/") {
			return usagef("--delay-updates requires a relative --partial-dir")
		}
	}
	if o.CopyLinks {
		o.Links = false
	}
	return nil
}

func (o *Options) addRulesFrom(path, prefix string) error {
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(o.stdin)
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return fmt.Errorf("failed to open %s file %s: %w", strings.TrimSpace(prefix), path, err)
	}
	for line := range strings.Lines(string(data)) {
		line = strings.TrimRight(line, "\r\n")
		if line == "" || line[0] == ';' || line[0] == '#' {
			continue
		}
		o.Filters = append(o.Filters, prefix+line)
	}
	return nil
}

// ParseSize parses a size such as "100", "1.5K", "10MB", "2GiB" or "1G-1"
// the way rsync does: K/M/G/T/P suffixes are powers of 1024, unless followed
// by "B" alone (powers of 1000); "iB" is explicitly 1024. unit is the
// multiplier for a bare number (1024 for --bwlimit, 1 otherwise).
func ParseSize(s string, unit int64) (int64, error) {
	orig := s
	adjust := int64(0)
	if i := strings.LastIndexAny(s, "+-"); i > 0 {
		n, err := strconv.ParseInt(s[i:], 10, 64)
		if err != nil || (n != 1 && n != -1) {
			return 0, usagef("invalid size %q", orig)
		}
		adjust, s = n, s[:i]
	}
	end := len(s)
	for end > 0 && !unicode.IsDigit(rune(s[end-1])) && s[end-1] != '.' {
		end--
	}
	num, suffix := s[:end], strings.ToLower(s[end:])
	f, err := strconv.ParseFloat(num, 64)
	if err != nil || f < 0 {
		return 0, usagef("invalid size %q", orig)
	}
	mult := float64(unit)
	if suffix != "" {
		base := 1024.0
		switch {
		case len(suffix) == 1:
		case suffix[1:] == "b":
			base = 1000
		case suffix[1:] == "ib":
		default:
			return 0, usagef("invalid size %q", orig)
		}
		exp := strings.IndexByte("bkmgtp", suffix[0])
		if exp < 0 {
			return 0, usagef("invalid size %q", orig)
		}
		mult = 1
		for range exp {
			mult *= base
		}
	}
	return int64(f*mult) + adjust, nil
}
