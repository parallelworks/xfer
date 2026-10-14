// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

#include "textflag.h"

// Byte weights 32, 31, ..., 1 for one 32-byte chunk.
DATA weights<>+0(SB)/8, $0x191a1b1c1d1e1f20
DATA weights<>+8(SB)/8, $0x1112131415161718
DATA weights<>+16(SB)/8, $0x090a0b0c0d0e0f10
DATA weights<>+24(SB)/8, $0x0102030405060708
GLOBL weights<>(SB), RODATA|NOPTR, $32

DATA ones8<>+0(SB)/8, $0x0101010101010101
DATA ones8<>+8(SB)/8, $0x0101010101010101
DATA ones8<>+16(SB)/8, $0x0101010101010101
DATA ones8<>+24(SB)/8, $0x0101010101010101
GLOBL ones8<>(SB), RODATA|NOPTR, $32

DATA ones16<>+0(SB)/8, $0x0001000100010001
DATA ones16<>+8(SB)/8, $0x0001000100010001
DATA ones16<>+16(SB)/8, $0x0001000100010001
DATA ones16<>+24(SB)/8, $0x0001000100010001
GLOBL ones16<>(SB), RODATA|NOPTR, $32

// Horizontal sum of the eight dwords of Y into R, using X5 as scratch.
#define HSUM(Y, X, R) \
	VEXTRACTI128 $1, Y, X5 \
	VPADDD X5, X, X        \
	VPSHUFD $0x4e, X, X5   \
	VPADDD X5, X, X        \
	VPSHUFD $0xb1, X, X5   \
	VPADDD X5, X, X        \
	VMOVD X, R

// func rollingBlocks(p []byte) (s1, pre, w uint32)
TEXT ·rollingBlocks(SB), NOSPLIT, $0-36
	MOVQ p_base+0(FP), SI
	MOVQ p_len+8(FP), CX
	SHRQ $5, CX
	VMOVDQU weights<>(SB), Y10
	VMOVDQU ones8<>(SB), Y11
	VMOVDQU ones16<>(SB), Y12
	VPXOR Y0, Y0, Y0 // per-lane s1
	VPXOR Y1, Y1, Y1 // per-lane sum of s1 before each chunk
	VPXOR Y2, Y2, Y2 // per-lane weighted sums

loop:
	TESTQ CX, CX
	JZ    done
	VMOVDQU (SI), Y3
	VPADDD  Y0, Y1, Y1
	VPMADDUBSW Y3, Y11, Y4 // ones (unsigned) x data (signed), pairwise
	VPMADDWD   Y12, Y4, Y4
	VPADDD     Y4, Y0, Y0
	VPMADDUBSW Y3, Y10, Y6 // weights x data
	VPMADDWD   Y12, Y6, Y6
	VPADDD     Y6, Y2, Y2
	ADDQ $32, SI
	DECQ CX
	JMP  loop

done:
	HSUM(Y0, X0, AX)
	HSUM(Y1, X1, BX)
	HSUM(Y2, X2, DX)
	VZEROUPPER
	MOVL AX, s1+24(FP)
	MOVL BX, pre+28(FP)
	MOVL DX, w+32(FP)
	RET
