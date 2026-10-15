// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package rsync_test

import (
	"bytes"
	"crypto/fips140"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The interop suite runs transfers between this implementation and a real
// rsync in every direction, over a fake remote shell that runs the "remote"
// command locally. It needs an rsync binary: $XFER_INTEROP_RSYNC, or
// "rsync" on $PATH. It is skipped with -short. $XFER_INTEROP_XFER names a
// prebuilt xfer binary; otherwise one is built with the go command.
//
// Scenario names are <client>-><server>/<direction>/<case>; for example
// "xfer->gnu/push/archive" runs our client pushing to a GNU rsync server.

var (
	buildOnce sync.Once
	xferBin   string
	buildErr  error
)

func xferBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		if bin := os.Getenv("XFER_INTEROP_XFER"); bin != "" {
			xferBin = bin // prebuilt, e.g. when running inside a container
			return
		}
		dir, err := os.MkdirTemp("", "xfer-interop-bin")
		if err != nil {
			buildErr = err
			return
		}
		xferBin = filepath.Join(dir, "xfer")
		args := []string{"build", "-o", xferBin}
		if os.Getenv("XFER_INTEROP_RACE") != "" {
			args = append(args, "-race")
		}
		args = append(args, "../cmd/xfer")
		cmd := exec.Command("go", args...)
		cmd.Stderr = os.Stderr
		buildErr = cmd.Run()
	})
	if buildErr != nil {
		t.Fatalf("building xfer: %v", buildErr)
	}
	return xferBin
}

type peer struct {
	name    string
	argv    []string // command to run as a client: argv + options + operands
	server  string   // value for --rsync-path to run it as a server
	version int      // protocol version the peer speaks
	about   string   // its --version output, listing optional features
}

// supports reports whether the peer offers an optional algorithm.
func (p *peer) supports(feature string) bool {
	return p.name == "xfer" || strings.Contains(p.about, feature)
}

func refRsync(t *testing.T) *peer {
	t.Helper()
	if testing.Short() {
		t.Skip("interop tests are skipped with -short")
	}
	bin := os.Getenv("XFER_INTEROP_RSYNC")
	if bin == "" {
		var err error
		if bin, err = exec.LookPath("rsync"); err != nil {
			t.Skip("no reference rsync found; set XFER_INTEROP_RSYNC")
		}
	}
	out, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		t.Skipf("%s --version: %v", bin, err)
	}
	m := regexp.MustCompile(`protocol version (\d+)`).FindSubmatch(out)
	if m == nil {
		t.Skipf("cannot determine protocol of %s", bin)
	}
	v, _ := strconv.Atoi(string(m[1]))
	name := "gnu"
	if bytes.Contains(out, []byte("openrsync")) {
		name = "openrsync"
	}
	return &peer{name: name, argv: []string{bin}, server: bin, version: v, about: string(out)}
}

func xferPeer(t *testing.T) *peer {
	bin := xferBinary(t)
	return &peer{name: "xfer", argv: []string{bin, "rsync"}, server: bin + " rsync", version: 31}
}

// fakeSSH writes a remote shell that ignores the host and runs the command
// locally, the way ssh would run it remotely.
func fakeSSH(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fakessh")
	script := "#!/bin/sh\n" +
		"while [ $# -gt 0 ]; do case \"$1\" in -l) shift 2;; -*) shift;; *) break;; esac; done\n" +
		"shift\nexec sh -c \"$*\"\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// tree describes a test file tree.
type tree map[string]entry

type entry struct {
	data   []byte
	link   string // symlink target
	dir    bool
	mode   fs.FileMode
	mtime  time.Time
	hardTo string // hard link to another entry
}

var epoch = time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)

func randData(r *rand.Rand, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.UintN(256))
	}
	return b
}

