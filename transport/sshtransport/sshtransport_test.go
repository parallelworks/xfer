// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package sshtransport_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	mrand "math/rand/v2"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/parallelworks/xfer/rsync"
	"github.com/parallelworks/xfer/transport"
	"github.com/parallelworks/xfer/transport/sshtransport"
)

const (
	testUser     = "alice"
	testPassword = "secret"
)

// execFunc runs an exec request's command on ch and returns its exit status.
type execFunc func(ctx context.Context, cmd string, ch ssh.Channel) int

// serveInProcess handles commands the way an agent embedding xfer does:
// "rsync --server ..." runs rsync.Serve in this process.
func serveInProcess(ctx context.Context, cmd string, ch ssh.Channel) int {
	argv, err := transport.ParseShell(cmd)
	if err != nil || len(argv) < 2 || filepath.Base(argv[0]) != "rsync" || argv[1] != "--server" {
		fmt.Fprintf(ch.Stderr(), "sh: %s: command not found\n", cmd)
		return 127
	}
	err = rsync.Serve(ctx, argv[1:], ch, ch.Stderr())
	if err != nil {
		fmt.Fprintf(ch.Stderr(), "rsync: %v\n", err)
	}
	return rsync.ExitCode(err)
}

// serveSubprocess handles commands the way sshd does: through a shell, with
// path prepended to PATH.
func serveSubprocess(path string) execFunc {
	return func(ctx context.Context, cmd string, ch ssh.Channel) int {
		c := exec.CommandContext(ctx, "sh", "-c", cmd)
		c.Env = append(os.Environ(), "PATH="+path+string(os.PathListSeparator)+os.Getenv("PATH"))
		c.Stdin, c.Stdout, c.Stderr = ch, ch, ch.Stderr()
		c.WaitDelay = 5 * time.Second
		err := c.Run()
		var xe *exec.ExitError
		if errors.As(err, &xe) {
			return xe.ExitCode()
		}
		if err != nil {
			return 255
		}
		return 0
	}
}

type testServer struct {
	addr    string
	hostKey ssh.PublicKey
	userKey ssh.Signer
	active  atomic.Int32 // exec handlers running
}

func startServer(t *testing.T, run execFunc) *testServer {
	t.Helper()
	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	_, userPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	userSigner, err := ssh.NewSignerFromKey(userPriv)
	if err != nil {
		t.Fatal(err)
	}
	authorized := userSigner.PublicKey().Marshal()
	config := &ssh.ServerConfig{
		PasswordCallback: func(md ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if md.User() == testUser && string(pw) == testPassword {
				return nil, nil
			}
			return nil, errors.New("access denied")
		},
		PublicKeyCallback: func(md ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if md.User() == testUser && bytes.Equal(key.Marshal(), authorized) {
				return nil, nil
			}
			return nil, errors.New("unknown key")
		},
	}
	config.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		ln.Close()
		wg.Wait()
	})
	s := &testServer{addr: ln.Addr().String(), hostKey: hostSigner.PublicKey(), userKey: userSigner}
	wg.Go(func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Go(func() {
				context.AfterFunc(ctx, func() { nc.Close() })
				sc, chans, reqs, err := ssh.NewServerConn(nc, config)
				if err != nil {
					nc.Close()
					return
				}
				defer sc.Close()
				go ssh.DiscardRequests(reqs)
				for nch := range chans {
					if nch.ChannelType() != "session" {
						nch.Reject(ssh.UnknownChannelType, "only sessions")
						continue
					}
					ch, chReqs, err := nch.Accept()
					if err != nil {
						continue
					}
					wg.Go(func() { s.session(ctx, ch, chReqs, run) })
				}
			})
		}
	})
	return s
}

func (s *testServer) session(ctx context.Context, ch ssh.Channel, reqs <-chan *ssh.Request, run execFunc) {
	for req := range reqs {
		if req.Type != "exec" {
			req.Reply(false, nil)
			continue
		}
		var payload struct{ Command string }
		if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
			req.Reply(false, nil)
			continue
		}
		req.Reply(true, nil)
		s.active.Add(1)
		go func() {
			defer s.active.Add(-1)
			go ssh.DiscardRequests(reqs)
			status := run(ctx, payload.Command, ch)
			ch.CloseWrite()
			ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(status)}))
			ch.Close()
		}()
		return
	}
	ch.Close()
}

