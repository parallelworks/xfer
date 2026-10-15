// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package rsync_test

import (
	"bytes"
	"context"
	"crypto/fips140"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/parallelworks/xfer/rsync"
)

// runMain runs rsync in-process and returns its exit code and output.
func runMain(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := rsync.Main(context.Background(), args, rsync.Stdio{Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr})
	return code, stdout.String(), stderr.String()
}

// TestLocal runs the interop scenarios as local transfers, where both
// halves of the protocol run in this process. It needs no other rsync.
func TestLocal(t *testing.T) {
	for _, sc := range scenarios {
		if sc.name == "protect-args" || sc.name == "files-from-remote" {
			continue // meaningless without a remote shell
		}
		if sc.name == "protocol-29" && fips140.Enabled() {
			continue // refused by design in FIPS mode; TestFIPS covers it
		}
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			base := t.TempDir()
			src, dst := filepath.Join(base, "src"), filepath.Join(base, "dst")
			os.Mkdir(src, 0o755)
			os.Mkdir(dst, 0o755)
			sourceTree(2).write(t, src)
			if sc.prepare != nil {
				sc.prepare(t, src, dst)
			}
			args := append(expandOpts(sc.opts, base), src+"/", dst+"/")
			code, stdout, stderr := runMain(t, args...)
			if code != 0 {
				t.Fatalf("rsync %s: exit %d\n%s%s", strings.Join(args, " "), code, stdout, stderr)
			}
			if sc.check != nil {
				sc.check(t, src, dst, strings.ReplaceAll(stdout, "<f", ">f"))
				return
			}
			if d := diffSnapshots(snapshot(t, src, sc.times, sc.perms), snapshot(t, dst, sc.times, sc.perms)); d != "" {
				t.Errorf("destination differs:\n%s", d)
			}
			// A second run must find nothing to do.
			code, stdout, _ = runMain(t, append([]string{"-i"}, args...)...)
			if code != 0 || (strings.Contains(stdout, ">f") && !slicesContainsAny(sc.opts, "-c", "-rlc", "-I", "--inplace")) {
				t.Errorf("second run not idempotent (exit %d):\n%s", code, stdout)
			}
		})
	}
}

func slicesContainsAny(opts []string, want ...string) bool {
	for _, o := range opts {
		for _, w := range want {
			if o == w {
				return true
			}
		}
	}
	return false
}

// TestFIPS checks the FIPS policy: only xxHash checksums are negotiated,
// and peers that cannot negotiate are refused cleanly. CI also runs the
// whole suite with GODEBUG=fips140=only, where MD4/MD5 would panic.
func TestFIPS(t *testing.T) {
	base := t.TempDir()
	src, dst := filepath.Join(base, "src"), filepath.Join(base, "dst")
	sourceTree(3).write(t, src)

	code, out, errOut := runMain(t, "-a", "--fips", "--no-W", "-c", src+"/", dst+"/")
	if code != 0 {
		t.Fatalf("FIPS transfer failed: %d\n%s%s", code, out, errOut)
	}
	if d := diffSnapshots(snapshot(t, src, true, false), snapshot(t, dst, true, false)); d != "" {
		t.Errorf("destination differs:\n%s", d)
	}

	code, _, errOut = runMain(t, "-a", "--fips", "--protocol=29", src+"/", filepath.Join(base, "old")+"/")
	if code != rsync.ExitUnsupported || !strings.Contains(errOut, "FIPS mode") {
		t.Errorf("protocol 29 in FIPS mode: exit %d, stderr %q; want a FIPS refusal", code, errOut)
	}
	code, _, errOut = runMain(t, "-a", "--fips", "--checksum-choice=md5", src+"/", filepath.Join(base, "md5")+"/")
	if code != rsync.ExitUnsupported || !strings.Contains(errOut, "not allowed") {
		t.Errorf("md5 in FIPS mode: exit %d, stderr %q", code, errOut)
	}
	if fips140.Enabled() {
		t.Log("running in Go FIPS 140 mode")
	}
}

// TestPartial interrupts a transfer and checks that --partial and
// --partial-dir keep the received data and that the next run uses it.
func TestPartial(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	data := make([]byte, 24<<20)
	rand.NewChaCha8([32]byte{2}).Read(data)
	for _, mode := range []string{"--partial", "--partial-dir=.part"} {
		t.Run(mode, func(t *testing.T) {
			base := t.TempDir()
			src, dst := filepath.Join(base, "src"), filepath.Join(base, "dst")
			os.Mkdir(src, 0o755)
			os.WriteFile(filepath.Join(src, "f"), data, 0o644)
			ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
			defer cancel()
			var stderr bytes.Buffer
			code := rsync.Main(ctx, []string{"-a", "--no-W", mode, "--bwlimit=8m", src + "/", dst + "/"},
				rsync.Stdio{Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: &stderr})
			if code != rsync.ExitSignal {
				t.Fatalf("interrupted run: exit %d, want %d\n%s", code, rsync.ExitSignal, stderr.String())
			}
			kept := filepath.Join(dst, "f")
			if mode != "--partial" {
				kept = filepath.Join(dst, ".part", "f")
			}
			fi, err := os.Stat(kept)
			if err != nil || fi.Size() == 0 || fi.Size() >= int64(len(data)) {
				t.Fatalf("partial file %s: %v, %v", kept, fi, err)
			}
			code, stdout, stderrS := runMain(t, "-a", "--no-W", "--stats", mode, src+"/", dst+"/")
			if code != 0 {
				t.Fatalf("resume: exit %d\n%s", code, stderrS)
			}
			if !strings.Contains(stdout, "Matched data: ") || strings.Contains(stdout, "Matched data: 0 bytes") {
				t.Errorf("resume did not use the partial file:\n%s", stdout)
			}
			if got, _ := os.ReadFile(filepath.Join(dst, "f")); !bytes.Equal(got, data) {
				t.Error("resumed file differs")
			}
			if _, err := os.Stat(filepath.Join(dst, ".part")); err == nil {
				t.Error("partial dir left behind")
			}
		})
	}
}

// TestTimeout runs against a remote shell whose server never answers.
func TestTimeout(t *testing.T) {
	dir := t.TempDir()
	shell := filepath.Join(dir, "hang")
	os.WriteFile(shell, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755)
	start := time.Now()
	code, _, stderr := runMain(t, "-a", "--timeout=1", "-e", shell, dir+"/", "host:"+dir+"/dst/")
	if code != 30 || !strings.Contains(stderr, "io timeout after 1 seconds") {
		t.Errorf("exit %d, stderr:\n%s", code, stderr)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %v", d)
	}
}

// TestConTimeout runs against a remote shell that never starts rsync.
func TestConTimeout(t *testing.T) {
	dir := t.TempDir()
	shell := filepath.Join(dir, "hang")
	os.WriteFile(shell, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755)
	code, _, stderr := runMain(t, "-a", "--contimeout=1", "-e", shell, dir+"/", "host:"+dir+"/dst/")
	if code != 35 {
		t.Errorf("exit %d, stderr:\n%s", code, stderr)
	}
}
