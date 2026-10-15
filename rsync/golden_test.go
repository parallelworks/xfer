// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package rsync_test

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The golden tests hold our output to rsync's, byte for byte, for the
// common output options. testdata/golden has the output of GNU rsync 3.5.1
// for a fixed tree; regenerate it with
//
//	XFER_GOLDEN_UPDATE=1 XFER_INTEROP_RSYNC=/path/to/rsync-3.5.1 go test -run TestGolden ./rsync
//
// Each case runs three times against the same destination: the initial
// copy, a run with nothing to do, and a run after the source changed. The
// no-op output must match exactly; for the others the parts that depend on
// timing are masked (see normalize). Even rsync's byte counts vary by a
// few bytes from run to run, with the framing of its output (the file
// list size counts frame headers too), so those are
// compared with a tolerance. The cases leave out -o and -g, whose user and
// group names would make the counts depend on the machine, and %l, which
// for a directory depends on the filesystem.

type goldenCase struct {
	name string
	args []string
	// dest prepares the destination before the first run.
	dest func(t *testing.T, dst string)
}

var goldenCases = []goldenCase{
	{name: "v", args: []string{"-rlptv"}},
	{name: "vv", args: []string{"-rlptvv"}},
	{name: "i", args: []string{"-rlpti"}},
	{name: "ii", args: []string{"-rlptii"}},
	{name: "vi", args: []string{"-rlptvi"}},
	{name: "stats", args: []string{"-rlpt", "--stats"}},
	{name: "v-stats", args: []string{"-rlptv", "--stats"}},
	{name: "h-stats", args: []string{"-rlptvh", "--stats"}},
	{name: "delete", args: []string{"-rlptv", "--delete"}, dest: extraneous},
	{name: "i-delete", args: []string{"-rlpti", "--delete"}, dest: extraneous},
	{name: "progress", args: []string{"-rlpt", "--progress"}},
	{name: "dry-run", args: []string{"-rlptvn"}},
	{name: "no-inc", args: []string{"-rlptv", "--no-inc-recursive"}},
	{name: "no-inc-i-delete", args: []string{"-rlpti", "--no-inc-recursive", "--delete"}, dest: extraneous},
	{name: "c-i", args: []string{"-rlptci"}},
	{name: "out-format", args: []string{"-rlpt", "--out-format=%o %i %n%L"}},
	{name: "q", args: []string{"-rlptq"}},
}

// goldenTree is the source: small, empty and larger files, a symlink, an
// empty directory and a name that needs escaping in output.
func goldenTree() tree {
	r := rand.New(rand.NewPCG(5, 5))
	return tree{
		"a":       {data: []byte("hello"), mode: 0o644},
		"d":       {dir: true, mode: 0o755},
		"d/b":     {data: []byte("world"), mode: 0o600},
		"d/e":     {dir: true, mode: 0o755},
		"f":       {dir: true, mode: 0o755},
		"f/big":   {data: randData(r, 100000), mode: 0o644},
		"f/empty": {data: nil, mode: 0o644},
		"l":       {link: "a"},
		"nl\nx":   {data: []byte("odd name"), mode: 0o644},
	}
}

// extraneous fills the destination with files the source lacks.
func extraneous(t *testing.T, dst string) {
	for _, p := range []string{"x/y/z", "x/gone", "zz"} {
		os.MkdirAll(filepath.Join(dst, filepath.Dir(p)), 0o755)
		os.WriteFile(filepath.Join(dst, p), []byte("extra"), 0o644)
	}
}

// changeSource makes the third run's changes: new contents of the same
// size, a new time, a file more and a file fewer.
func changeSource(t *testing.T, src string) {
	os.WriteFile(filepath.Join(src, "a"), []byte("HELLO"), 0o644)
	os.Chtimes(filepath.Join(src, "a"), epoch.Add(time.Hour), epoch.Add(time.Hour))
	big := filepath.Join(src, "f/big")
	data, _ := os.ReadFile(big)
	copy(data[50000:], "a change in the middle")
	os.WriteFile(big, data, 0o644)
	os.Chtimes(big, epoch.Add(2*time.Hour), epoch.Add(2*time.Hour))
	os.Chtimes(filepath.Join(src, "d/b"), epoch.Add(3*time.Hour), epoch.Add(3*time.Hour))
	os.WriteFile(filepath.Join(src, "d/new"), []byte("new"), 0o644)
	os.Chtimes(filepath.Join(src, "d/new"), epoch, epoch)
	os.Remove(filepath.Join(src, "f/empty"))
	for _, d := range []string{"d", "f", "."} {
		os.Chtimes(filepath.Join(src, d), epoch, epoch)
	}
}

