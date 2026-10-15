// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package rsync

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"

	"github.com/parallelworks/xfer/rsync/internal/checksum"
	"github.com/parallelworks/xfer/rsync/internal/filter"
	"github.com/parallelworks/xfer/rsync/internal/flist"
)

// scanner builds the sender's file list from source paths.
type scanner struct {
	s       *session
	o       *Options
	filters *filter.List
	files   []*flist.File

	implied map[string]bool // -R ancestors already listed
	rootDev uint64          // device of the current top-level argument, for -x

	// With incremental recursion, walk lists a directory's contents
	// without descending, and leaves holds the filter scopes of transfer
	// roots that stay entered while their lists are sent.
	inc    bool
	leaves []func()
}

func (s *session) newScanner(filters *filter.List) *scanner {
	return &scanner{s: s, o: s.opts, filters: filters, implied: map[string]bool{}, inc: s.inc}
}

// scanArgs lists the source arguments: everything, or with incremental
// recursion the top level of the transfer. The entries are in the order
// they are sent, not sorted.
func (sc *scanner) scanArgs(args []string) []*flist.File {
	if len(args) == 0 && (sc.o.Recursive || sc.o.Dirs || sc.o.ListOnly) {
		args = []string{"."}
	}
	for _, arg := range args {
		sc.addArg(arg)
	}
	return sc.take()
}

// take returns the entries listed since the last call, with --checksum
// sums computed.
func (sc *scanner) take() []*flist.File {
	if sc.o.Checksum {
		sc.checksumAll()
	}
	files := sc.files
	sc.files = nil
	return files
}

// close leaves the filter scopes still entered.
func (sc *scanner) close() {
	for i := len(sc.leaves) - 1; i >= 0; i-- {
		sc.leaves[i]()
	}
	sc.leaves = nil
}

func (sc *scanner) ioError(bits int32, format string, args ...any) {
	sc.s.ioErr.Or(bits)
	sc.s.sendError(format, args...)
}

// stat returns the attributes the sender uses for p, whose name in the
// transfer is name.
func (sc *scanner) stat(p, name string, top bool) (fs.FileInfo, error) {
	fi, err := os.Lstat(p)
	if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
		return fi, err
	}
	follow := sc.o.CopyLinks
	if !follow && top && sc.o.CopyDirlinks {
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			return st, nil
		}
	}
	if !follow && sc.o.CopyUnsafeLinks {
		target, err := os.Readlink(p)
		if err == nil && unsafeLink(target, name) {
			follow = true
		}
	}
	if follow {
		st, err := os.Stat(p)
		if err != nil {
			return nil, &fs.PathError{Op: "symlink has no referent", Path: p, Err: err}
		}
		return st, nil
	}
	return fi, nil
}

func (sc *scanner) addArg(arg string) {
	o := sc.o
	var fsDir, name string
	dotDir := false

	if o.Relative {
		fsDir, name = ".", arg
		if i := strings.Index(arg, "/./"); i >= 0 {
			fsDir, name = arg[:i], arg[i+3:]
			if fsDir == "" {
				fsDir = "/"
			}
		} else if strings.HasPrefix(arg, "/") {
			fsDir = "/"
		}
		name = strings.TrimLeft(path.Clean("/"+name), "/")
		if name == "" {
			name, dotDir = ".", true
		}
		if strings.HasSuffix(arg, "/") {
			dotDir = true
		}
	} else {
		trimmed := arg
		switch {
		case arg == "" || strings.HasSuffix(arg, "/"):
			trimmed = strings.TrimRight(arg, "/")
			if trimmed == "" {
				trimmed = map[bool]string{true: "/", false: "."}[strings.HasPrefix(arg, "/")]
			}
			dotDir = true
		case arg == "." || strings.HasSuffix(arg, "/."):
			trimmed = strings.TrimSuffix(arg, "/.")
			if trimmed == "" {
				trimmed = "."
			}
			dotDir = true
		case arg == ".." || strings.HasSuffix(arg, "/.."):
			dotDir = true
		}
		if dotDir {
			fsDir, name = trimmed, "."
		} else {
			fsDir, name = path.Split(arg)
			if fsDir == "" {
				fsDir = "."
			}
		}
	}

	p := filepath.Join(fsDir, name)
	fi, err := sc.stat(p, name, true)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			sc.ioError(ioErrGeneral, "link_stat %q failed: No such file or directory (2)", absPath(p))
		} else {
			sc.ioError(ioErrGeneral, "%v", err)
		}
		return
	}
	if !dotDir && name != "." && sc.filters.Excluded(name, fi.IsDir(), filter.SenderSide) {
		return
	}
	if o.Relative && o.ImpliedDirs {
		sc.addImplied(fsDir, name)
	}
	sc.rootDev = statSys(fi).dev

	if fi.IsDir() {
		switch {
		case o.Recursive:
		case o.Dirs:
			if !dotDir {
				sc.addEntry(p, name, fi, 0)
				return
			}
		default:
			sc.s.info("skipping directory " + name + "\n")
			return
		}
		f := sc.addEntry(p, name, fi, flist.FlagTopDir|flist.FlagContentDir)
		if f == nil {
			return
		}
		f.RootDev = sc.rootDev
		if !sc.inc {
			sc.walk(p, name, true)
		} else if dotDir {
			// The contents of a dot-dir argument are in the first list;
			// its subdirectories get lists of their own.
			if leave := sc.walkDir(p, name); leave != nil {
				sc.leaves = append(sc.leaves, leave)
			}
		}
		return
	}
	sc.addEntry(p, name, fi, 0)
}

