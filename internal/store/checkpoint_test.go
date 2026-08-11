package store_test

import (
	"bytes"
	"context"
	"testing"

	"altv1/internal/store"
)

func TestCheckpointPointerCanBeDeletedWithoutLosingExactVersions(t *testing.T) {
	ctx := context.Background()
	ledger, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()

	key := "peer:collaboration-7"
	first := []byte{0, 1, 2, 3, 255}
	second := []byte("new exact runner state")
	if err := ledger.Set(ctx, key, first); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Set(ctx, key, second); err != nil {
		t.Fatal(err)
	}
	current, found, err := ledger.Get(ctx, key)
	if err != nil || !found || !bytes.Equal(current, second) {
		t.Fatalf("current checkpoint = (%v, %v, %v)", current, found, err)
	}
	if err := ledger.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, found, err := ledger.Get(ctx, key); err != nil || found {
		t.Fatalf("deleted checkpoint pointer = (%v, %v)", found, err)
	}
	versions, err := ledger.CheckpointVersions(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 2 || versions[0].Version != 1 || versions[1].Version != 2 {
		t.Fatalf("checkpoint versions = %#v", versions)
	}
	if !bytes.Equal(versions[0].Value, first) || !bytes.Equal(versions[1].Value, second) {
		t.Fatal("append-only checkpoint archive changed exact bytes")
	}
	if versions[0].Digest == versions[1].Digest || versions[0].Digest == "" {
		t.Fatal("checkpoint versions do not carry useful content digests")
	}
}
