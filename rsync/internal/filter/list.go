// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package filter

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"strings"
)

// Options configures a List.
type Options struct {
	// AmSender is this process's role. Rules read from per-directory merge
	// files that apply only to the other side are dropped.
	AmSender bool
	// DeleteExcluded applies --delete-excluded: rules without an explicit
	// side (and no-prefix per-dir merge rules) become sender-only. Clients
	// and local transfers set it; a remote server leaves it false, because
	// the client already sent sided rules (parse received rules with
	// Parser.DeleteExcluded instead).
	DeleteExcluded bool
	// Cwd is the absolute directory that matcher names are relative to (the
	// directory the sender works from, or the destination root for the
	// receiver's delete scan). For a daemon module it is relative to the
	// module root but still starts with "/". Empty means "/". It is used by
	// absolute-path rules ("/" modifier), by anchored rules in per-dir
	// merge files and by the parent-directory scan of dir-merge names that
	// contain a slash.
	Cwd string
	// ReadFile reads a file by absolute path. It is used for the
	// parent-directory pre-scan of dir-merge rules such as
	// ": /.rsync-filter" (-F) and for "." merges named inside per-dir merge
	// files. Nil means os.ReadFile.
	ReadFile func(path string) ([]byte, error)
	// From0 makes records in per-dir merge files NUL-terminated.
	From0 bool
	// CVS configures the list inserted by "-C" rules. Nil means only the
	// built-in patterns (perishable, as for protocol >= 30).
	CVS *CVSConfig
}

// List is an ordered rule list together with the state of its per-directory
// merge files.
type List struct {
	opts     Options
	cwd      string // cleaned absolute Cwd
	cwdRel   string // cwd without the leading '/'
	rules    []Rule
	merges   []*mergeList
	cvs      []Rule
	cvsBuilt bool
	dirMerge bool
}

// mergeList is the state of one per-directory merge rule: its current rules
// are the rules of the innermost directory's file followed by the inherited
// rules of the enclosing directories.
type mergeList struct {
	flags     Flags
	name      string // file name; reduced to its base name by setup
	needSetup bool
	rules     []Rule
}

const sidesAll = 1<<SenderSide | 1<<ReceiverSide

// NewList builds a list from rules (in evaluation order) with default
// Options. A rule carrying FlagClear discards all rules before it. If the
// list contains "-C" rules, the CVS list is built from the built-in
// patterns only; use NewListWithOptions to configure it.
func NewList(rules []Rule) *List {
	l, err := NewListWithOptions(rules, Options{})
	if err != nil {
		// Only CVS list construction can fail, and the default list
		// always parses.
		panic(err)
	}
	return l
}

// NewListWithOptions builds a list from rules with the given options.
func NewListWithOptions(rules []Rule, opts Options) (*List, error) {
	l := &List{opts: opts}
	if l.opts.ReadFile == nil {
		l.opts.ReadFile = os.ReadFile
	}
	l.cwd = "/"
	if opts.Cwd != "" {
		l.cwd = path.Clean("/" + opts.Cwd)
	}
	l.cwdRel = strings.TrimPrefix(l.cwd, "/")
	if err := l.Append(rules...); err != nil {
		return nil, err
	}
	return l, nil
}

// Append adds rules to the end of the global list (for example the ":C" and
// "-C" rules implied by --cvs-exclude, which rsync appends after all user
// rules). It must not be called after EnterDir.
func (l *List) Append(rules ...Rule) error {
	for _, r := range rules {
		if r.Flags&FlagClear != 0 {
			l.rules = nil
			l.merges = nil
			continue
		}
		r.merge = nil
		if r.Flags&FlagPerDirMerge != 0 {
			l.dirMerge = true
			if !l.register(&r) {
				continue
			}
		}
		if err := l.prepare(&r); err != nil {
			return err
		}
		r.sides = sideMask(r.Flags, l.opts.DeleteExcluded)
		l.rules = append(l.rules, r)
	}
	return nil
}

