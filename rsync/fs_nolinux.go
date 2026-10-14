// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package rsync

import (
	"errors"
	"os"
	"time"
)

// futimes is not available here without losing precision; callers fall
// back to setting the time by name.
func futimes(*os.File, time.Time) error { return errors.ErrUnsupported }
