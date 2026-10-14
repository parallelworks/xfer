// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package transport

import "os"

// GrowPipe enlarges the kernel buffer of a pipe where the system allows it;
// elsewhere it does nothing.
func GrowPipe(*os.File) {}
