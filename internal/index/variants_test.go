package index

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestVariantsAndNodeOps(t *testing.T) {
	ctx := context.Background()
	_, _, ix := setup(t)
	const uuid, ops = "00000000-0000-0000-0000-000000000001", "-/resize/100x/-/format/webp/"

	if _, err := ix.Variant(ctx, uuid, ops); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing variant: err = %v", err)
	}
	v := Variant{UUID: uuid, Ops: ops, Size: 1234, MIME: "image/webp", ETag: `W/"x"`,
		CheckedAt: t0.Add(time.Hour), ChangedAt: t0}
	if err := ix.PutVariant(ctx, v); err != nil {
		t.Fatal(err)
	}
	v.CheckedAt = t0.Add(2 * time.Hour)
	if err := ix.PutVariant(ctx, v); err != nil {
		t.Fatal(err)
	}
	got, err := ix.Variant(ctx, uuid, ops)
	if err != nil || got.Size != v.Size || got.MIME != v.MIME || got.ETag != v.ETag ||
		!got.CheckedAt.Equal(v.CheckedAt) || !got.ChangedAt.Equal(v.ChangedAt) {
		t.Errorf("variant = %+v, %v; want %+v", got, err, v)
	}

	// Ids are full 64-bit hashes; the top bit must survive SQLite's signed integers.
	const id = uint64(0xfedcba9876543210)
	if _, _, err := ix.NodeOps(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing node: err = %v", err)
	}
	for range 2 {
		if err := ix.PutNodeOps(ctx, id, uuid, ops); err != nil {
			t.Fatal(err)
		}
	}
	if u, o, err := ix.NodeOps(ctx, id); err != nil || u != uuid || o != ops {
		t.Errorf("node ops = %q %q %v", u, o, err)
	}
}