// runGolden runs one case with the rsync at argv[0] (plus argv[1:]) in a
// fresh directory and returns the output of the three runs.
func runGolden(t *testing.T, argv []string, c goldenCase) [3]string {
	base := t.TempDir()
	src, dst := filepath.Join(base, "src"), filepath.Join(base, "dst")
	os.Mkdir(src, 0o755)
	goldenTree().write(t, src)
	os.Chtimes(src, epoch, epoch)
	if c.dest != nil {
		os.Mkdir(dst, 0o755)
		c.dest(t, dst)
	}
	var out [3]string
	for run := range out {
		if run == 2 {
			changeSource(t, src)
		}
		args := append(append(append([]string{}, argv[1:]...), c.args...), "src/", "dst/")
		cmd := exec.Command(argv[0], args...)
		cmd.Dir = base
		var b bytes.Buffer
		cmd.Stdout, cmd.Stderr = &b, &b
		err := cmd.Run()
		out[run] = b.String()
		if err != nil {
			out[run] += fmt.Sprintf("[exit: %v]\n", err)
		}
	}
	return out
}

var (
	reRate     = regexp.MustCompile(`[0-9.,]+[KMG]? bytes/sec`)
	reSpeedup  = regexp.MustCompile(`speedup is [0-9.,]+`)
	reFlTime   = regexp.MustCompile(`(File list (generation|transfer) time:) .*`)
	reProgRate = regexp.MustCompile(` +[0-9.]+[kMG]B/s +[0-9]+:[0-9]{2}:[0-9]{2}`)
	reBytes    = regexp.MustCompile(`(sent|received|Total bytes sent:|Total bytes received:|File list size:) ([0-9,.]+[KMG]?)`)
)

// normalize masks what depends on timing: rates and times, the
// intermediate progress updates and the byte counts, which it returns.
func normalize(s string) (string, []string) {
	var lines, counts []string
	for _, l := range strings.Split(s, "\n") {
		if i := strings.LastIndexByte(l, '\r'); i >= 0 {
			l = l[i+1:] // keep the final progress line only
		}
		l = reRate.ReplaceAllString(l, "N bytes/sec")
		l = reFlTime.ReplaceAllString(l, "$1 N")
		l = reProgRate.ReplaceAllString(l, " R T")
		l = reSpeedup.ReplaceAllString(l, "speedup is N")
		for _, m := range reBytes.FindAllStringSubmatch(l, -1) {
			counts = append(counts, m[2])
		}
		l = reBytes.ReplaceAllString(l, "$1 N")
		lines = append(lines, l)
	}
	return strings.Join(lines, "\n"), counts
}

// compareOutput reports how got differs from want, rsync's output for the
// same run, or "" if it does not.
func compareOutput(want, got string) string {
	w, wc := normalize(want)
	g, gc := normalize(got)
	if w != g {
		return lineDiff(w, g)
	}
	for i := range wc {
		a, b := parseCount(wc[i]), parseCount(gc[i])
		if d := a - b; d > 8+a/20 || -d > 8+a/20 {
			return fmt.Sprintf("byte counts differ beyond the tolerance: rsync %v, xfer %v", wc, gc)
		}
	}
	return ""
}

// parseCount reads a byte count as printed, with digit separators and
// (for -h) a unit suffix.
func parseCount(s string) int64 {
	mult := 1.0
	switch s[len(s)-1] {
	case 'K':
		mult = 1e3
	case 'M':
		mult = 1e6
	case 'G':
		mult = 1e9
	}
	v, _ := strconv.ParseFloat(strings.Trim(strings.ReplaceAll(s, ",", ""), "KMG"), 64)
	return int64(v * mult)
}

func goldenDir() string {
	if d := os.Getenv("XFER_GOLDEN_DIR"); d != "" {
		return d
	}
	return filepath.Join("testdata", "golden")
}

func TestGolden(t *testing.T) {
	if testing.Short() {
		t.Skip("golden tests are skipped with -short")
	}
	if os.Getenv("XFER_GOLDEN_UPDATE") != "" {
		ref := refRsync(t)
		os.MkdirAll(goldenDir(), 0o755)
		for _, c := range goldenCases {
			out := runGolden(t, ref.argv, c)
			for run, o := range out {
				p := filepath.Join(goldenDir(), fmt.Sprintf("%s.%d.txt", c.name, run+1))
				if err := os.WriteFile(p, []byte(o), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
		return
	}
	ours := xferPeer(t)
	for _, c := range goldenCases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			out := runGolden(t, ours.argv, c)
			for run, got := range out {
				p := filepath.Join(goldenDir(), fmt.Sprintf("%s.%d.txt", c.name, run+1))
				want, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				if d := compareOutput(string(want), got); d != "" {
					t.Errorf("run %d differs from rsync's output (%s):\n%s", run+1, p, d)
				}
			}
		})
	}
}

