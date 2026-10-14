// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package token

// Interoperability tests against GNU rsync.
//
// Fixtures are GNU rsync batch files (testdata/*.batch). A batch file written
// by a receiving client is a verbatim copy of the (demultiplexed) byte stream
// the GNU sender transmitted, compressed token streams included, so it is a
// capture of real sender output. The source and basis files are generated
// deterministically by genInteropFiles and are not stored.
//
//   - TestInteropReplay (always runs) decodes every fixture's token streams
//     with our Reader and checks that the reconstructed files equal the
//     generated sources.
//   - TestInteropGNU (set TOKEN_DOCKER=1; needs docker and the rsynctest
//     image with GNU rsync) produces the batches with real rsync, replays
//     them as above, then re-encodes every file with our Writer, splices the
//     result into the batch and has GNU rsync apply it with --read-batch,
//     which checks the GNU receiver decodes our encoding. TOKEN_UPDATE=1
//     additionally rewrites the fixtures.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/parallelworks/xfer/rsync/internal/wire"
)

type interopCase struct {
	name  string
	kind  Kind
	proto int
	block int
	args  []string // compression options passed to rsync
	nb    [3]int   // basis block counts of files a, b, c
}

var interopCases = []interopCase{
	{"simple31", Simple, 31, 2000, nil, [3]int{60, 11, 13}},
	{"zlib31", Zlib, 31, 2000, []string{"--compress-choice=zlib"}, [3]int{60, 11, 13}},
	{"zlib31-l9", Zlib, 31, 2000, []string{"--compress-choice=zlib", "--compress-level=9"}, [3]int{60, 11, 13}},
	{"zlib30", Zlib, 30, 2000, []string{"-z"}, [3]int{60, 11, 13}},
	{"zlib29", Zlib, 29, 2000, []string{"-z"}, [3]int{60, 11, 13}},
	// Blocks > 0xFFFF: the proto < 31 history duplication quirk, and the
	// fixed behavior for comparison.
	{"zlib30-bigblock", Zlib, 30, 100000, []string{"-z"}, [3]int{12, 11, 13}},
	{"zlib31-bigblock", Zlib, 31, 100000, []string{"--compress-choice=zlib"}, [3]int{12, 11, 13}},
	{"zlibx31", Zlibx, 31, 2000, []string{"--compress-choice=zlibx"}, [3]int{60, 11, 13}},
	{"zstd31", Zstd, 31, 2000, []string{"--compress-choice=zstd"}, [3]int{60, 11, 13}},
	{"zstd31-l19", Zstd, 31, 2000, []string{"--compress-choice=zstd", "--compress-level=19"}, [3]int{60, 11, 13}},
	{"lz4", LZ4, 31, 2000, []string{"--compress-choice=lz4"}, [3]int{60, 11, 13}},
}

var interopNames = [3]string{"a.txt", "b.txt", "c.txt"}

type interopFile struct {
	name      string
	basis     []byte
	src       []byte
	count     int32
	remainder int32
}

func textData(rng *rand.Rand, n int) []byte {
	words := []string{"the ", "quick ", "brown ", "fox ", "jumps ", "over ", "lazy ", "dog ", "\n", "rsync ", "token "}
	b := make([]byte, 0, n+16)
	for len(b) < n {
		if rng.IntN(6) == 0 {
			b = fmt.Appendf(b, "%d ", rng.IntN(100000))
		} else {
			b = append(b, words[rng.IntN(len(words))]...)
		}
	}
	return b[:n]
}

