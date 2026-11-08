package vfs

import (
	"context"
	"encoding/binary"
	"hash/fnv"
	"log/slog"
	"sync"

	"github.com/alxgsv/ucfs/internal/index"
)

// hashID is a node id: the 64-bit FNV-1a hash of the node's key, and of a
// salt after it when the plain hash collided. Tests replace it to force
// collisions.
var hashID = func(key string, salt uint32) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	if salt > 0 {
		var b [5]byte
		binary.BigEndian.PutUint32(b[1:], salt)
		_, _ = h.Write(b[:])
	}
	return h.Sum64()
}

// fingerprint tells apart keys whose ids collide: a second hash, FNV-1
// rather than FNV-1a, so two keys colliding in both is out of reach.
func fingerprint(key string) uint64 {
	h := fnv.New64()
	_, _ = h.Write([]byte(key))
	return h.Sum64()
}

// maxIDs bounds the ids remembered for collision checks. Past it they are
// forgotten, keeping the salted ones, and checks start over: a collision
// with a node handed out before then goes unseen, which at about 2^18
// nodes per period has a chance near 2·10^-9.
const maxIDs = 1 << 18

// idTable hands out node ids and detects collisions: an id is claimed by
// the key it was first handed out for, in this process or, for nodes known
// by path, in node_ops of the index. A key whose id is claimed by another
// gets a salted hash, and the salt goes to the index, so the key keeps its
// id across restarts and old NFS handles still resolve.
type idTable struct {
	mu    sync.Mutex
	ix    *index.Index
	owner map[uint64]uint64 // id → fingerprint of its key
	salts map[string]uint32
}

func (t *idTable) load(ix *index.Index) {
	t.ix, t.owner = ix, map[uint64]uint64{}
	salts, err := ix.NodeSalts(context.Background())
	if err != nil {
		slog.Warn("read node id salts", "err", err)
		salts = map[string]uint32{}
	}
	t.salts = salts
}

// ID is a node's stable 64-bit identity, used as inode number and NFS file
// id; no two nodes handed out get the same one.
func (f *FS) ID(n Node) uint64 {
	key := n.idKey()
	if key == "" {
		f.ids.claim(n.Ino)
		return n.Ino
	}
	return f.ids.id(key, n)
}

// claim records an object's inode number, which salting cannot move, so a
// hashed id that meets it is the one salted.
func (t *idTable) claim(ino uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.owner[ino]; !ok {
		t.rememberLocked(ino, 0)
	}
}

func (t *idTable) rememberLocked(id, fp uint64) {
	if len(t.owner) >= maxIDs {
		clear(t.owner)
	}
	t.owner[id] = fp
}

func (t *idTable) id(key string, n Node) uint64 {
	fp := fingerprint(key)
	t.mu.Lock()
	defer t.mu.Unlock()
	salt := t.salts[key]
	for start := salt; ; salt++ {
		id := hashID(key, salt)
		owner, taken := t.owner[id]
		if !taken && n.Kind.ByPath() {
			taken, owner = t.recordedLocked(id, n, fp)
		}
		if !taken || owner == fp {
			if salt != start {
				t.salts[key] = salt
				if err := t.ix.PutNodeSalt(context.Background(), key, salt); err != nil {
					slog.Warn("record node id salt", "err", err)
				}
				slog.Warn("node id collision; the node gets a salted id", "kind", n.Kind, "uuid", n.UUID, "ops", n.Ops,
					"id", hashID(key, start), "salted", id)
			}
			t.rememberLocked(id, fp)
			return id
		}
	}
}

// recordedLocked checks node_ops, where the ids of nodes known by path
// survive restarts: an id recorded for another path is taken.
func (t *idTable) recordedLocked(id uint64, n Node, fp uint64) (bool, uint64) {
	uuid, ops, err := t.ix.NodeOps(context.Background(), id)
	if err != nil || (uuid == n.UUID && ops == n.Ops) {
		return false, fp
	}
	return true, fingerprint(uuid + "\x00" + ops)
}