// sourceTree is a varied tree: small, empty and large files, nesting,
// symlinks, odd names and permissions.
func sourceTree(seed uint64) tree {
	r := rand.New(rand.NewPCG(seed, seed))
	t := tree{
		"a.txt":                {data: []byte("hello\n"), mode: 0o644},
		"empty":                {data: nil, mode: 0o600},
		"exec.sh":              {data: []byte("#!/bin/sh\necho hi\n"), mode: 0o755},
		"big.bin":              {data: randData(r, 3<<20+12345), mode: 0o644},
		"dir":                  {dir: true, mode: 0o755},
		"dir/nested":           {dir: true, mode: 0o750},
		"dir/nested/deep.txt":  {data: randData(r, 70000), mode: 0o640},
		"dir/emptydir":         {dir: true, mode: 0o700},
		"dir/link-rel":         {link: "../a.txt"},
		"dir/link-dangling":    {link: "nowhere/at/all"},
		"name with spaces.txt": {data: []byte("spaces"), mode: 0o644},
		"ünïcødé-文件.txt":       {data: []byte("utf8"), mode: 0o644},
		"weird'quote\"$name":   {data: []byte("q"), mode: 0o644},
		"skip.o":               {data: []byte("object"), mode: 0o644},
		"dir/skip.o":           {data: []byte("object"), mode: 0o644},
	}
	for i := range 40 {
		t[fmt.Sprintf("many/f%03d", i)] = entry{data: randData(r, int(r.UintN(5000))), mode: 0o644}
	}
	t["many"] = entry{dir: true, mode: 0o755}
	return t
}