// addImplied lists the ancestors of a -R name.
func (sc *scanner) addImplied(fsDir, name string) {
	dir := path.Dir(name)
	if dir == "." {
		return
	}
	parts := strings.Split(dir, "/")
	for i := range parts {
		anc := strings.Join(parts[:i+1], "/")
		if sc.implied[anc] {
			continue
		}
		sc.implied[anc] = true
		fi, err := os.Stat(filepath.Join(fsDir, anc))
		if err != nil {
			sc.ioError(ioErrGeneral, "link_stat %q failed: %v", anc, err)
			return
		}
		sc.addEntry(filepath.Join(fsDir, anc), anc, fi, flist.FlagImpliedDir)
	}
}

// addEntry appends a file-list entry for the file at p named name.
func (sc *scanner) addEntry(p, name string, fi fs.FileInfo, flags uint32) *flist.File {
	if len(name) >= 1024 {
		sc.ioError(ioErrGeneral, "skipping overly long name: %s", name)
		return nil
	}
	sys := statSys(fi)
	f := &flist.File{
		Name:    name,
		Mode:    wireMode(fi.Mode()),
		Size:    fi.Size(),
		ModTime: fi.ModTime().Unix(),
		ModNsec: sys.mtimeNsec,
		UID:     sys.uid,
		GID:     sys.gid,
		Flags:   flags,
		Source:  p,
	}
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(p)
		if err != nil {
			sc.ioError(ioErrGeneral, "readlink %q failed: %v", p, err)
			return nil
		}
		if target == "" {
			return nil
		}
		f.Link = target
	case f.IsDevice():
		f.RdevMajor, f.RdevMinor = devMajor(sys.rdev), devMinor(sys.rdev)
		f.Size = 0
	case f.IsSpecial():
		f.Size = 0
	}
	if f.IsDir() && f.Flags&flist.FlagImpliedDir == 0 && sc.o.Recursive {
		f.Flags |= flist.FlagContentDir
	}
	if sc.o.HardLinks && !f.IsDir() && sys.nlink > 1 {
		f.Flags |= flist.FlagHlinked
		f.Dev, f.Ino = int64(sys.dev), int64(sys.ino)
	}
	sc.files = append(sc.files, f)
	return f
}

// walk lists the contents of directory p (wire name dir), recursing with
// -r. top marks a transfer root, whose own -x device is the reference.
func (sc *scanner) walk(p, dir string, top bool) {
	if leave := sc.walkDir(p, dir); leave != nil {
		defer leave()
	}
}

