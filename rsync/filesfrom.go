// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package rsync

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"strings"
)

// --files-from names the files to transfer in a list, relative to the one
// source directory. The list is read by the side that has it and, when
// that is not the sender, forwarded to the sender over the connection: each
// name ends in a NUL byte and an empty name ends the list.

// filesFromRemote splits a --files-from argument naming a file on the
// remote host ("host:path" or ":path") into its path.
func filesFromRemote(spec string) (path string, ok bool) {
	if spec == "-" {
		return "", false
	}
	loc, err := parseLocation(spec)
	if err != nil || (loc.host == "" && !strings.HasPrefix(spec, ":")) {
		return "", false
	}
	if loc.host == "" {
		return spec[1:], true
	}
	return loc.path, true
}

// readFilesFrom reads a list of names from a file: one per line, or
// NUL-terminated with --from0. Empty names are skipped, and so are names
// starting with '#' or ';', as comments. (A list forwarded over the
// connection has no comments: every name in it is taken as is.)
func readFilesFrom(r io.Reader, from0 bool) ([]string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 64<<10)
	sc.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		seps := "\n\r"
		if from0 {
			seps = "\x00"
		}
		if i := bytes.IndexAny(data, seps); i >= 0 {
			return i + 1, data[:i], nil
		}
		if atEOF && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	var names []string
	for sc.Scan() {
		name := sc.Text()
		if name == "" || name[0] == '#' || name[0] == ';' {
			continue
		}
		names = append(names, name)
	}
	return names, sc.Err()
}

// openFilesFrom reads the list named by a local --files-from argument.
func openFilesFrom(spec string, stdin io.Reader, from0 bool) ([]string, error) {
	if spec == "-" {
		if stdin == nil {
			stdin = os.Stdin
		}
		return readFilesFrom(stdin, from0)
	}
	f, err := os.Open(spec)
	if err != nil {
		return nil, exitErrorf(ExitFileSelect, "failed to open files-from file %s: %s", spec, errnoText(err))
	}
	defer f.Close()
	return readFilesFrom(f, from0)
}

// forwardNames sends a list to the peer.
func (s *session) forwardNames(names []string) error {
	w := s.out
	w.Lock()
	defer w.Unlock()
	for _, n := range names {
		w.W.Write([]byte(n))
		w.W.Byte(0)
	}
	w.W.Byte(0)
	return w.W.Flush()
}

// recvNames reads a list the peer forwards.
func (s *session) recvNames() ([]string, error) {
	var names []string
	var b []byte
	for {
		c := s.in.Byte()
		if err := s.in.Err(); err != nil {
			return nil, err
		}
		if c != 0 {
			b = append(b, c)
			continue
		}
		if len(b) == 0 {
			return names, nil
		}
		names = append(names, string(b))
		b = b[:0]
	}
}

// filesFromPaths turns listed names into scanner arguments below the
// source directory dir. The "/./" marks where the transferred path starts,
// as with --relative.
func filesFromPaths(dir string, names []string) []string {
	if dir == "" {
		dir = "."
	}
	dir = strings.TrimSuffix(dir, "/")
	paths := make([]string, 0, len(names))
	for _, n := range names {
		n = strings.TrimLeft(n, "/")
		if n == "" {
			n = "."
		}
		paths = append(paths, dir+"/./"+n)
	}
	return paths
}
