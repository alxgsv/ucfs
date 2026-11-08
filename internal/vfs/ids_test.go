package vfs_test

import (
	"context"
	"testing"

	"github.com/alxgsv/ucfs/internal/cdn"
	"github.com/alxgsv/ucfs/internal/vfs"
	"github.com/alxgsv/ucfs/internal/vfs/vfstest"
)

// Two files whose plain ids collide get different ids: the one handed out
// first keeps the plain id, the other a salted one recorded in the index,
// which a remount keeps, so its NFS handles still resolve.
func TestIDCollisionIsSalted(t *testing.T) {
	env := vfstest.New(t, file(1, "a.jpg", 10), file(2, "b.jpg", 10))
	a, b := vfs.Node{Kind: vfs.KindFile, UUID: uuid(1)}, vfs.Node{Kind: vfs.KindFile, UUID: uuid(2)}
	vfs.CollideIDs(t, vfs.IDKey(a), vfs.IDKey(b))
	ctx := context.Background()
	fs1 := vfs.New(env.Index, env.Cache, env.CDN, nil)

	entries, err := fs1.ReadDir(ctx, walk(t, env, "files").Node)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[uint64]string{}
	for _, e := range entries {
		id := fs1.ID(e.Node)
		if other, dup := ids[id]; dup {
			t.Fatalf("%s and %s share id %d", other, e.Name, id)
		}
		ids[id] = e.Name
	}
	idA, idB := fs1.ID(a), fs1.ID(b)
	if idA != 42 || idB == 42 || fs1.ID(b) != idB {
		t.Fatalf("ids %d, %d: want the first plain (42) and the second salted and stable", idA, idB)
	}
	if n, err := fs1.NodeByID(ctx, vfs.KindFile, uuid(2), idB); err != nil || n != b {
		t.Fatalf("salted id resolves to %+v, %v", n, err)
	}

	// A new filesystem on the same index, as after a restart, hands b out
	// first and still gives it the salted id; a keeps the plain one.
	fs2 := vfs.New(env.Index, env.Cache, env.CDN, nil)
	if got := fs2.ID(b); got != idB {
		t.Fatalf("after a restart b has id %d, want %d", got, idB)
	}
	if got := fs2.ID(a); got != idA {
		t.Fatalf("after a restart a has id %d, want %d", got, idA)
	}
}

// A path recorded in node_ops for an id, in an earlier run, keeps the id:
// another path whose plain id collides with it is salted although nothing
// was handed out in this run.
func TestIDCollisionWithRecordedPath(t *testing.T) {
	env := vfstest.New(t, hero())
	x := vfs.Node{Kind: vfs.KindVariantDir, UUID: uuid(1), Ops: "-/resize/10x/"}
	y := vfs.Node{Kind: vfs.KindVariantDir, UUID: uuid(1), Ops: "-/resize/20x/"}
	vfs.CollideIDs(t, vfs.IDKey(x), vfs.IDKey(y))
	ctx := context.Background()
	env.FS.Remember(ctx, x)

	fs2 := vfs.New(env.Index, env.Cache, cdn.New(env.Fake.CDNBase(), nil, nil), nil)
	idY := fs2.ID(y)
	if idY == 42 {
		t.Fatal("a path got the id node_ops records for another")
	}
	fs2.Remember(ctx, y)
	for _, n := range []vfs.Node{x, y} {
		got, err := fs2.NodeByID(ctx, n.Kind, n.UUID, fs2.ID(n))
		if err != nil || got != n {
			t.Fatalf("%s resolves to %+v, %v", n.Ops, got, err)
		}
	}
}