func (s *testServer) dial(t *testing.T, auth ssh.AuthMethod) *ssh.Client {
	t.Helper()
	c, err := ssh.Dial("tcp", s.addr, &ssh.ClientConfig{
		User:            testUser,
		Auth:            []ssh.AuthMethod{auth},
		HostKeyCallback: ssh.FixedHostKey(s.hostKey),
		Timeout:         10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func (s *testServer) client(t *testing.T) *ssh.Client {
	return s.dial(t, ssh.PublicKeys(s.userKey))
}

// run runs an rsync command line over tr.
func run(ctx context.Context, tr transport.Transport, args ...string) (*rsync.Stats, string, error) {
	o, operands, err := rsync.ParseArgs(args)
	if err != nil {
		return nil, "", err
	}
	var out bytes.Buffer
	c := &rsync.Client{Transport: tr, Stdout: &out, Stderr: &out}
	stats, err := c.Run(ctx, o, operands)
	return stats, out.String(), err
}

func newTransport(c *ssh.Client, stderr io.Writer) *sshtransport.Transport {
	tr := sshtransport.New(c)
	tr.Stderr = stderr
	return tr
}

var bigData = func() []byte {
	b := make([]byte, 1<<20+12345)
	mrand.NewChaCha8([32]byte{7}).Read(b)
	return b
}()

func writeTree(t *testing.T, root string) {
	t.Helper()
	old := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	files := map[string]struct {
		data string
		mode fs.FileMode
	}{
		"a.txt":         {"hello\n", 0o644},
		"exec.sh":       {"#!/bin/sh\necho hi\n", 0o755},
		"sub/b.txt":     {"in a subdir\n", 0o600},
		"sub/deep/c":    {"", 0o640},
		"name with sp":  {"spaces", 0o644},
		"sub/big.bin":   {string(bigData), 0o644},
		"sub/deep/d.go": {"package d\n", 0o444},
	}
	for name, f := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f.data), f.mode); err != nil {
			t.Fatal(err)
		}
		os.Chmod(p, f.mode)
		os.Chtimes(p, old, old)
	}
	if err := os.Symlink("sub/b.txt", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	os.Chmod(filepath.Join(root, "sub", "deep"), 0o750)
	for _, d := range []string{"sub/deep", "sub", "."} {
		os.Chtimes(filepath.Join(root, d), old.Add(time.Hour), old.Add(time.Hour))
	}
}

// compareTrees checks that dst has the same entries, contents, modes and
// modification times as src.
func compareTrees(t *testing.T, src, dst string) {
	t.Helper()
	snap := func(root string) map[string]string {
		m := map[string]string{}
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			fi, err := os.Lstat(p)
			if err != nil {
				return err
			}
			desc := fi.Mode().String()
			switch {
			case fi.Mode()&fs.ModeSymlink != 0:
				target, _ := os.Readlink(p)
				desc += " -> " + target
			case fi.Mode().IsRegular():
				data, _ := os.ReadFile(p)
				desc += fmt.Sprintf(" %d %x %d", fi.ModTime().Unix(), data[:min(len(data), 32)], len(data))
				if !bytes.Equal(data, mustRead(t, filepath.Join(src, rel))) {
					desc += " (content differs)"
				}
			default:
				desc += fmt.Sprintf(" %d", fi.ModTime().Unix())
			}
			m[rel] = desc
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	a, b := snap(src), snap(dst)
	for k, v := range a {
		if b[k] != v {
			t.Errorf("%s: source %q, destination %q", k, v, b[k])
		}
	}
	for k, v := range b {
		if _, ok := a[k]; !ok {
			t.Errorf("%s: extra in destination: %q", k, v)
		}
	}
}

func mustRead(t *testing.T, p string) []byte {
	b, err := os.ReadFile(p)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return b
}

func testPushPull(t *testing.T, c *ssh.Client) {
	ctx := context.Background()
	for _, dir := range []string{"push", "pull"} {
		t.Run(dir, func(t *testing.T) {
			base := t.TempDir()
			src, dst := filepath.Join(base, "src"), filepath.Join(base, "dst")
			writeTree(t, src)
			var stderr bytes.Buffer
			tr := newTransport(c, &stderr)
			from, to := src+"/", "host:"+dst+"/"
			if dir == "pull" {
				from, to = "host:"+src+"/", dst+"/"
			}
			if _, out, err := run(ctx, tr, "-a", from, to); err != nil {
				t.Fatalf("%s: %v\n%s%s", dir, err, out, stderr.String())
			}
			compareTrees(t, src, dst)

			// Change the middle of the big file, keeping its size, and
			// check that the update travels as a delta.
			big := filepath.Join(src, "sub", "big.bin")
			data := bytes.Clone(bigData)
			copy(data[len(data)/2:], "a small change in the middle")
			os.WriteFile(big, data, 0o644)
			mtime := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
			os.Chtimes(big, mtime, mtime)
			stats, out, err := run(ctx, tr, "-a", "--stats", from, to)
			if err != nil {
				t.Fatalf("%s update: %v\n%s%s", dir, err, out, stderr.String())
			}
			compareTrees(t, src, dst)
			if stats.NumTransferred != 1 || stats.MatchedData < int64(len(data))*9/10 || stats.LiteralData > 64<<10 {
				t.Errorf("update was not a small delta: %d files, matched %d, literal %d",
					stats.NumTransferred, stats.MatchedData, stats.LiteralData)
			}
		})
	}
}

func TestInProcessServer(t *testing.T) {
	s := startServer(t, serveInProcess)
	testPushPull(t, s.client(t))
}

func TestPasswordAuth(t *testing.T) {
	s := startServer(t, serveInProcess)
	c := s.dial(t, ssh.Password(testPassword))
	base := t.TempDir()
	src, dst := filepath.Join(base, "src"), filepath.Join(base, "dst")
	writeTree(t, src)
	if _, out, err := run(context.Background(), newTransport(c, io.Discard), "-a", "host:"+src+"/", dst); err != nil {
		t.Fatalf("pull: %v\n%s", err, out)
	}
	compareTrees(t, src, dst)
}

func TestSubprocessServer(t *testing.T) {
	if testing.Short() {
		t.Skip("builds cmd/xfer")
	}
	bin := t.TempDir()
	build := exec.Command("go", "build", "-o", filepath.Join(bin, "xfer"), "../../cmd/xfer")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building xfer: %v\n%s", err, out)
	}
	if err := os.Symlink("xfer", filepath.Join(bin, "rsync")); err != nil {
		t.Fatal(err)
	}
	s := startServer(t, serveSubprocess(bin))
	testPushPull(t, s.client(t))
}

func TestErrors(t *testing.T) {
	s := startServer(t, serveInProcess)
	c := s.client(t)
	ctx := context.Background()
	dst := t.TempDir()

	var stderr bytes.Buffer
	_, out, err := run(ctx, newTransport(c, &stderr), "-a", "host:/nonexistent/xfer-test-src/", dst+"/")
	if err == nil || rsync.ExitCode(err) == 0 {
		t.Errorf("missing source: err %v, want a non-zero exit", err)
	}
	if !strings.Contains(out+stderr.String(), "nonexistent") {
		t.Errorf("missing source: no explanation:\n%s%s", out, stderr.String())
	}

	stderr.Reset()
	_, _, err = run(ctx, newTransport(c, &stderr), "-a", "--rsync-path=no-such-rsync", "host:/tmp/", dst+"/")
	if code := rsync.ExitCode(err); code != 127 {
		t.Errorf("missing remote rsync: exit %d (%v), want 127", code, err)
	}
	if !strings.Contains(stderr.String(), "command not found") {
		t.Errorf("missing remote rsync: stderr %q", stderr.String())
	}
}

func TestCancel(t *testing.T) {
	s := startServer(t, serveInProcess)
	c := s.client(t)
	base := t.TempDir()
	src := filepath.Join(base, "src")
	os.Mkdir(src, 0o755)
	data := make([]byte, 16<<20)
	mrand.NewChaCha8([32]byte{9}).Read(data)
	os.WriteFile(filepath.Join(src, "f"), data, 0o644)

	for _, dir := range []string{"push", "pull"} {
		t.Run(dir, func(t *testing.T) {
			dst := filepath.Join(base, "dst-"+dir)
			from, to := src+"/", "host:"+dst+"/"
			if dir == "pull" {
				from, to = "host:"+src+"/", dst+"/"
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, _, err := run(ctx, newTransport(c, io.Discard), "-a", "--bwlimit=1m", from, to)
				done <- err
			}()
			deadline := time.Now().Add(10 * time.Second)
			for s.active.Load() == 0 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			time.Sleep(300 * time.Millisecond)
			cancel()
			start := time.Now()
			select {
			case err := <-done:
				if code := rsync.ExitCode(err); code != rsync.ExitSignal {
					t.Errorf("cancelled transfer: exit %d (%v), want %d", code, err, rsync.ExitSignal)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("transfer did not return after cancellation")
			}
			t.Logf("returned %v after cancel", time.Since(start))
			for s.active.Load() != 0 {
				if time.Since(start) > 10*time.Second {
					t.Fatal("remote session still running after cancellation")
				}
				time.Sleep(10 * time.Millisecond)
			}
			// The connection stays usable for the next command.
			if _, out, err := run(context.Background(), newTransport(c, io.Discard), "-a", "host:"+src+"/", filepath.Join(base, "after-"+dir)+"/"); err != nil {
				t.Fatalf("transfer after cancel: %v\n%s", err, out)
			}
		})
	}
}