// genInteropFiles deterministically builds the basis and source files.
func genInteropFiles(c interopCase) []interopFile {
	rng := rand.New(rand.NewPCG(42, uint64(c.block)))
	B := c.block
	var files []interopFile
	for i, nb := range c.nb {
		rem := 777 + 100*i
		basis := textData(rng, nb*B+rem)
		blk := func(a, b int) []byte { return basis[a*B : b*B] }
		var src []byte
		switch i {
		case 0:
			src = append(src, blk(0, 4)...)
			// A literal that repeats matched data (zlib history references).
			src = append(src, "inserted text that repeats: "...)
			src = append(src, basis[2*B+17:2*B+517]...)
			src = append(src, blk(4, 6)...)
			// A large incompressible literal (several records, > 32 KiB).
			rnd := make([]byte, 34000)
			for j := range rnd {
				rnd[j] = byte(rng.Uint32())
			}
			src = append(src, rnd...)
			src = append(src, blk(9, 10)...) // out of order: LONG tokens
			src = append(src, blk(6, 7)...)
			// Literals copying the end and the start of the previous block.
			src = append(src, basis[7*B-2000:7*B]...)
			src = append(src, basis[6*B:6*B+min(B, 40000)]...)
			src = append(src, basis[7*B+1:7*B+1+min(2*B-1, 20000)]...) // shifted: literal
			src = append(src, basis[10*B:]...)                         // run + remainder block
		case 1:
			src = append(src, textData(rng, 3000)...)
			src = append(src, basis[:5*B]...)
			src = append(src, basis[5*B+100:]...)
		default:
			src = append(src, basis...) // identical: one long run
		}
		files = append(files, interopFile{
			name:      interopNames[i],
			basis:     basis,
			src:       src,
			count:     int32(nb + 1),
			remainder: int32(rem),
		})
	}
	return files
}

// findSumHead finds the sum_head echo (count, blength, s2length, remainder)
// of f at or after off and returns the offset just past it.
func findSumHead(data []byte, off int, f interopFile, block int) int {
	var pat [8]byte
	binary.LittleEndian.PutUint32(pat[0:], uint32(f.count))
	binary.LittleEndian.PutUint32(pat[4:], uint32(block))
	for off < len(data) {
		i := bytes.Index(data[off:], pat[:])
		if i < 0 {
			return -1
		}
		p := off + i
		if p+16 <= len(data) && binary.LittleEndian.Uint32(data[p+12:]) == uint32(f.remainder) {
			return p + 16
		}
		off = p + 1
	}
	return -1
}

func basisBlocks(f interopFile, block int) [][]byte {
	var bl [][]byte
	for o := 0; o < len(f.basis); o += block {
		bl = append(bl, f.basis[o:min(o+block, len(f.basis))])
	}
	return bl
}

type streamSpan struct{ start, end int }

// replayBatch decodes all token streams of a batch file with our Reader,
// checks the reconstructed files and returns the decoded ops and the byte
// spans of each token stream.
func replayBatch(t *testing.T, c interopCase, data []byte, files []interopFile) ([][]op, []streamSpan) {
	t.Helper()
	br := bytes.NewReader(data)
	wr := wire.NewReader(br)
	tr := NewReader(c.kind, wr, c.proto)
	defer tr.Close()
	var allOps [][]op
	var spans []streamSpan
	pos := 0
	for _, f := range files {
		start := findSumHead(data, pos, f, c.block)
		if start < 0 {
			t.Fatalf("%s: sum_head of %s not found", c.name, f.name)
		}
		skip := make([]byte, start-pos)
		wr.ReadFull(skip)
		blocks := basisBlocks(f, c.block)
		var ops []op
		var out []byte
		for {
			lit, blk, done, err := tr.Next()
			if err != nil {
				t.Fatalf("%s/%s: %v", c.name, f.name, err)
			}
			if done {
				break
			}
			if lit != nil {
				ops = append(ops, op{lit: bytes.Clone(lit)})
				out = append(out, lit...)
				continue
			}
			if int(blk) >= len(blocks) {
				t.Fatalf("%s/%s: block %d out of range", c.name, f.name, blk)
			}
			tr.SeeMatch(blocks[blk])
			out = append(out, blocks[blk]...)
			ops = append(ops, op{block: blk})
		}
		if !bytes.Equal(out, f.src) {
			t.Fatalf("%s/%s: reconstructed %d bytes, want %d, first diff at %d", c.name, f.name, len(out), len(f.src), firstDiff(out, f.src))
		}
		pos = len(data) - br.Len()
		spans = append(spans, streamSpan{start, pos})
		allOps = append(allOps, ops)
	}
	return allOps, spans
}

