package vfs

import "time"

// SetClock replaces the clock that variant TTLs are measured with.
func SetClock(f *FS, now func() time.Time) { f.now = now }

// CollideIDs makes the plain ids of the given node keys equal, as a 64-bit
// hash collision would, until the test ends.
func CollideIDs(t interface{ Cleanup(func()) }, keys ...string) {
	plain := hashID
	hashID = func(key string, salt uint32) uint64 {
		for _, k := range keys {
			if salt == 0 && key == k {
				return 42
			}
		}
		return plain(key, salt)
	}
	t.Cleanup(func() { hashID = plain })
}

// IDKey is the key a node's id hashes.
func IDKey(n Node) string { return n.idKey() }
