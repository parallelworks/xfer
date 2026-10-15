// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package rsync

import (
	"io"
	"path"
	"strings"
)

type filterDir struct {
	dir   string // wire name; "" for the top of the transfer
	leave func()
}

// enterFilterDir makes the per-directory merge files (such as -F's
// .rsync-filter) of the destination directory dir and of its parents the
// ones in effect, so the delete scan of dir honours the rules they hold.
// Directories are scanned roughly in tree order, so the directories already
// entered are kept as a stack and only the difference is left and entered.
func (rv *receiving) enterFilterDir(dir string) {
	if rv.filters == nil || !rv.filters.HasDirMerge() {
		return
	}
	dir = strings.Trim(strings.TrimSuffix(dir, "/."), "/")
	if dir == "." {
		dir = ""
	}
	within := func(parent string) bool {
		return parent == "" || dir == parent || strings.HasPrefix(dir, parent+"/")
	}
	for n := len(rv.filterDirs); n > 0 && !within(rv.filterDirs[n-1].dir); n-- {
		rv.filterDirs[n-1].leave()
		rv.filterDirs = rv.filterDirs[:n-1]
	}
	// The directories still to enter, outermost first.
	var todo []string
	if len(rv.filterDirs) == 0 {
		todo = append(todo, "")
	}
	if dir != "" {
		have := ""
		if n := len(rv.filterDirs); n > 0 {
			have = rv.filterDirs[n-1].dir
		}
		rest := dir
		if have != "" {
			rest = strings.TrimPrefix(dir, have+"/")
			if dir == have {
				rest = ""
			}
		}
		cur := have
		for part := range strings.SplitSeq(rest, "/") {
			if part == "" {
				continue
			}
			cur = path.Join(cur, part)
			todo = append(todo, cur)
		}
	}
	for _, d := range todo {
		local := d
		if local == "" {
			local = "."
		}
		leave, err := rv.filters.EnterDir(d, func(base string) ([]byte, error) {
			f, err := rv.root.Open(path.Join(local, base))
			if err != nil {
				return nil, err
			}
			defer f.Close()
			return io.ReadAll(f)
		})
		if err != nil {
			rv.s.sendError("%v", err)
		}
		rv.filterDirs = append(rv.filterDirs, filterDir{dir: d, leave: leave})
	}
}
