// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

//go:build pprof

package main

import (
	"fmt"
	"os"
	"runtime/pprof"
)

// startProfile writes a CPU profile to $XFER_CPUPROFILE.<pid> when the
// binary is built with -tags pprof. Each process of a transfer (client and
// server) writes its own file.
func startProfile() func() {
	p := os.Getenv("XFER_CPUPROFILE")
	if p == "" {
		return func() {}
	}
	f, err := os.Create(fmt.Sprintf("%s.%d", p, os.Getpid()))
	if err != nil {
		return func() {}
	}
	pprof.StartCPUProfile(f)
	return func() {
		pprof.StopCPUProfile()
		f.Close()
		if mp := os.Getenv("XFER_MEMPROFILE"); mp != "" {
			if m, err := os.Create(fmt.Sprintf("%s.%d", mp, os.Getpid())); err == nil {
				pprof.Lookup("heap").WriteTo(m, 0)
				m.Close()
			}
		}
	}
}