// walkDir lists the contents of directory p (wire name dir). Without
// incremental recursion it descends into subdirectories; with it, they are
// listed later on their own. It returns the function that leaves the
// directory's filter scope, which with incremental recursion must stay
// entered while the subdirectories are listed.
func (sc *scanner) walkDir(p, dir string) (leave func()) {
	o := sc.o
	leave, err := sc.filters.EnterDir(dirKey(dir), func(base string) ([]byte, error) {
		return os.ReadFile(filepath.Join(p, base))
	})
	if err != nil {
		sc.ioError(ioErrGeneral, "%v", err)
	}

	d, err := openFast(p)
	if err != nil {
		sc.ioError(ioErrGeneral, "opendir %q failed: %v", p, errnoText(err))
		return leave
	}
	entries, err := d.ReadDir(-1)
	d.Close()
	if err != nil {
		sc.ioError(ioErrGeneral, "readdir %q failed: %v", p, errnoText(err))
	}

	type sub struct{ p, name string }
	var subdirs []sub
	for _, e := range entries {
		base := e.Name()
		childPath := filepath.Join(p, base)
		name := base
		if dir != "." {
			name = dir + "/" + base
		}
		fi, err := sc.stat(childPath, name, false)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				// Vanished between readdir and stat.
				sc.s.ioErr.Or(ioErrVanished)
				sc.s.log.errorf("file has vanished: %q\n", absPath(childPath))
			} else {
				sc.ioError(ioErrGeneral, "%v", err)
			}
			continue
		}
		if sc.filters.Excluded(name, fi.IsDir(), filter.SenderSide) {
			continue
		}
		var flags uint32
		if fi.IsDir() && o.OneFileSystem > 0 && statSys(fi).dev != sc.rootDev {
			if o.OneFileSystem > 1 {
				continue
			}
			flags |= flist.FlagMountDir
		}
		f := sc.addEntry(childPath, name, fi, flags)
		if f == nil || !fi.IsDir() || !o.Recursive || flags&flist.FlagMountDir != 0 {
			if f != nil && flags&flist.FlagMountDir != 0 {
				f.Flags &^= flist.FlagContentDir
			}
			continue
		}
		f.RootDev = sc.rootDev
		subdirs = append(subdirs, sub{childPath, name})
	}
	if !sc.inc {
		for _, sd := range subdirs {
			sc.walk(sd.p, sd.name, false)
		}
	}
	return leave
}

// dirKey is the directory name handed to per-directory filter scoping.
func dirKey(dir string) string {
	if dir == "." {
		return ""
	}
	return dir
}

// checksumAll computes --checksum file sums in parallel.
func (sc *scanner) checksumAll() {
	alg := sc.s.fileSum
	work := make(chan *flist.File)
	var wg sync.WaitGroup
	for range max(2, runtime.GOMAXPROCS(0)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range work {
				f.Checksum, _ = fileChecksum(f.Source, alg, sc.s.opts.OpenNoatime)
			}
		}()
	}
	for _, f := range sc.files {
		if f.IsRegular() {
			work <- f
		}
	}
	close(work)
	wg.Wait()
}

// fileChecksum computes the unseeded whole-file checksum used by -c.
// sumBufPool holds read buffers for whole-file checksums, which run for
// every file with --checksum.
var sumBufPool = sync.Pool{New: func() any { b := make([]byte, 256<<10); return &b }}

func fileChecksum(p string, alg checksum.Algorithm, noatime bool) ([]byte, error) {
	if alg == checksum.MD4Old {
		alg = checksum.MD4 // file-list sums are never seeded
	}
	file, err := openSource(p, noatime)
	if err != nil {
		return make([]byte, alg.Size()), err
	}
	defer file.Close()
	h := checksum.NewFile(alg, 0)
	bp := sumBufPool.Get().(*[]byte)
	defer sumBufPool.Put(bp)
	buf := *bp
	for {
		n, err := file.Read(buf)
		h.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return h.Sum(nil), nil
}

// unsafeLink reports whether a symlink target escapes the tree it is in:
// absolute targets, and relative ones that climb above the transfer root.
func unsafeLink(target, linkName string) bool {
	if strings.HasPrefix(target, "/") {
		return true
	}
	depth := strings.Count(path.Clean(linkName), "/")
	for _, c := range strings.Split(target, "/") {
		switch c {
		case "..":
			depth--
			if depth < 0 {
				return true
			}
		case ".", "":
		default:
			depth++
		}
	}
	return false
}

func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// errnoText renders an error the way rsync reports system errors:
// "No such file or directory (2)".
func errnoText(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno.Error() + " (" + itoa(int(errno)) + ")"
	}
	return err.Error()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
