// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

//go:build !amd64

package checksum

func rollingVec([]byte) int { return 0 }

func rollingBlocks([]byte) (s1, pre, w uint32) { panic("unreachable") }