// prepare builds the CVS list on the first "-C" rule.
func (l *List) prepare(r *Rule) error {
	if !r.isCVSPlaceholder() || l.cvsBuilt {
		return nil
	}
	cfg := CVSConfig{}
	if l.opts.CVS != nil {
		cfg = *l.opts.CVS
	}
	cvs, err := buildCVS(cfg, r.Flags&^FlagCVSIgnore, l.opts.DeleteExcluded)
	if err != nil {
		return err
	}
	for i := range cvs {
		cvs[i].sides = sideMask(cvs[i].Flags, l.opts.DeleteExcluded)
	}
	l.cvs, l.cvsBuilt = cvs, true
	return nil
}

// sideMask returns which sides a rule of the global list applies to. It is
// the local counterpart of the "elide" computation done when the list is
// sent.
func sideMask(f Flags, deleteExcluded bool) uint8 {
	switch f & sideFlags {
	case FlagSenderSide:
		return 1 << SenderSide
	case FlagReceiverSide:
		return 1 << ReceiverSide
	case sideFlags:
		return sidesAll
	}
	if deleteExcluded && (f&FlagPerDirMerge == 0 || f&FlagNoPrefixes != 0) {
		return 1 << SenderSide
	}
	return sidesAll
}

// register activates a per-dir merge rule. It returns false (and the rule
// is dropped) if a merge file with the same base name is already active.
func (l *List) register(r *Rule) bool {
	base := baseName(r.Pattern)
	for _, m := range l.merges {
		if baseName(m.name) == base {
			return false
		}
	}
	m := &mergeList{flags: r.Flags, name: r.Pattern, needSetup: true}
	l.merges = append(l.merges, m)
	r.merge = m
	return true
}

func baseName(s string) string {
	return s[strings.LastIndexByte(s, '/')+1:]
}

// HasDirMerge reports whether any dir-merge rule was seen. A client
// receiver must then trust the sender's filtering instead of rejecting
// excluded names in the received file list.
func (l *List) HasDirMerge() bool { return l.dirMerge }

// Rules returns the global rules (per-dir merge rules appear as
// placeholders). The slice must not be modified.
func (l *List) Rules() []Rule { return l.rules }

// Excluded reports whether name is excluded for side. name is the path
// relative to the transfer root as rsync's matcher sees it ("dir/file", no
// "./" prefix and no trailing slash); isDir tells whether the item is a
// directory (symlinks are not directories unless they are followed).
func (l *List) Excluded(name string, isDir bool, side Side) bool {
	return l.Match(name, isDir, side, 0) == Excluded
}

// Match evaluates the list for name: the first matching rule decides.
func (l *List) Match(name string, isDir bool, side Side, mf MatchFlags) Result {
	n := makeName(name, isDir, l.cwdRel, mf&XattrName != 0)
	return l.check(l.rules, &n, uint8(1)<<side, mf&IgnorePerishable != 0)
}

func (l *List) check(rules []Rule, n *name, side uint8, ignorePerishable bool) Result {
	for i := range rules {
		r := &rules[i]
		if ignorePerishable && r.Flags&FlagPerishable != 0 {
			continue
		}
		if r.merge != nil {
			if res := l.check(r.merge.rules, n, side, ignorePerishable); res != NoMatch {
				return res
			}
			continue
		}
		if r.Flags&FlagCVSIgnore != 0 {
			if res := l.check(l.cvs, n, side, ignorePerishable); res != NoMatch {
				return res
			}
			continue
		}
		if r.sides&side == 0 {
			continue
		}
		if r.matches(n) {
			if r.Flags&FlagInclude != 0 {
				return Included
			}
			return Excluded
		}
	}
	return NoMatch
}

