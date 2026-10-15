// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package rsync

import (
	"fmt"
	"strconv"
	"strings"
)

// rsync sorts its output into categories, each shown at a level: -v raises
// several at once and --info / --debug set them one by one.

type infoFlag int

const (
	infoBackup infoFlag = iota
	infoCopy
	infoDel
	infoFlist
	infoMisc
	infoMount
	infoName
	infoNonreg
	infoProgress
	infoRemove
	infoSkip
	infoStats
	infoSymsafe
	numInfo
)

var infoWords = [numInfo]struct {
	name, help string
}{
	{"BACKUP", "Mention files backed up"},
	{"COPY", "Mention files copied locally on the receiving side"},
	{"DEL", "Mention deletions on the receiving side"},
	{"FLIST", "Mention file-list receiving/sending (levels 1-2)"},
	{"MISC", "Mention miscellaneous information (levels 1-2)"},
	{"MOUNT", "Mention mounts that were found or skipped"},
	{"NAME", "Mention 1) updated file/dir names, 2) unchanged names"},
	{"NONREG", "Mention skipped non-regular files (default 1, 0 disables)"},
	{"PROGRESS", "Mention 1) per-file progress or 2) total transfer progress"},
	{"REMOVE", "Mention files removed on the sending side"},
	{"SKIP", "Mention files skipped due to transfer overrides (levels 1-2)"},
	{"STATS", "Mention statistics at end of run (levels 1-3)"},
	{"SYMSAFE", "Mention symlinks that are unsafe"},
}

// infoByVerbosity lists what each -v adds.
var infoByVerbosity = []string{
	"NONREG",
	"COPY,DEL,FLIST,MISC,NAME,STATS,SYMSAFE",
	"BACKUP,MISC2,MOUNT,NAME2,REMOVE,SKIP",
}

type debugFlag int

const (
	debugACL debugFlag = iota
	debugBackup
	debugBind
	debugChdir
	debugConnect
	debugCmd
	debugDel
	debugDeltasum
	debugDup
	debugExit
	debugFilter
	debugFlist
	debugFuzzy
	debugGenr
	debugHash
	debugHlink
	debugIconv
	debugIO
	debugNstr
	debugOwn
	debugProto
	debugRecv
	debugSend
	debugTime
	numDebug
)

var debugWords = [numDebug]struct {
	name, help string
}{
	{"ACL", "Debug extra ACL info"},
	{"BACKUP", "Debug backup actions (levels 1-2)"},
	{"BIND", "Debug socket bind actions"},
	{"CHDIR", "Debug when the current directory changes"},
	{"CONNECT", "Debug connection events (levels 1-2)"},
	{"CMD", "Debug commands+options that are issued (levels 1-2)"},
	{"DEL", "Debug delete actions (levels 1-3)"},
	{"DELTASUM", "Debug delta-transfer checksumming (levels 1-4)"},
	{"DUP", "Debug weeding of duplicate names"},
	{"EXIT", "Debug exit events (levels 1-3)"},
	{"FILTER", "Debug filter actions (levels 1-3)"},
	{"FLIST", "Debug file-list operations (levels 1-4)"},
	{"FUZZY", "Debug fuzzy scoring (levels 1-2)"},
	{"GENR", "Debug generator functions"},
	{"HASH", "Debug hashtable code"},
	{"HLINK", "Debug hard-link actions (levels 1-3)"},
	{"ICONV", "Debug iconv character conversions (levels 1-2)"},
	{"IO", "Debug I/O routines (levels 1-4)"},
	{"NSTR", "Debug negotiation strings"},
	{"OWN", "Debug ownership changes in users & groups (levels 1-2)"},
	{"PROTO", "Debug protocol information"},
	{"RECV", "Debug receiver functions"},
	{"SEND", "Debug sender functions"},
	{"TIME", "Debug setting of modified times (levels 1-2)"},
}

var debugByVerbosity = []string{
	"",
	"",
	"BIND,CMD,CONNECT,DEL,DELTASUM,DUP,FILTER,FLIST,ICONV",
	"ACL,BACKUP,CONNECT2,DELTASUM2,DEL2,EXIT,FILTER2,FLIST2,FUZZY,GENR,OWN,RECV,SEND,TIME",
	"CMD2,DELTASUM3,DEL3,EXIT2,FLIST3,ICONV2,OWN2,PROTO,TIME2",
	"CHDIR,DELTASUM4,FLIST4,FUZZY2,HASH,HLINK",
}

