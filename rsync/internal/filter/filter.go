// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

// Package filter implements rsync-compatible filter rules: parsing of the
// --filter / --include / --exclude rule syntax, wildcard matching with
// rsync's semantics, per-directory merge files (dir-merge, -F, -C), and the
// textual encoding used when the filter list is exchanged on the wire.
//
// The decisions made by this package are intended to be identical to GNU
// rsync's for the same rules and names. Matching works on raw bytes; there is
// no case folding and no Unicode normalisation.
//
// Typical use:
//
//	var rules []filter.Rule
//	for _, arg := range filterArgs {
//		r, err := filter.ParseRule(arg)
//		...
//		rules = append(rules, r...)
//	}
//	l, err := filter.NewListWithOptions(rules, filter.Options{AmSender: true, Cwd: cwd})
//	if l.Excluded("dir/file.o", false, filter.SenderSide) { ... }
//
// A List is not safe for concurrent use while EnterDir/leave are being called;
// concurrent calls to Excluded/Match on a List whose per-directory state is not
// changing are safe.
package filter

import (
	"errors"
	"strings"
)

// Flags is the set of attributes of a rule. The names follow the rule
// modifiers documented in rsync(1).
type Flags uint32

const (
	// FlagInclude marks an include rule (+, show, risk); otherwise the rule
	// excludes (-, hide, protect).
	FlagInclude Flags = 1 << iota
	// FlagDirectory: the pattern had a trailing slash and only matches
	// directories.
	FlagDirectory
	// FlagAbsPath: the pattern is matched against the absolute path of the
	// item (modifier "/"), or it is an anchored rule from a per-directory
	// merge file.
	FlagAbsPath
	// FlagNegate: the rule fires when the pattern does not match (modifier "!").
	FlagNegate
	// FlagSenderSide restricts the rule to the sending side (modifier "s",
	// hide, show).
	FlagSenderSide
	// FlagReceiverSide restricts the rule to the receiving side (modifier
	// "r", protect, risk).
	FlagReceiverSide
	// FlagPerishable (modifier "p") is ignored while scanning a directory
	// that is itself being deleted.
	FlagPerishable
	// FlagXattr (modifier "x") makes the rule apply to xattr names only.
	FlagXattr
	// FlagMerge marks a merge-file rule ("." or ":").
	FlagMerge
	// FlagPerDirMerge marks a per-directory merge rule (":", dir-merge).
	FlagPerDirMerge
	// FlagNoInherit (modifier "n"): per-dir rules are not inherited by
	// subdirectories.
	FlagNoInherit
	// FlagWordSplit (modifier "w"): the merge file is split on whitespace.
	FlagWordSplit
	// FlagNoPrefixes (modifiers "-"/"+" on a merge rule): the merge file
	// holds bare patterns.
	FlagNoPrefixes
	// FlagExcludeSelf (modifier "e"): the merge file itself is excluded.
	FlagExcludeSelf
	// FlagCVSIgnore (modifier "C"): on a merge rule, parse the file like a
	// .cvsignore; on an include/exclude rule, insert the CVS excludes here.
	FlagCVSIgnore
	// FlagClear marks the "!" (clear) pseudo-rule. It is never stored in a
	// List; when NewList encounters it all preceding rules are dropped.
	FlagClear

	// Derived from the final pattern (not settable by callers).
	flagWild        // contains any of * ? [
	flagWild2       // contains **
	flagWild2Prefix // starts with **
	flagWild3Suffix // contains ** and ends with ***
)

// Flags inherited by rules read from a merge file from the merge rule.
const inheritedFlags = FlagAbsPath | FlagInclude | FlagDirectory | FlagNegate | FlagPerishable

const sideFlags = FlagSenderSide | FlagReceiverSide

// Side selects which rules apply when matching.
type Side int

const (
	// SenderSide applies rules without a side and sender-side rules
	// (exclude/include/hide/show). The sender uses it when building the
	// file list; a client receiver uses it to validate the sender's list.
	SenderSide Side = iota
	// ReceiverSide applies rules without a side and receiver-side rules
	// (exclude/include/protect/risk). The receiver uses it for --delete.
	ReceiverSide
)

// Result is the outcome of evaluating a rule list.
type Result int

const (
	// NoMatch means no rule matched; the item is included by default.
	NoMatch Result = iota
	// Included means an include rule matched first.
	Included
	// Excluded means an exclude rule matched first.
	Excluded
)

// MatchFlags modify a Match call.
type MatchFlags uint8

const (
	// IgnorePerishable skips perishable rules; used while scanning the
	// contents of a directory that is being deleted.
	IgnorePerishable MatchFlags = 1 << iota
	// XattrName matches an extended attribute name instead of a file name:
	// only rules with the "x" modifier apply.
	XattrName
)

// Errors returned by the parser and wire encoder.
var (
	ErrSyntax    = errors.New("filter: syntax error")
	ErrTooModern = errors.New("filter rules are too modern for remote rsync")
	ErrMergeFile = errors.New("filter: cannot read merge file")
)

// SyntaxError describes an invalid filter rule. It wraps ErrSyntax.
type SyntaxError struct {
	Msg  string // e.g. "Unknown filter rule"
	Rule string // the offending rule text
}

