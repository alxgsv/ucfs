package vfs

import "time"

// SetClock replaces the clock that variant TTLs are measured with.
func SetClock(f *FS, now func() time.Time) { f.now = now }