// levels holds the level of every --info and --debug category.
type levels struct {
	info  [numInfo]int
	debug [numDebug]int
}

// applyWords sets categories from a --info or --debug argument: a
// comma-separated list of names with optional levels, "ALL"/"NONE", and
// "HELP". It returns errInfoHelp for HELP.
func applyWords(arg string, names []string, set []int, option string) error {
	for w := range strings.SplitSeq(arg, ",") {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		up := strings.ToUpper(w)
		i := len(up)
		for i > 0 && up[i-1] >= '0' && up[i-1] <= '9' {
			i--
		}
		word, lvl := up[:i], 1
		if i < len(up) {
			lvl, _ = strconv.Atoi(up[i:])
		}
		switch word {
		case "HELP":
			return &helpError{option}
		case "ALL":
			if i == len(up) {
				lvl = 4
			}
			for j := range set {
				set[j] = lvl
			}
			continue
		case "NONE":
			clear(set)
			continue
		}
		j := indexOf(names, word)
		if j < 0 {
			return usagef("Unknown --%s item: \"%s\"", option, w)
		}
		set[j] = lvl
	}
	return nil
}

func indexOf(names []string, w string) int {
	for i, n := range names {
		if n == w {
			return i
		}
	}
	return -1
}

func infoNames() []string {
	n := make([]string, numInfo)
	for i, w := range infoWords {
		n[i] = w.name
	}
	return n
}

func debugNames() []string {
	n := make([]string, numDebug)
	for i, w := range debugWords {
		n[i] = w.name
	}
	return n
}

// helpError asks for the --info or --debug help.
type helpError struct{ option string }

func (e *helpError) Error() string { return "--" + e.option + "=help" }

func (e *helpError) text() string {
	var b strings.Builder
	b.WriteString("Use OPT or OPT1 for level 1 output, OPT2 for level 2, etc.; OPT0 silences.\n\n")
	if e.option == "info" {
		for _, w := range infoWords {
			fmt.Fprintf(&b, "%-10s %s\n", w.name, w.help)
		}
	} else {
		for _, w := range debugWords {
			fmt.Fprintf(&b, "%-10s %s\n", w.name, w.help)
		}
	}
	fmt.Fprintf(&b, "\n%-10s Set all --%s options (e.g. all4)\n", "ALL", e.option)
	fmt.Fprintf(&b, "%-10s Silence all --%s options (same as all0)\n", "NONE", e.option)
	fmt.Fprintf(&b, "%-10s Output this help message\n", "HELP")
	b.WriteString("\nOptions added at each level of verbosity:\n")
	table := infoByVerbosity
	if e.option == "debug" {
		table = debugByVerbosity
	}
	for i, t := range table {
		if t != "" {
			fmt.Fprintf(&b, "%d) %s\n", i, t)
		}
	}
	return b.String()
}

// outputLevels works out every category's level from the options, as
// rsync does: -v first, then the options that imply categories, then
// explicit --info and --debug words in order.
func outputLevels(o *Options) levels {
	var l levels
	in, db := l.info[:], l.debug[:]
	raise := func(table []string, names []string, set []int, upTo int) {
		for v := 0; v <= upTo && v < len(table); v++ {
			for w := range strings.SplitSeq(table[v], ",") {
				if w == "" {
					continue
				}
				word, lvl := w, 1
				if c := w[len(w)-1]; c >= '0' && c <= '9' {
					word, lvl = w[:len(w)-1], int(c-'0')
				}
				if j := indexOf(names, word); j >= 0 && set[j] < lvl {
					set[j] = lvl
				}
			}
		}
	}
	if !o.Quiet {
		raise(infoByVerbosity, infoNames(), in, o.Verbose)
		raise(debugByVerbosity, debugNames(), db, o.Verbose)
	}
	if o.Progress {
		l.info[infoFlist] = max(l.info[infoFlist], 2)
		l.info[infoName] = max(l.info[infoName], 1)
		l.info[infoProgress] = max(l.info[infoProgress], 1)
	}
	if o.ProgressTotal {
		l.info[infoProgress] = 2
	}
	if o.Stats {
		l.info[infoStats] = max(l.info[infoStats], 2)
	}
	for _, a := range o.Info {
		applyWords(a, infoNames(), in, "info")
	}
	for _, a := range o.Debug {
		applyWords(a, debugNames(), db, "debug")
	}
	return l
}