// Error returns the message followed by the offending rule.
func (e *SyntaxError) Error() string {
	return "filter: " + e.Msg + ": " + e.Rule
}

// Unwrap returns ErrSyntax.
func (e *SyntaxError) Unwrap() error { return ErrSyntax }

// matchMode is how the subject is compared with the pattern.
type matchMode uint8

const (
	modeBase     matchMode = iota // last path component only
	modeTail                      // last tailN components
	modeAnywhere                  // at the start or after any '/'
	modeWhole                     // the entire subject
)

// fastKind selects a precompiled fast path.
type fastKind uint8

const (
	fastWild     fastKind = iota // general wildcard matcher
	fastLiteral                  // byte equality (per mode)
	fastSuffix                   // "*lit"  in base mode
	fastPrefix                   // "lit*"  in base mode
	fastContains                 // "*lit*" in base mode
	fastAny                      // "*"     in base mode
)

// Rule is one parsed filter rule.
type Rule struct {
	// Pattern is the normalised pattern: without the trailing slash of a
	// directory rule, and for anchored per-directory rules prefixed with the
	// directory holding the merge file. For merge rules it is the file name.
	Pattern string
	// Flags holds the rule type and modifiers.
	Flags Flags

	slashCnt int

	// compiled matcher
	mode  matchMode
	kind  fastKind
	pat   string // pattern with the anchoring '/' removed
	lit   string // literal part for fast paths
	tailN int    // for modeTail: number of trailing components

	merge *mergeList // per-dir merge state (only inside a List)
	sides uint8      // applicability mask computed by the List (bit 1<<Side)
}

// IsInclude reports whether r is an include rule.
func (r *Rule) IsInclude() bool { return r.Flags&FlagInclude != 0 }

// isCVSPlaceholder reports whether r is a "-C" rule (insert the CVS list).
func (r *Rule) isCVSPlaceholder() bool {
	return r.Flags&(FlagCVSIgnore|FlagMerge) == FlagCVSIgnore
}

// String returns the rule in canonical --filter syntax, e.g. "-p *.o" or
// "+ dir/".
func (r Rule) String() string {
	if r.Flags&FlagClear != 0 {
		return "!"
	}
	p := r.prefix()
	var b strings.Builder
	b.WriteString(p)
	if r.Pattern != "" {
		b.WriteByte(' ')
		b.WriteString(r.Pattern)
	}
	if r.Flags&FlagDirectory != 0 {
		b.WriteByte('/')
	}
	return b.String()
}

// prefix returns the full (non-wire) rule prefix without the trailing space.
func (r *Rule) prefix() string {
	s, _ := buildPrefix(r, false, 0, false, false)
	return strings.TrimSuffix(s, " ")
}

// compile derives the wildcard flags, slash count and fast matcher from
// r.Pattern. It must be called whenever Pattern changes.
func (r *Rule) compile() {
	pat := r.Pattern
	r.Flags &^= flagWild | flagWild2 | flagWild2Prefix | flagWild3Suffix
	r.slashCnt = strings.Count(pat, "/")
	if strings.ContainsAny(pat, "*?[") {
		r.Flags |= flagWild
		if i := strings.Index(pat, "**"); i >= 0 {
			r.Flags |= flagWild2
			if i == 0 {
				r.Flags |= flagWild2Prefix
			}
			if strings.HasSuffix(pat, "***") {
				r.Flags |= flagWild3Suffix
			}
		}
	}
	anchored := strings.HasPrefix(pat, "/")
	if anchored {
		pat = pat[1:]
	}
	r.pat = pat
	switch {
	case r.slashCnt == 0 && r.Flags&flagWild2 == 0:
		r.mode = modeBase
	case !anchored && r.slashCnt > 0 && r.Flags&flagWild2 == 0:
		r.mode = modeTail
		r.tailN = r.slashCnt + 1
	case !anchored && r.Flags&flagWild2 != 0 && r.Flags&flagWild2Prefix == 0:
		r.mode = modeAnywhere
	default:
		r.mode = modeWhole
	}
	r.kind = fastWild
	r.lit = ""
	if r.Flags&flagWild == 0 {
		r.kind = fastLiteral
		r.lit = pat
		return
	}
	if r.mode != modeBase {
		return
	}
	// Base-name fast paths: patterns made only of plain bytes and single
	// '*'s at either end. The base name never contains '/', so a single '*'
	// matches any run of its bytes.
	core := pat
	lead := strings.HasPrefix(core, "*")
	if lead {
		core = core[1:]
	}
	trail := strings.HasSuffix(core, "*")
	if trail {
		core = core[:len(core)-1]
	}
	if strings.ContainsAny(core, "*?[\\") {
		return
	}
	switch {
	case lead && trail && core == "":
		// "**" cannot occur here (it would be WILD2), so this is "*".
		r.kind = fastAny
	case lead && trail:
		r.kind, r.lit = fastContains, core
	case lead && core != "":
		r.kind, r.lit = fastSuffix, core
	case lead:
		r.kind = fastAny
	case trail:
		r.kind, r.lit = fastPrefix, core
	}
}
