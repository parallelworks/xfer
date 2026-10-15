// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package checksum

import (
	"crypto/md5"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"hash"

	"github.com/cespare/xxhash/v2"
	"github.com/zeebo/xxh3"
	//lint:ignore SA1019 the rsync protocol requires MD4 before protocol 30
	"golang.org/x/crypto/md4"
)

// Algorithm identifies a strong checksum algorithm.
type Algorithm int

const (
	None Algorithm = iota
	// MD4Old is the protocol 27-29 MD4 variant whose whole-file sum is
	// seeded with the checksum seed.
	MD4Old
	MD4
	MD5
	SHA1
	XXH64
	XXH3
	XXH128
)

var names = map[Algorithm]string{
	None: "none", MD4Old: "md4", MD4: "md4", MD5: "md5", SHA1: "sha1",
	XXH64: "xxh64", XXH3: "xxh3", XXH128: "xxh128",
}

func (a Algorithm) String() string { return names[a] }

// Preference lists the algorithms we offer in negotiation, strongest and
// fastest first, using their protocol names.
var Preference = []string{"xxh128", "xxh3", "xxh64", "xxhash", "md5", "md4", "sha1", "none"}

// Parse maps a protocol name to an Algorithm.
func Parse(name string) (Algorithm, error) {
	switch name {
	case "xxh128":
		return XXH128, nil
	case "xxh3":
		return XXH3, nil
	case "xxh64", "xxhash":
		return XXH64, nil
	case "md5":
		return MD5, nil
	case "md4":
		return MD4, nil
	case "sha1":
		return SHA1, nil
	case "none":
		return None, nil
	}
	return 0, fmt.Errorf("unknown checksum name: %s", name)
}

// Size returns the digest length in bytes.
func (a Algorithm) Size() int {
	switch a {
	case None:
		return 1
	case MD4Old, MD4, MD5, XXH128:
		return 16
	case SHA1:
		return 20
	case XXH64, XXH3:
		return 8
	}
	panic("checksum: unknown algorithm")
}

// Block computes the seeded strong checksums of transfer blocks.
type Block struct {
	alg  Algorithm
	seed int32
	// seedFirst places an MD5 seed before the data instead of after it
	// (rsync's CF_CHKSUM_SEED_FIX).
	seedFirst bool
	h         hash.Hash
	x64       *xxhash.Digest
	seedBuf   [4]byte
}

// NewBlock returns a block checksummer.
func NewBlock(alg Algorithm, seed int32, seedFirst bool) *Block {
	b := &Block{alg: alg, seed: seed, seedFirst: seedFirst}
	binary.LittleEndian.PutUint32(b.seedBuf[:], uint32(seed))
	switch alg {
	case MD4Old, MD4:
		b.h = md4.New()
	case MD5:
		b.h = md5.New()
	case SHA1:
		b.h = sha1.New()
	}
	return b
}

// Size returns the full digest length.
func (b *Block) Size() int { return b.alg.Size() }

// Sum appends the checksum of p to dst.
func (b *Block) Sum(dst, p []byte) []byte {
	seed64 := uint64(int64(b.seed))
	switch b.alg {
	case None:
		return append(dst, 0)
	case XXH64:
		if b.x64 == nil {
			b.x64 = xxhash.NewWithSeed(seed64)
		} else {
			b.x64.ResetWithSeed(seed64)
		}
		b.x64.Write(p)
		return binary.LittleEndian.AppendUint64(dst, b.x64.Sum64())
	case XXH3:
		return binary.LittleEndian.AppendUint64(dst, xxh3.HashSeed(p, seed64))
	case XXH128:
		v := xxh3.Hash128Seed(p, seed64)
		dst = binary.LittleEndian.AppendUint64(dst, v.Lo)
		return binary.LittleEndian.AppendUint64(dst, v.Hi)
	case MD4Old, MD4:
		b.h.Reset()
		b.h.Write(p)
		if b.seed != 0 {
			b.h.Write(b.seedBuf[:])
		}
		return b.h.Sum(dst)
	default: // MD5, SHA1
		b.h.Reset()
		if b.seed != 0 && b.seedFirst {
			b.h.Write(b.seedBuf[:])
		}
		b.h.Write(p)
		if b.seed != 0 && !b.seedFirst {
			b.h.Write(b.seedBuf[:])
		}
		return b.h.Sum(dst)
	}
}

// NewFile returns a hash for whole-file checksums: the checksum sent after
// each file's data, and the per-file checksum of --checksum file lists.
func NewFile(alg Algorithm, seed int32) hash.Hash {
	switch alg {
	case None:
		return &noneHash{}
	case MD4Old:
		h := &seededHash{Hash: md4.New()}
		binary.LittleEndian.PutUint32(h.seed[:], uint32(seed))
		h.Reset()
		return h
	case MD4:
		return md4.New()
	case MD5:
		return md5.New()
	case SHA1:
		return sha1.New()
	case XXH64:
		return be64{xxhash.New()}
	case XXH3:
		return xxh3Hash{xxh3.New()}
	case XXH128:
		return xxh128Hash{xxh3.New128()}
	}
	panic("checksum: unknown algorithm")
}

// seededHash is a hash whose input always starts with the 4-byte seed,
// including after Reset.
type seededHash struct {
	hash.Hash
	seed [4]byte
}

func (h *seededHash) Reset() {
	h.Hash.Reset()
	h.Hash.Write(h.seed[:])
}

// rsync stores 64-bit xxhash digests little-endian, while the Go packages'
// Sum methods append big-endian; these wrappers fix the byte order.

type be64 struct{ *xxhash.Digest }

func (h be64) Sum(b []byte) []byte { return binary.LittleEndian.AppendUint64(b, h.Sum64()) }

type xxh3Hash struct{ *xxh3.Hasher }

func (h xxh3Hash) Sum(b []byte) []byte { return binary.LittleEndian.AppendUint64(b, h.Sum64()) }

type xxh128Hash struct{ *xxh3.Hasher128 }

func (h xxh128Hash) Sum(b []byte) []byte {
	v := h.Sum128()
	b = binary.LittleEndian.AppendUint64(b, v.Lo)
	return binary.LittleEndian.AppendUint64(b, v.Hi)
}

func (h xxh128Hash) Size() int { return 16 }

type noneHash struct{}

func (*noneHash) Write(p []byte) (int, error) { return len(p), nil }
func (*noneHash) Sum(b []byte) []byte         { return append(b, 0) }
func (*noneHash) Reset()                      {}
func (*noneHash) Size() int                   { return 1 }
func (*noneHash) BlockSize() int              { return 1 }