// TestGoldenLive compares our output with the reference rsync's for the
// archive options, in local transfers and over a remote shell in both
// directions. It needs a reference of protocol 33 (rsync 3.5.1 or later).
func TestGoldenLive(t *testing.T) {
	ref := refRsync(t)
	if ref.version < 33 || ref.name != "gnu" {
		t.Skipf("needs GNU rsync speaking protocol 33, have %s %d", ref.name, ref.version)
	}
	ours := xferPeer(t)
	// Run our server as one word, as rsync's is, so the remote command
	// lines have the same number of arguments.
	wrapper := filepath.Join(t.TempDir(), "xfer-server")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec "+ours.server+" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ours.server = wrapper
	ssh := fakeSSH(t)
	for _, c := range []goldenCase{
		{name: "av", args: []string{"-av"}},
		{name: "av-delete", args: []string{"-av", "--delete"}, dest: extraneous},
		{name: "avv", args: []string{"-avv"}},
		{name: "ai", args: []string{"-ai"}},
		{name: "a-stats", args: []string{"-a", "--stats"}},
		{name: "a-progress", args: []string{"-a", "--progress"}},
	} {
		for _, mode := range []string{"local", "push", "pull"} {
			t.Run(c.name+"/"+mode, func(t *testing.T) {
				t.Parallel()
				run := func(p *peer) [3]string {
					cc := c
					switch mode {
					case "push":
						cc.args = append(append([]string{}, c.args...), "-e", ssh, "--rsync-path="+p.server)
						return runRemote(t, p.argv, cc, true)
					case "pull":
						cc.args = append(append([]string{}, c.args...), "-e", ssh, "--rsync-path="+p.server)
						return runRemote(t, p.argv, cc, false)
					}
					return runGolden(t, p.argv, cc)
				}
				want, got := run(ref), run(ours)
				for i := range got {
					w := strings.ReplaceAll(want[i], ref.server, "RSYNC")
					g := strings.ReplaceAll(got[i], ours.server, "RSYNC")
					if d := compareOutput(hoistDeletes(maskBase(w)), hoistDeletes(maskBase(g))); d != "" {
						t.Errorf("run %d differs:\n%s", i+1, d)
					}
				}
			})
		}
	}
}

// hoistDeletes moves the deletion lines to the front. Where they fall
// among the names of transferred files depends, even with rsync, on how
// the processes are scheduled: a receiving client's names reach its
// output through its generator, which mentions deletions itself.
func hoistDeletes(s string) string {
	var del, rest []string
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, "deleting ") || strings.HasPrefix(l, "*deleting ") {
			del = append(del, l)
		} else {
			rest = append(rest, l)
		}
	}
	return strings.Join(append(del, rest...), "\n")
}

var reBase = regexp.MustCompile(`/[^ ]*/(src|dst)\b`)

// maskBase hides the temporary directory in remote paths.
func maskBase(s string) string { return reBase.ReplaceAllString(s, "T/$1") }

// runRemote is runGolden over the remote shell given in c.args.
func runRemote(t *testing.T, argv []string, c goldenCase, push bool) [3]string {
	base := t.TempDir()
	src, dst := filepath.Join(base, "src"), filepath.Join(base, "dst")
	os.Mkdir(src, 0o755)
	goldenTree().write(t, src)
	os.Chtimes(src, epoch, epoch)
	if c.dest != nil {
		os.Mkdir(dst, 0o755)
		c.dest(t, dst)
	}
	var out [3]string
	for run := range out {
		if run == 2 {
			changeSource(t, src)
		}
		args := append(append([]string{}, argv[1:]...), c.args...)
		if push {
			args = append(args, "src/", "host:"+dst+"/")
		} else {
			args = append(args, "host:"+src+"/", "dst/")
		}
		cmd := exec.Command(argv[0], args...)
		cmd.Dir = base
		var b bytes.Buffer
		cmd.Stdout, cmd.Stderr = &b, &b
		err := cmd.Run()
		out[run] = b.String()
		if err != nil {
			out[run] += fmt.Sprintf("[exit: %v]\n", err)
		}
	}
	return out
}

// lineDiff shows the lines that differ between rsync's output (-) and ours
// (+), with the common lines indented.
func lineDiff(want, got string) string {
	a, b := strings.Split(want, "\n"), strings.Split(got, "\n")
	// lcs[i][j] is the length of the longest common subsequence of a[i:]
	// and b[j:].
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out strings.Builder
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case i < len(a) && j < len(b) && a[i] == b[j]:
			fmt.Fprintf(&out, "  %q\n", a[i])
			i, j = i+1, j+1
		case j < len(b) && (i == len(a) || lcs[i][j+1] >= lcs[i+1][j]):
			fmt.Fprintf(&out, "+ %q\n", b[j])
			j++
		default:
			fmt.Fprintf(&out, "- %q\n", a[i])
			i++
		}
	}
	return out.String()
}
