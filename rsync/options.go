// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package rsync

import "time"

// Options configures a transfer. The zero value copies nothing but the
// named files; [ParseArgs] fills Options from an rsync command line.
//
// Field names follow rsync's long option names where one exists.
type Options struct {
	// Output.
	Verbose        int    // -v, repeatable
	Quiet          bool   // -q
	Stats          bool   // --stats
	HumanReadable  int    // -h, repeatable
	Progress       bool   // --progress: per-file progress
	ProgressTotal  bool   // --info=progress2: whole-transfer progress
	ItemizeChanges int    // -i, repeatable
	OutFormat      string // --out-format
	ListOnly       bool   // --list-only
	EightBitOutput bool   // -8

	// What to transfer.
	Recursive         bool // -r
	Dirs              bool // -d
	Relative          bool // -R
	ImpliedDirs       bool // --no-implied-dirs clears it; defaults to true
	OneFileSystem     int  // -x, repeatable
	Existing          bool // --existing
	IgnoreExisting    bool // --ignore-existing
	Update            bool // -u
	MaxSize           int64
	MinSize           int64
	PruneEmptyDirs    bool // -m
	NoIncRecursive    bool // --no-inc-recursive: send the whole file list up front
	MkPath            bool // --mkpath
	RemoveSourceFiles bool

	// Comparing.
	Checksum       bool          // -c
	IgnoreTimes    bool          // -I
	SizeOnly       bool          // --size-only
	ModifyWindow   time.Duration // --modify-window
	ChecksumChoice string        // --checksum-choice

	// What to preserve.
	Links           bool // -l
	CopyLinks       bool // -L
	CopyUnsafeLinks bool
	SafeLinks       bool
	CopyDirlinks    bool // -k
	KeepDirlinks    bool // -K
	HardLinks       bool // -H
	Perms           bool // -p
	Executability   bool // -E
	Owner           bool // -o
	Group           bool // -g
	Devices         bool // --devices
	Specials        bool // --specials
	Times           bool // -t
	OmitDirTimes    bool // -O
	OmitLinkTimes   bool // -J
	NumericIDs      bool // --numeric-ids
	Super           bool // --super
	Chmod           []string

	// Deleting.
	Delete         DeleteMode
	DeleteExcluded bool
	Force          bool
	IgnoreErrors   bool
	MaxDelete      int // 0 means unlimited; rsync's "--max-delete=0" is ErrMaxDelete on any delete and is stored as -1

	// Filtering, in command-line order.
	Filters    []string // each entry is one filter rule in --filter syntax
	CVSExclude bool     // -C

	// How to write files.
	WholeFile    *bool // -W / --no-W; nil lets the transfer decide
	Inplace      bool
	Partial      bool
	PartialDir   string
	TempDir      string // -T
	Sparse       bool   // -S
	DelayUpdates bool
	Fsync        bool
	BlockSize    int // -B; 0 picks per file

	// Compression.
	Compress       bool   // -z
	CompressChoice string // --compress-choice
	CompressLevel  int    // --compress-level; 0 is the algorithm default
	SkipCompress   string

	// Remote side.
	Rsh           string // -e
	RsyncPath     string // --rsync-path
	ProtectArgs   bool   // -s
	Timeout       time.Duration // --timeout: give up after this long without I/O
	ConTimeout    time.Duration // --contimeout: give up if the remote rsync has not started by then
	BWLimit       int64 // bytes per second; 0 is unlimited
	Protocol      int   // --protocol; 0 is the newest supported
	ChecksumSeed  int32
	RemoteOptions []string // -M

	DryRun bool // -n

	// FIPS restricts the transfer to algorithms acceptable in FIPS 140
	// environments: checksums are negotiated only among the
	// non-cryptographic xxHash family, never MD4, MD5 or SHA-1, and peers
	// that cannot negotiate (rsync before 3.2.0) are refused. It is
	// implied when the program runs in Go's FIPS 140 mode.
	FIPS bool // --fips

	// Server mode (set when invoked as the remote half of a transfer).
	Server bool
	Sender bool

	// set records which options were given explicitly, keyed by long name.
	set map[string]bool
}

// DeleteMode says whether and when the receiver deletes extraneous files.
type DeleteMode int

const (
	DeleteNone   DeleteMode = iota
	DeleteAuto              // --delete: during for protocol 30+, before otherwise
	DeleteBefore            // --delete-before
	DeleteDuring            // --delete-during / --del
	DeleteDelay             // --delete-delay
	DeleteAfter             // --delete-after
)

// IsSet reports whether the option with the given long name appeared on the
// command line.
func (o *Options) IsSet(long string) bool { return o.set[long] }
