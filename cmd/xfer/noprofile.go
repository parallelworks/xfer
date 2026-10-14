// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

//go:build !pprof

package main

func startProfile() func() { return func() {} }
