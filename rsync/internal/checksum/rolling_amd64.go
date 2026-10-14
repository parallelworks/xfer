// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package checksum

import "golang.org/x/sys/cpu"

var useAVX2 = cpu.X86.HasAVX2

// rollingVec returns how many leading bytes of p rollingBlocks handles.
func rollingVec(p []byte) int {
	if !useAVX2 || len(p) < 64 {
		return 0
	}
	return len(p) &^ 31
}

// rollingBlocks sums p, whose length is a multiple of 32, in 32-byte
// chunks c. It returns s1 = the sum of all bytes, pre = the sum over chunks
// of s1 before the chunk, and w = the sum over chunks of the bytes weighted
// 32 down to 1; the caller derives s2 = 32*pre + w. Bytes are signed.
//
//go:noescape
func rollingBlocks(p []byte) (s1, pre, w uint32)