func TestInteropReplay(t *testing.T) {
	for _, c := range interopCases {
		t.Run(c.name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", c.name+".batch"))
			if err != nil {
				t.Skipf("no fixture: %v", err)
			}
			files := genInteropFiles(c)
			allOps, _ := replayBatch(t, c, data, files)
			// Sanity: the fixtures must exercise both literals and matches.
			var nlit, nmatch int
			for _, ops := range allOps {
				for _, o := range ops {
					if o.lit != nil {
						nlit++
					} else {
						nmatch++
					}
				}
			}
			if nlit == 0 || nmatch == 0 {
				t.Fatalf("degenerate fixture: %d literals, %d matches", nlit, nmatch)
			}
		})
	}
}

func rsyncArgs(c interopCase) []string {
	a := []string{"-rI", "--no-whole-file", fmt.Sprintf("--protocol=%d", c.proto), fmt.Sprintf("-B%d", c.block)}
	if c.proto >= 30 {
		a = append(a, "--checksum-choice=md5")
	}
	return append(a, c.args...)
}

func shellQuote(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(q, " ")
}

func dockerRun(t *testing.T, dir, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("docker", "run", "--rm", "-v", dir+":/w", "rsynctest", "sh", "-e", "/w/run.sh")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker: %v\n%s", err, out)
	}
	t.Logf("docker output:\n%s", out)
}

func TestInteropGNU(t *testing.T) {
	if os.Getenv("TOKEN_DOCKER") == "" {
		t.Skip("set TOKEN_DOCKER=1 to run against GNU rsync in docker (image rsynctest)")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Step 1: GNU sender → batch file, over a remote-shell pull so that
	// compression is negotiated as for a real remote transfer.
	var sh strings.Builder
	sh.WriteString("printf '#!/bin/sh\\nshift\\nexec \"$@\"\\n' > /tmp/rsh; chmod +x /tmp/rsh\nrsync --version | head -1\n")
	for _, c := range interopCases {
		cd := filepath.Join(dir, c.name)
		for _, f := range genInteropFiles(c) {
			for sub, data := range map[string][]byte{"src": f.src, "basis": f.basis} {
				p := filepath.Join(cd, sub, f.name)
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, data, 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
		fmt.Fprintf(&sh, "cd /w/%s; cp -r basis dst; timeout 120 rsync %s -e /tmp/rsh --write-batch=gnu.batch localhost:/w/%s/src/ dst/; diff -r src dst; echo %s ok\n",
			c.name, shellQuote(rsyncArgs(c)), c.name, c.name)
	}
	dockerRun(t, dir, sh.String())

	// Step 2: decode the real streams; re-encode with our Writer.
	sh.Reset()
	for _, c := range interopCases {
		cd := filepath.Join(dir, c.name)
		data, err := os.ReadFile(filepath.Join(cd, "gnu.batch"))
		if err != nil {
			t.Fatal(err)
		}
		if os.Getenv("TOKEN_UPDATE") != "" {
			if err := os.WriteFile(filepath.Join("testdata", c.name+".batch"), data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		files := genInteropFiles(c)
		allOps, spans := replayBatch(t, c, data, files)
		t.Logf("%s: decoded GNU stream OK (%d bytes)", c.name, len(data))

		var nb bytes.Buffer
		w := wire.NewWriter(&nb)
		tw := NewWriter(c.kind, w, c.proto, 0)
		prev := 0
		for i, f := range files {
			w.Write(data[prev:spans[i].start])
			// Coalesce the literals so our chunking differs from GNU's.
			var merged []op
			for _, o := range allOps[i] {
				if o.lit != nil && len(merged) > 0 && merged[len(merged)-1].lit != nil {
					merged[len(merged)-1].lit = append(merged[len(merged)-1].lit, o.lit...)
					continue
				}
				merged = append(merged, o)
			}
			encode(t, tw, merged, basisBlocks(f, c.block))
			prev = spans[i].end
		}
		w.Write(data[prev:])
		if err := w.Flush(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cd, "ours.batch"), nb.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: GNU batch %d bytes, ours %d bytes", c.name, len(data), nb.Len())
		// Sanity: our own reader must accept our batch as well.
		replayBatch(t, c, nb.Bytes(), files)
		fmt.Fprintf(&sh, "cd /w/%s; cp -r basis dst2; timeout 120 rsync %s --read-batch=ours.batch dst2/; diff -r src dst2; echo %s read-batch ok\n",
			c.name, shellQuote(rsyncArgs(c)), c.name)
	}
	// Step 3: GNU receiver applies our encoding.
	dockerRun(t, dir, sh.String())
}
