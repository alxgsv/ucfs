//go:build race

package cache

// raceEnabled says the race detector is on, which slows the CPU-bound fake
// the timing tests compete with.
const raceEnabled = true
