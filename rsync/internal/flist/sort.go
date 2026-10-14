// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package flist

import (
	"slices"
	"strings"
)

// Sort sorts files stably into rsync's order for protocol proto. File-list
// indexes refer to positions in the sorted list.
func Sort(files []*File, proto int) {
	slices.SortStableFunc(files, func(a, b *File) int { return Compare(a, b, proto) })
}

// Clean marks duplicate entries of a sorted list with FlagCleared, the way
// rsync's receiver does, leaving their slots in place so indexes still
// match the sender's.
func Clean(files []*File) {
	kept := make(map[string]int, len(files))
	for i, f := range files {
		key := f.Name
		if f.IsDir() {
			if key == "." {
				key = ""
			} else {
				key = strings.TrimSuffix(key, "/.")
			}
		}
		j, dup := kept[key]
		if !dup {
			kept[key] = i
			continue
		}
		prev := files[j]
		switch {
		case f.IsDir() && !prev.IsDir():
			prev.Flags |= FlagCleared
			kept[key] = i
		case f.IsDir():
			prev.Flags |= f.Flags & (FlagTopDir | FlagContentDir)
			if f.Flags&FlagImpliedDir == 0 {
				prev.Flags &^= FlagImpliedDir
			}
			f.Flags |= FlagCleared
		default:
			f.Flags |= FlagCleared
		}
	}
}