func (tr tree) write(t *testing.T, root string) {
	t.Helper()
	names := make([]string, 0, len(tr))
	for n := range tr {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		e := tr[n]
		p := filepath.Join(root, n)
		os.MkdirAll(filepath.Dir(p), 0o755)
		switch {
		case e.dir:
			os.MkdirAll(p, 0o755)
		case e.link != "":
			if err := os.Symlink(e.link, p); err != nil {
				t.Fatal(err)
			}
		case e.hardTo != "":
		default:
			if err := os.WriteFile(p, e.data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, n := range names {
		if e := tr[n]; e.hardTo != "" {
			if err := os.Link(filepath.Join(root, e.hardTo), filepath.Join(root, n)); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Modes and times last, deepest first, so directory times stick.
	for i := len(names) - 1; i >= 0; i-- {
		e := tr[names[i]]
		p := filepath.Join(root, names[i])
		if e.link != "" || e.hardTo != "" {
			continue
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		os.Chmod(p, mode)
		mt := e.mtime
		if mt.IsZero() {
			mt = epoch.Add(time.Duration(i) * time.Minute)
		}
		os.Chtimes(p, mt, mt)
	}
}

// snapshot captures what matters about a tree for comparison.
func snapshot(t *testing.T, root string, withTimes, withPerms bool) map[string]string {
	t.Helper()
	snap := map[string]string{}
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(root, p)
		fi, _ := os.Lstat(p)
		var desc string
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			desc = "link:" + target
		case fi.IsDir():
			desc = "dir"
		default:
			data, _ := os.ReadFile(p)
			desc = fmt.Sprintf("file:%x", sha256.Sum256(data))
			if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
				desc += fmt.Sprintf(" nlink=%d", st.Nlink)
			}
		}
		if withPerms && fi.Mode()&fs.ModeSymlink == 0 {
			desc += fmt.Sprintf(" mode=%o", fi.Mode().Perm())
		}
		if withTimes && fi.Mode()&fs.ModeSymlink == 0 && (rel != "." || !fi.IsDir()) {
			desc += " mtime=" + strconv.FormatInt(fi.ModTime().Unix(), 10)
		}
		snap[rel] = desc
		return nil
	})
	return snap
}

func diffSnapshots(want, got map[string]string) string {
	var b strings.Builder
	keys := map[string]bool{}
	for k := range want {
		keys[k] = true
	}
	for k := range got {
		keys[k] = true
	}
	sorted := slices.Sorted(func(yield func(string) bool) {
		for k := range keys {
			if !yield(k) {
				return
			}
		}
	})
	for _, k := range sorted {
		if want[k] != got[k] {
			fmt.Fprintf(&b, "  %s:\n    want %q\n    got  %q\n", k, want[k], got[k])
		}
	}
	return b.String()
}

type scenario struct {
	name string
	opts []string
	// prepare edits the source and/or destination before the transfer.
	prepare func(t *testing.T, src, dst string)
	// check verifies the destination; nil compares it with the source.
	check        func(t *testing.T, src, dst string, out string)
	times, perms bool
	minProto     int    // skip when the reference speaks an older protocol
	needs        string // skip unless both peers offer this algorithm
}

func copyTree(t *testing.T, from, to string) {
	t.Helper()
	cmd := exec.Command("cp", "-a", from+"/.", to)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cp: %v\n%s", err, out)
	}
}

// staleDest fills dst with an outdated copy of src so transfers exercise
// the delta algorithm, deletion and attribute updates.
func staleDest(t *testing.T, src, dst string) {
	copyTree(t, src, dst)
	big := filepath.Join(dst, "big.bin")
	data, _ := os.ReadFile(big)
	copy(data[100000:], []byte("CHANGED REGION"))
	data = append(data[:2000000], data[2001000:]...)
	os.WriteFile(big, data, 0o644)
	os.Chtimes(big, epoch, epoch)
	os.WriteFile(filepath.Join(dst, "a.txt"), []byte("stale!\n"), 0o644)
	os.WriteFile(filepath.Join(dst, "extra-file"), []byte("delete me"), 0o644)
	os.MkdirAll(filepath.Join(dst, "extra-dir/sub"), 0o755)
	os.WriteFile(filepath.Join(dst, "extra-dir/sub/x"), []byte("x"), 0o644)
	os.Chmod(filepath.Join(dst, "exec.sh"), 0o644)
	os.Remove(filepath.Join(dst, "dir/nested/deep.txt"))
}

// shiftDest makes big.bin's basis a forward shift of the source (data
// removed at the front) and a.txt's a backward shift (data added), the
// cases an --inplace sender must not reference overwritten blocks for.
func shiftDest(t *testing.T, src, dst string) {
	copyTree(t, src, dst)
	big := filepath.Join(dst, "big.bin")
	data, _ := os.ReadFile(big)
	os.WriteFile(big, data[7777:], 0o644)
	os.Chtimes(big, epoch, epoch)
	a := filepath.Join(dst, "a.txt")
	data, _ = os.ReadFile(a)
	os.WriteFile(a, append(bytes.Repeat([]byte("prefix "), 3000), data...), 0o644)
	os.Chtimes(a, epoch, epoch)
}

// manyFiles adds a large file followed by many small ones. While the
// sender streams the large file the generator runs ahead, so both
// directions fill their pipe buffers at once; that deadlocks if a reader
// ever waits on a write.
// unsafeLinks adds symlinks that stay inside the tree and one that
// escapes it, nested so that the link's depth in the transfer matters.
func unsafeLinks(t *testing.T, src, dst string) {
	os.WriteFile(filepath.Join(filepath.Dir(src), "outside.txt"), []byte("outside\n"), 0o644)
	os.MkdirAll(filepath.Join(src, "u/v"), 0o755)
	os.Symlink("../../outside.txt", filepath.Join(src, "u/escape"))
	os.Symlink("../../a.txt", filepath.Join(src, "u/v/inside"))
}

func checkUnsafeLinks(t *testing.T, src, dst, out string) {
	if fi, err := os.Lstat(filepath.Join(dst, "u/escape")); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("u/escape: want a regular file, got %v %v", fi, err)
	} else if b, _ := os.ReadFile(filepath.Join(dst, "u/escape")); string(b) != "outside\n" {
		t.Errorf("u/escape: content %q", b)
	}
	if target, err := os.Readlink(filepath.Join(dst, "u/v/inside")); err != nil || target != "../../a.txt" {
		t.Errorf("u/v/inside: want symlink to ../../a.txt, got %q %v", target, err)
	}
}

// sparseFile adds a file that is mostly holes.
func sparseFile(t *testing.T, src, dst string) {
	f, err := os.Create(filepath.Join(src, "sparse.bin"))
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("data"), 1<<18)
	f.Write(data)
	f.WriteAt(data, 9<<20)
	f.Truncate(16 << 20)
	f.Close()
}

