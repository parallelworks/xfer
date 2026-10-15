// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package rsync

import (
	"path"
	"strings"

	"github.com/parallelworks/xfer/rsync/internal/filter"
)

// defaultSkipCompress lists the suffixes of files that are already
// compressed, which -z sends without compressing again unless
// --skip-compress gives another list. It is the list rsync documents.
const defaultSkipCompress = "3g2/3gp/7z/aac/ace/apk/avi/bz2/deb/dmg/ear/f4v/flac/flv/gpg/gz/iso/jar/jpeg/jpg/lrz/lz/lz4/lzma/lzo/m1a/m1v/m2a/m2ts/m2v/m4a/m4b/m4p/m4r/m4v/mka/mkv/mov/mp1/mp2/mp3/mp4/mpa/mpeg/mpg/mpv/mts/odb/odf/odg/odi/odm/odp/ods/odt/oga/ogg/ogm/ogv/ogx/opus/otg/oth/otp/ots/ott/oxt/png/qt/rar/rpm/rz/rzip/spx/squashfs/sxc/sxd/sxg/sxm/sxw/sz/tbz/tbz2/tgz/tlz/ts/txz/tzo/vob/war/webm/webp/xz/z/zip/zst"

// skipCompress matches file names against a --skip-compress list: suffixes
// separated by slashes, compared without case, which may use wildcards.
type skipCompress struct {
	exact    map[string]bool
	patterns []string
}

func newSkipCompress(o *Options) *skipCompress {
	list := o.SkipCompress
	if list == "" && !o.set["skip-compress"] {
		list = defaultSkipCompress
	}
	sc := &skipCompress{exact: map[string]bool{}}
	for s := range strings.SplitSeq(strings.ToLower(list), "/") {
		switch {
		case s == "":
		case strings.ContainsAny(s, "*?["):
			sc.patterns = append(sc.patterns, s)
		default:
			sc.exact[s] = true
		}
	}
	return sc
}

// skip reports whether name's data should be sent uncompressed.
func (sc *skipCompress) skip(name string) bool {
	base := path.Base(name)
	i := strings.LastIndexByte(base, '.')
	if i < 0 {
		return false
	}
	suffix := strings.ToLower(base[i+1:])
	if sc.exact[suffix] {
		return true
	}
	for _, p := range sc.patterns {
		if filter.WildMatch(p, suffix) {
			return true
		}
	}
	return false
}