// EnterDir must be called when the scanner descends into directory dir
// (relative to Options.Cwd; "" or "." is Cwd itself), before matching the
// directory's entries. readFile reads a file in that directory by base name
// and returns an error satisfying errors.Is(err, fs.ErrNotExist) if it is
// absent. Each active dir-merge rule's file is read: its rules take effect
// for dir and (unless the rule has the "n" modifier) its descendants, ahead
// of the rules inherited from parent directories.
//
// The returned leave function restores the previous state and must be
// called, in LIFO order, when the scanner leaves dir. It is never nil, even
// when err is not: a syntax error in a merge file is returned (rsync treats
// it as fatal), as is a read error other than fs.ErrNotExist (the file is
// then skipped).
func (l *List) EnterDir(dir string, readFile func(base string) ([]byte, error)) (leave func(), err error) {
	saved := make([][]Rule, len(l.merges))
	for i, m := range l.merges {
		saved[i] = m.rules
	}
	nMerges := len(l.merges)
	leave = func() {
		for i := 0; i < nMerges && i < len(l.merges); i++ {
			l.merges[i].rules = saved[i]
		}
		for i := nMerges; i < len(l.merges); i++ {
			l.merges[i] = nil
		}
		if len(l.merges) > nMerges {
			l.merges = l.merges[:nMerges]
		}
	}
	if len(l.merges) == 0 {
		return leave, nil
	}

	absDir := l.cwd
	if dir != "" && dir != "." {
		absDir = path.Join(l.cwd, dir)
	}
	// The loop bound is re-read on purpose: dir-merge rules found while
	// reading files in this directory are processed here too.
	for i := 0; i < len(l.merges); i++ {
		m := l.merges[i]
		if m.flags&FlagNoInherit != 0 {
			m.rules = nil
		}
		if m.needSetup {
			m.needSetup = false
			if e := l.setup(m, absDir); e != nil && err == nil {
				err = e
			}
		}
		data, rerr := readFile(m.name)
		if rerr != nil {
			if !errors.Is(rerr, fs.ErrNotExist) && err == nil {
				err = rerr
			}
			continue
		}
		if e := l.readPerDir(m, data, absDir); e != nil && err == nil {
			err = e
		}
	}
	return leave, err
}

// readPerDir parses the contents of m's file in directory absDir and puts
// its rules in front of m's inherited rules.
func (l *List) readPerDir(m *mergeList, data []byte, absDir string) error {
	c := &parseCtx{
		readFile:       l.opts.ReadFile,
		deleteExcluded: l.opts.DeleteExcluded,
		from0:          l.opts.From0,
		perDir:         true,
		amSender:       l.opts.AmSender,
		mergeDir:       absDir,
		cwd:            l.cwd,
	}
	if absDir != "/" {
		c.dirPrefix = absDir
	}
	local, err := c.parseData(nil, data, m.flags, false)
	inherited := m.rules
	if len(local) > 0 && local[0].Flags&FlagClear != 0 {
		local = local[1:]
		inherited = nil
	}
	out := make([]Rule, 0, len(local)+len(inherited))
	for _, r := range local {
		r.sides = sidesAll
		if r.Flags&FlagPerDirMerge != 0 {
			l.dirMerge = true
			if !l.register(&r) {
				continue
			}
		}
		if perr := l.prepare(&r); perr != nil && err == nil {
			err = perr
		}
		out = append(out, r)
	}
	m.rules = append(out, inherited...)
	return err
}

// setup performs the parent-directory pre-scan for a dir-merge name that
// contains a slash: the file is read in every directory from the named
// directory down to, but not including, firstDir (the first directory
// entered), and the merge name is reduced to its base name.
func (l *List) setup(m *mergeList, firstDir string) error {
	if !strings.Contains(m.name, "/") {
		return nil
	}
	full := m.name
	if !strings.HasPrefix(full, "/") {
		full = path.Join(firstDir, full)
	}
	full = path.Clean(full)
	start, base := path.Split(full)
	m.name = base
	start = path.Clean(start)

	// start must be an ancestor of (or equal to) firstDir.
	var rel string
	switch {
	case start == firstDir:
		return nil
	case start == "/":
		rel = strings.TrimPrefix(firstDir, "/")
	case strings.HasPrefix(firstDir, start+"/"):
		rel = firstDir[len(start)+1:]
	default:
		return nil
	}
	dir := start
	comps := strings.Split(rel, "/")
	var err error
	for i := 0; ; i++ {
		data, rerr := l.opts.ReadFile(path.Join(dir, base))
		if rerr == nil {
			if e := l.readPerDir(m, data, dir); e != nil && err == nil {
				err = e
			}
			if m.flags&FlagNoInherit != 0 {
				m.rules = nil
			}
		} else if !errors.Is(rerr, fs.ErrNotExist) && err == nil {
			err = rerr
		}
		if i == len(comps)-1 {
			break
		}
		dir = path.Join(dir, comps[i])
	}
	return err
}