func checkSparse(t *testing.T, src, dst, out string) {
	if d := diffSnapshots(snapshot(t, src, true, false), snapshot(t, dst, true, false)); d != "" {
		t.Errorf("destination differs:\n%s", d)
	}
	used := func(p string) (int64, int64) {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		return int64(fi.Sys().(*syscall.Stat_t).Blocks) * 512, fi.Size()
	}
	if u, size := used(filepath.Join(src, "sparse.bin")); u > size/2 {
		t.Log("the file system does not keep holes; not checking sparseness")
		return
	}
	if u, size := used(filepath.Join(dst, "sparse.bin")); u > size/2 {
		t.Errorf("sparse.bin uses %d bytes of %d: not sparse", u, size)
	}
}

func checkBackup(t *testing.T, src, dst, out string) {
	if d := diffSnapshots(snapshot(t, src, true, false), snapshot(t, dst, true, false)); !strings.Contains(d, "big.bin~") || strings.Contains(d, "big.bin:") {
		t.Errorf("want the source plus backups:\n%s", d)
	}
	for _, name := range []string{"big.bin~", "extra-file~", "extra-dir/sub/x~"} {
		if _, err := os.Lstat(filepath.Join(dst, name)); err != nil {
			t.Errorf("backup %s: %v", name, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(dst, "extra-file")); err == nil {
		t.Error("extra-file was not deleted")
	}
}

func checkBackupDir(t *testing.T, src, dst, out string) {
	if d := diffSnapshots(snapshot(t, src, true, false), snapshot(t, dst, true, false)); d != "" {
		t.Errorf("destination differs:\n%s", d)
	}
	bak := filepath.Join(filepath.Dir(dst), "bak")
	for _, name := range []string{"big.bin", "extra-file", "extra-dir/sub/x"} {
		if _, err := os.Lstat(filepath.Join(bak, name)); err != nil {
			t.Errorf("backup %s: %v", name, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(bak, "big.bin")); bytes.Equal(b, mustRead(t, filepath.Join(src, "big.bin"))) {
		t.Error("backup of big.bin holds the new contents")
	}
}

// prevTree makes ../prev, an older copy of the source in which big.bin
// differs.
func prevTree(t *testing.T, src, dst string) {
	prev := filepath.Join(filepath.Dir(dst), "prev")
	os.Mkdir(prev, 0o755)
	copyTree(t, src, prev)
	big := filepath.Join(prev, "big.bin")
	data := mustRead(t, big)
	copy(data[1000:], "CHANGED")
	os.WriteFile(big, data, 0o644)
}

func sameInode(t *testing.T, a, b string) bool {
	fa, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	fb, err := os.Stat(b)
	if err != nil {
		t.Fatal(err)
	}
	return os.SameFile(fa, fb)
}

func checkBasisDest(mode string) func(t *testing.T, src, dst, out string) {
	return func(t *testing.T, src, dst, out string) {
		prev := filepath.Join(filepath.Dir(dst), "prev")
		if mode == "compare" {
			if _, err := os.Lstat(filepath.Join(dst, "a.txt")); err == nil {
				t.Error("a.txt is unchanged in the compare-dest but was copied")
			}
			if !bytes.Equal(mustRead(t, filepath.Join(dst, "big.bin")), mustRead(t, filepath.Join(src, "big.bin"))) {
				t.Error("big.bin differs")
			}
			return
		}
		// Links into prev add to the link counts.
		dsnap := snapshot(t, dst, true, true)
		for k, v := range dsnap {
			dsnap[k] = regexp.MustCompile(` nlink=\d+`).ReplaceAllString(v, "")
		}
		if d := diffSnapshots(snapshot(t, src, true, true), dsnap); d != "" {
			t.Errorf("destination differs:\n%s", d)
		}
		if linked := sameInode(t, filepath.Join(prev, "a.txt"), filepath.Join(dst, "a.txt")); linked != (mode == "link") {
			t.Errorf("a.txt linked=%v, want %v", linked, mode == "link")
		}
		if sameInode(t, filepath.Join(prev, "big.bin"), filepath.Join(dst, "big.bin")) {
			t.Error("big.bin was linked to a different file")
		}
	}
}

// filesFromList writes the --files-from lists ../list and ../list0.
func filesFromList(t *testing.T, src, dst string) {
	base := filepath.Dir(src)
	os.WriteFile(filepath.Join(base, "list"), []byte("a.txt\n\ndir/nested/deep.txt\ndir/emptydir\nname with spaces.txt\n"), 0o644)
	os.WriteFile(filepath.Join(base, "list0"), []byte("a.txt\x00dir/nested/deep.txt\x00dir/emptydir\x00name with spaces.txt\x00"), 0o644)
}

func checkFilesFrom(t *testing.T, src, dst, out string) {
	got := snapshot(t, dst, false, false)
	want := []string{".", "a.txt", "dir", "dir/emptydir", "dir/nested", "dir/nested/deep.txt", "name with spaces.txt"}
	var names []string
	for k := range got {
		names = append(names, k)
	}
	slices.Sort(names)
	if !slices.Equal(names, want) {
		t.Errorf("got %q, want %q", names, want)
	}
	if !bytes.Equal(mustRead(t, filepath.Join(dst, "dir/nested/deep.txt")), mustRead(t, filepath.Join(src, "dir/nested/deep.txt"))) {
		t.Error("deep.txt differs")
	}
}

// expandOpts fills in "{base}", the directory holding src and dst.
func expandOpts(opts []string, base string) []string {
	out := make([]string, len(opts))
	for i, o := range opts {
		out[i] = strings.ReplaceAll(o, "{base}", base)
	}
	return out
}

// mergeDelete puts a per-directory filter file excluding *.tmp in dir, on
// both sides, plus an excluded file and an extraneous file in the
// destination.
func mergeDelete(t *testing.T, src, dst string) {
	for _, root := range []string{src, dst} {
		os.MkdirAll(filepath.Join(root, "dir"), 0o755)
		os.WriteFile(filepath.Join(root, "dir/.rsync-filter"), []byte("- *.tmp\n"), 0o644)
	}
	os.WriteFile(filepath.Join(src, "dir/skipped.tmp"), []byte("not sent\n"), 0o644)
	os.WriteFile(filepath.Join(dst, "dir/keep.tmp"), []byte("protected\n"), 0o644)
	os.WriteFile(filepath.Join(dst, "dir/gone.txt"), []byte("extraneous\n"), 0o644)
}

func checkMergeDelete(t *testing.T, src, dst, out string) {
	if _, err := os.Lstat(filepath.Join(dst, "dir/keep.tmp")); err != nil {
		t.Errorf("dir/keep.tmp, excluded by dir/.rsync-filter, was deleted: %v", err)
	}
	for _, name := range []string{"dir/gone.txt", "dir/skipped.tmp"} {
		if _, err := os.Lstat(filepath.Join(dst, name)); err == nil {
			t.Errorf("%s exists", name)
		}
	}
	if _, err := os.Lstat(filepath.Join(dst, "dir/nested/deep.txt")); err != nil {
		t.Error(err)
	}
}

func mustRead(t *testing.T, p string) []byte {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func manyFiles(t *testing.T, src, dst string) {
	big := make([]byte, 24<<20)
	rand.NewChaCha8([32]byte{1}).Read(big)
	os.WriteFile(filepath.Join(src, "a-big"), big, 0o644)
	for d := range 30 {
		dir := filepath.Join(src, fmt.Sprintf("m%02d", d))
		os.MkdirAll(dir, 0o755)
		for f := range 200 {
			os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%03d", f)), []byte{byte(d), byte(f)}, 0o644)
		}
	}
}

var scenarios = []scenario{
	{name: "archive", opts: []string{"-a"}, times: true, perms: true},
	{name: "archive-delta-delete", opts: []string{"-a", "--delete", "--no-W"}, prepare: staleDest, times: true, perms: true},
	{name: "compress", opts: []string{"-az", "--no-W"}, prepare: staleDest, times: true,
		check: checkIgnoring("extra-file", "extra-dir", "extra-dir/sub", "extra-dir/sub/x")},
	{name: "checksum", opts: []string{"-rlc", "--no-W"}, prepare: staleDest,
		check: checkIgnoring("extra-file", "extra-dir", "extra-dir/sub", "extra-dir/sub/x")},
	{name: "delete-after", opts: []string{"-rlt", "--delete-after"}, prepare: staleDest, times: true},
	{name: "exclude", opts: []string{"-a", "--exclude=*.o", "--exclude=/many/f01*"}, times: true,
		check: checkWithout("skip.o", "dir/skip.o", "many/f010", "many/f011", "many/f012", "many/f013", "many/f014",
			"many/f015", "many/f016", "many/f017", "many/f018", "many/f019")},
	{name: "delete-protect", opts: []string{"-a", "--delete", "--filter=P extra-file"}, prepare: staleDest, times: true,
		check: checkIgnoring("extra-file")},
	{name: "dry-run", opts: []string{"-a", "-n", "--delete"}, prepare: staleDest, check: checkUnchanged},
	{name: "hardlinks", opts: []string{"-aH"}, prepare: addHardlinks, times: true},
	{name: "itemize", opts: []string{"-ai", "--delete", "--no-W"}, prepare: staleDest, times: true, perms: true,
		check: checkItemized},
	{name: "inplace", opts: []string{"-a", "--inplace", "--no-W"}, prepare: staleDest, times: true,
		check: checkIgnoring("extra-file", "extra-dir", "extra-dir/sub", "extra-dir/sub/x")},
	{name: "inplace-shift", opts: []string{"-a", "--inplace", "--no-W"}, prepare: shiftDest, times: true},
	{name: "protocol-29", opts: []string{"-a", "--protocol=29", "--delete", "--no-W"}, prepare: staleDest, times: true},
	{name: "zstd", needs: "zstd", opts: []string{"-a", "--compress-choice=zstd", "--no-W"}, prepare: staleDest, times: true, minProto: 31,
		check: checkIgnoring("extra-file", "extra-dir", "extra-dir/sub", "extra-dir/sub/x")},
	{name: "zlibx", needs: "zlibx", opts: []string{"-a", "--compress-choice=zlibx", "--no-W"}, prepare: staleDest, times: true, minProto: 31,
		check: checkIgnoring("extra-file", "extra-dir", "extra-dir/sub", "extra-dir/sub/x")},
	{name: "xxh128", needs: "xxh128", opts: []string{"-a", "--checksum-choice=xxh128", "--no-W"}, prepare: staleDest, times: true, minProto: 31,
		check: checkIgnoring("extra-file", "extra-dir", "extra-dir/sub", "extra-dir/sub/x")},
	{name: "lz4", needs: "lz4", opts: []string{"-a", "--compress-choice=lz4", "--no-W"}, prepare: staleDest, times: true, minProto: 31,
		check: checkIgnoring("extra-file", "extra-dir", "extra-dir/sub", "extra-dir/sub/x")},
	{name: "many-files", opts: []string{"-a"}, prepare: manyFiles, times: true},
	{name: "delay-updates", opts: []string{"-a", "--delay-updates", "--delete", "--no-W"}, prepare: staleDest, times: true, perms: true},
	{name: "delay-updates-hardlinks", opts: []string{"-aH", "--delay-updates"}, prepare: addHardlinks, times: true},
	{name: "backup", opts: []string{"-a", "-b", "--delete", "--no-W"}, prepare: staleDest, check: checkBackup},
	{name: "backup-dir", opts: []string{"-a", "--backup-dir=../bak", "--delete", "--no-W"}, prepare: staleDest, check: checkBackupDir},
	{name: "link-dest", opts: []string{"-a", "--link-dest=../prev", "--no-W"}, prepare: prevTree, check: checkBasisDest("link")},
	{name: "copy-dest", opts: []string{"-a", "--copy-dest=../prev", "--no-W"}, prepare: prevTree, check: checkBasisDest("copy")},
	{name: "compare-dest", opts: []string{"-a", "--compare-dest=../prev", "--no-W"}, prepare: prevTree, check: checkBasisDest("compare")},
	{name: "files-from", opts: []string{"-a", "--files-from={base}/list"}, prepare: filesFromList, check: checkFilesFrom},
	{name: "files-from0", opts: []string{"-a", "--from0", "--files-from={base}/list0"}, prepare: filesFromList, check: checkFilesFrom},
	{name: "files-from-remote", opts: []string{"-a", "--files-from=:{base}/list"}, prepare: filesFromList, check: checkFilesFrom},
	{name: "merge-delete", opts: []string{"-a", "-F", "--delete"}, prepare: mergeDelete, check: checkMergeDelete},
	{name: "merge-delete-after", opts: []string{"-a", "-F", "--delete-after"}, prepare: mergeDelete, check: checkMergeDelete},
	{name: "sparse", opts: []string{"-a", "-S"}, prepare: sparseFile, check: checkSparse},
	{name: "timeout", opts: []string{"-a", "--timeout=1", "--delete", "--no-W"}, prepare: staleDest, times: true},
	{name: "copy-unsafe-links", opts: []string{"-a", "--copy-unsafe-links"}, prepare: unsafeLinks, check: checkUnsafeLinks},
	{name: "no-inc-recursive", opts: []string{"-a", "--no-inc-recursive", "--delete", "--no-W"}, prepare: staleDest, times: true, perms: true},
	{name: "protect-args", opts: []string{"-a", "-s"}, times: true},
}

func checkWithout(names ...string) func(*testing.T, string, string, string) {
	return func(t *testing.T, src, dst, _ string) {
		want := snapshot(t, src, true, false)
		for _, n := range names {
			delete(want, n)
		}
		if d := diffSnapshots(want, snapshot(t, dst, true, false)); d != "" {
			t.Errorf("destination differs:\n%s", d)
		}
	}
}

func checkIgnoring(extra ...string) func(*testing.T, string, string, string) {
	return func(t *testing.T, src, dst, _ string) {
		got := snapshot(t, dst, false, false)
		for _, n := range extra {
			delete(got, n)
		}
		if d := diffSnapshots(snapshot(t, src, false, false), got); d != "" {
			t.Errorf("destination differs:\n%s", d)
		}
	}
}

func checkUnchanged(t *testing.T, src, dst, _ string) {
	// staleDest produced the destination; a dry run must leave it alone.
	if _, err := os.Stat(filepath.Join(dst, "extra-file")); err != nil {
		t.Errorf("dry run deleted extra-file")
	}
	if data, _ := os.ReadFile(filepath.Join(dst, "a.txt")); string(data) != "stale!\n" {
		t.Errorf("dry run modified a.txt")
	}
}

func checkItemized(t *testing.T, src, dst, out string) {
	if d := diffSnapshots(snapshot(t, src, true, true), snapshot(t, dst, true, true)); d != "" {
		t.Errorf("destination differs:\n%s", d)
	}
	// Outgoing files are marked '<' when pushing and '>' when pulling.
	dir := ">"
	if strings.Contains(out, "<f") {
		dir = "<"
	}
	for _, want := range []string{
		"*deleting   extra-file",
		dir + "f.st...... a.txt",
		dir + "f+++++++++ dir/nested/deep.txt",
		".f...p..... exec.sh",
		".d..t...... dir/nested/",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("itemized output lacks %q:\n%s", want, out)
		}
	}
}

func addHardlinks(t *testing.T, src, dst string) {
	os.Link(filepath.Join(src, "a.txt"), filepath.Join(src, "dir/hard-a"))
	os.Link(filepath.Join(src, "a.txt"), filepath.Join(src, "many/hard-a2"))
}

// TestInterop runs every scenario between each pair of implementations.
func TestInterop(t *testing.T) {
	ref := refRsync(t)
	ours := xferPeer(t)
	ssh := fakeSSH(t)
	t.Logf("reference: %s, protocol %d", ref.argv[0], ref.version)

	pairs := []struct{ client, server *peer }{
		{ours, ref}, {ref, ours}, {ours, ours},
	}
	for _, pr := range pairs {
		for _, dir := range []string{"push", "pull"} {
			for _, sc := range scenarios {
				name := fmt.Sprintf("%s->%s/%s/%s", pr.client.name, pr.server.name, dir, sc.name)
				t.Run(name, func(t *testing.T) {
					if sc.minProto > min(pr.client.version, pr.server.version) {
						t.Skipf("needs protocol %d", sc.minProto)
					}
					if sc.needs != "" && !(pr.client.supports(sc.needs) && pr.server.supports(sc.needs)) {
						t.Skipf("needs %s support", sc.needs)
					}
					if pr.client.name == "openrsync" || pr.server.name == "openrsync" {
						if slices.ContainsFunc(sc.opts, func(o string) bool {
							return strings.HasPrefix(o, "--compress-choice") || strings.HasPrefix(o, "--checksum-choice") ||
								o == "-s" || o == "--inplace" || o == "--delete-after" || o == "--no-inc-recursive" || o == "--delay-updates" || o == "-b" || o == "-F" || strings.HasPrefix(o, "--files-from=:") || strings.HasPrefix(o, "--backup-dir") || o == "--copy-unsafe-links" || strings.HasPrefix(o, "--protocol")
						}) {
							t.Skip("not supported by openrsync")
						}
						sender := pr.server
						if dir == "push" {
							sender = pr.client
						}
						if sc.name == "copy-unsafe-links" && sender.name == "openrsync" {
							// openrsync's sender stats every symlink under
							// --copy-unsafe-links and fails on the safe,
							// dangling dir/link-dangling, which rsync keeps.
							t.Skip("openrsync sender fails on dangling safe symlinks")
						}
					}
					if fips140.Enabled() && !fipsCapable(sc, pr.client, pr.server) {
						// xfer refuses MD4/MD5 by design in FIPS mode;
						// TestFIPS covers the refusal.
						t.Skip("needs MD4/MD5, refused in FIPS mode")
					}
					t.Parallel()
					runScenario(t, sc, pr.client, pr.server, dir == "push", ssh)
				})
			}
		}
	}
}

// fipsCapable reports whether a scenario can run between two peers in FIPS
// mode, where only xxHash checksums may be negotiated: that needs protocol
// 30 or newer and an xxHash algorithm on both sides (GNU rsync 3.2.0+).
func fipsCapable(sc scenario, client, server *peer) bool {
	for _, o := range sc.opts {
		if v, ok := strings.CutPrefix(o, "--protocol="); ok {
			if n, _ := strconv.Atoi(v); n < 30 {
				return false
			}
		}
	}
	return min(client.version, server.version) >= 30 && client.supports("xxh") && server.supports("xxh")
}

func runScenario(t *testing.T, sc scenario, client, server *peer, push bool, ssh string) {
	base := t.TempDir()
	src, dst := filepath.Join(base, "src"), filepath.Join(base, "dst")
	os.Mkdir(src, 0o755)
	os.Mkdir(dst, 0o755)
	sourceTree(1).write(t, src)
	if sc.prepare != nil {
		sc.prepare(t, src, dst)
	}

	args := append([]string{}, client.argv[1:]...)
	args = append(args, expandOpts(sc.opts, base)...)
	args = append(args, "-e", ssh, "--rsync-path="+server.server)
	if push {
		args = append(args, src+"/", "host:"+dst+"/")
	} else {
		args = append(args, "host:"+src+"/", dst+"/")
	}
	cmd := exec.Command(client.argv[0], args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s %s: %v\nstdout:\n%s\nstderr:\n%s", client.argv[0], strings.Join(args, " "), err, stdout.String(), stderr.String())
		}
	case <-time.After(2 * time.Minute):
		cmd.Process.Kill()
		buf := make([]byte, 1<<20)
		t.Fatalf("transfer hung\nstdout:\n%s\nstderr:\n%s\n%s", stdout.String(), stderr.String(), buf[:runtime.Stack(buf, true)])
	}
	if sc.check != nil {
		if sc.name == "itemize" && (client.name == "openrsync" || server.name == "openrsync") {
			// openrsync emulates rsync 2.6.9's 9-character itemize
			// format; only the result is comparable.
			if d := diffSnapshots(snapshot(t, src, true, true), snapshot(t, dst, true, true)); d != "" {
				t.Errorf("destination differs:\n%s", d)
			}
			return
		}
		sc.check(t, src, dst, stdout.String())
		return
	}
	if d := diffSnapshots(snapshot(t, src, sc.times, sc.perms), snapshot(t, dst, sc.times, sc.perms)); d != "" {
		t.Errorf("destination differs from source:\n%s\nstdout:\n%s\nstderr:\n%s", d, stdout.String(), stderr.String())
	}
}
