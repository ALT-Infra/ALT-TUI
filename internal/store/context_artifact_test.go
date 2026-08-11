package store_test

import (
	"context"
	"strings"
	"testing"

	"altv1/internal/store"
)

func TestExactToolArtifactsAreSearchableImmutableAndOwnerScoped(t *testing.T) {
	ctx := context.Background()
	ledger, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	document := mustProfile(t)
	if err := ledger.ImportProfile(ctx, document); err != nil {
		t.Fatal(err)
	}
	session, err := ledger.CreateSession(ctx, document, "index offloaded evidence", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	refA := "alt-tool-output://" + strings.Repeat("a", 64)
	refB := "alt-tool-output://" + strings.Repeat("b", 64)
	for _, artifact := range []store.ContextArtifact{
		{Reference: refA, SessionID: session.ID, Owner: "member:alpha:d1", Content: []byte("QK-4417 exact alpha evidence")},
		{Reference: refB, SessionID: session.ID, Owner: "member:beta:d2", Content: []byte("QK-4417 exact beta evidence")},
	} {
		if err := ledger.ArchiveContextArtifact(ctx, artifact); err != nil {
			t.Fatal(err)
		}
	}
	alpha := store.ContextScope{SessionIDs: []string{session.ID}, Owners: []string{"member:alpha:d1"}}
	matches, err := ledger.SearchContextInScope(ctx, alpha, "QK-4417", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0].Reference != refA || matches[0].Kind != "tool.output.archive" {
		t.Fatalf("alpha search crossed owner scope: %#v", matches)
	}
	if _, err := ledger.ContextArtifactInScope(ctx, alpha, refB); err == nil {
		t.Fatal("alpha opened beta's private artifact")
	}
	granted := alpha
	granted.ArtifactReferences = []string{refB}
	opened, err := ledger.ContextArtifactInScope(ctx, granted, refB)
	if err != nil || string(opened.Content) != "QK-4417 exact beta evidence" {
		t.Fatalf("explicit artifact grant = (%#v, %v)", opened, err)
	}
	if err := ledger.ArchiveContextArtifact(ctx, store.ContextArtifact{
		Reference: refA, SessionID: session.ID, Owner: "member:alpha:d1",
		Content: []byte("different bytes"),
	}); err == nil {
		t.Fatal("immutable artifact reference accepted different bytes")
	}
}
