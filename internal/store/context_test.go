package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"altv1/internal/event"
	"altv1/internal/store"
)

func TestContextArchiveIndexesAndRecoversExactEventPayload(t *testing.T) {
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
	session, err := ledger.CreateSession(ctx, document, "preserve exact evidence", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	item, err := ledger.Append(ctx, session.ID, event.Draft{
		Kind: event.ToolCompleted, Actor: "research", CorrelationID: "delegation-7",
		CausationID: "tool-call-event-id",
		Data: event.ToolCompletedData{
			DelegationID: "delegation-7", ToolCallID: "call-11",
			Tool: "web_fetch", Result: "The exact QK-4417 evidence, punctuation: []{}.",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	records, err := ledger.ContextRecords(ctx, session.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != int(item.Sequence) {
		t.Fatalf("context records = %d, want one for every %d events", len(records), item.Sequence)
	}
	record := records[len(records)-1]
	if record.SourceSequence != item.Sequence || record.Kind != event.ToolCompleted {
		t.Fatalf("archived record = %#v", record)
	}
	if !bytes.Equal(record.Content, item.Data) {
		t.Fatalf("archive changed event bytes:\n got %s\nwant %s", record.Content, item.Data)
	}

	opened, err := ledger.ContextRecord(ctx, session.ID, record.Reference())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(opened.Content, item.Data) || opened.Digest != record.Digest ||
		opened.CausationID != "tool-call-event-id" || !opened.CreatedAt.Equal(item.At) {
		t.Fatal("exact context reference did not recover the archived occurrence")
	}
	matches, err := ledger.SearchContext(ctx, session.ID, "QK-4417 punctuation", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0].Reference != record.Reference() {
		t.Fatalf("context matches = %#v", matches)
	}

	other, err := ledger.CreateSession(ctx, document, "isolated request", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.ContextRecord(ctx, other.ID, record.Reference()); err == nil {
		t.Fatal("a context reference crossed its request boundary")
	}
}

func TestOpeningExistingDatabaseBackfillsLosslessArchives(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "alt.db")
	ledger, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	document := mustProfile(t)
	if err := ledger.ImportProfile(ctx, document); err != nil {
		t.Fatal(err)
	}
	session, err := ledger.CreateSession(ctx, document, "legacy exact evidence", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Set(ctx, "legacy-checkpoint", []byte("exact checkpoint")); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Append(ctx, session.ID, event.Draft{
		Kind: event.LeadTurnStarted, Actor: "engineering",
		CorrelationID: "lead-turn", CausationID: "previous-event",
		Data: event.LeadTurnData{Turn: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.DB().ExecContext(ctx, `DELETE FROM context_records`); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.DB().ExecContext(ctx, `DELETE FROM checkpoint_versions`); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	records, err := reopened.ContextRecords(ctx, session.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 || !bytes.Contains(records[0].Content, []byte("legacy exact evidence")) ||
		records[2].CausationID != "previous-event" {
		t.Fatalf("backfilled context records = %#v", records)
	}
	versions, err := reopened.CheckpointVersions(ctx, "legacy-checkpoint")
	if err != nil || len(versions) != 1 || string(versions[0].Value) != "exact checkpoint" {
		t.Fatalf("backfilled checkpoint versions = (%#v, %v)", versions, err)
	}
}

func TestContextEpochIsVersionedAndBoundToDurableSource(t *testing.T) {
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
	session, err := ledger.CreateSession(ctx, document, "compact safely", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	last, err := ledger.LastSequence(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	view := json.RawMessage(`{"objective":"compact safely","open_work":[],"evidence":[]}`)
	first, err := ledger.CommitContextEpoch(ctx, store.ContextEpoch{
		SessionID: session.ID, ScopeKind: "request", ScopeID: session.ID,
		SourceThroughSequence: last, View: view, EstimatedTokens: 17,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := ledger.CommitContextEpoch(ctx, store.ContextEpoch{
		SessionID: session.ID, ScopeKind: "request", ScopeID: session.ID,
		SourceThroughSequence: last, View: view, EstimatedTokens: 17,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Epoch != 1 || second.Epoch != 2 || first.ViewDigest != second.ViewDigest {
		t.Fatalf("epochs = %#v then %#v", first, second)
	}
	latest, found, err := ledger.LatestContextEpoch(ctx, session.ID, "request", session.ID)
	if err != nil || !found || latest.Epoch != 2 || !bytes.Equal(latest.View, view) {
		t.Fatalf("latest epoch = (%#v, %v, %v)", latest, found, err)
	}
	if _, err := ledger.CommitContextEpoch(ctx, store.ContextEpoch{
		SessionID: session.ID, ScopeKind: "request", ScopeID: session.ID,
		SourceThroughSequence: last + 1, View: view,
	}); err == nil {
		t.Fatal("context epoch accepted evidence that was not yet durable")
	}
}

func TestRepeatedContextEpochsNeverReplaceCanonicalEvidence(t *testing.T) {
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
	session, err := ledger.CreateSession(ctx, document, "repeat compaction without erosion", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	recordsBefore, err := ledger.ContextRecords(ctx, session.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	last, err := ledger.LastSequence(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	for epoch := 1; epoch <= 128; epoch++ {
		view := json.RawMessage(fmt.Sprintf(`{"epoch":%d,"pinned":"repeat compaction without erosion"}`, epoch))
		committed, err := ledger.CommitContextEpoch(ctx, store.ContextEpoch{
			SessionID: session.ID, ScopeKind: "lead", ScopeID: "lead-1",
			SourceThroughSequence: last, View: view, EstimatedTokens: 12,
		})
		if err != nil {
			t.Fatalf("epoch %d: %v", epoch, err)
		}
		if committed.Epoch != epoch {
			t.Fatalf("allocated epoch %d, want %d", committed.Epoch, epoch)
		}
	}
	latest, found, err := ledger.LatestContextEpoch(ctx, session.ID, "lead", "lead-1")
	if err != nil || !found || latest.Epoch != 128 {
		t.Fatalf("latest repeated epoch = (%#v, %v, %v)", latest, found, err)
	}
	recordsAfter, err := ledger.ContextRecords(ctx, session.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(recordsAfter) != len(recordsBefore) {
		t.Fatalf("canonical record count changed across projections: %d → %d", len(recordsBefore), len(recordsAfter))
	}
	for index := range recordsBefore {
		if recordsBefore[index].Digest != recordsAfter[index].Digest || !bytes.Equal(recordsBefore[index].Content, recordsAfter[index].Content) {
			t.Fatalf("canonical record %d eroded after repeated compaction", index)
		}
	}
}

func TestContextBrowsePagesUnknownEvidenceWithoutCrossingScope(t *testing.T) {
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
	session, err := ledger.CreateSession(ctx, document, "browse without query terms", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 4; index++ {
		correlation := "alpha"
		if index == 2 {
			correlation = "beta"
		}
		if _, err := ledger.Append(ctx, session.ID, event.Draft{
			Kind: event.UserInstruction, Actor: "user", CorrelationID: correlation,
			Data: event.UserInstructionData{Text: fmt.Sprintf("opaque observation %d", index)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	scope := store.ContextScope{SessionIDs: []string{session.ID}, CorrelationIDs: []string{"alpha"}}
	first, err := ledger.BrowseContextInScope(ctx, scope, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Records) != 2 || first.NextCursor == "" {
		t.Fatalf("first browse page = %#v", first)
	}
	second, err := ledger.BrowseContextInScope(ctx, scope, first.NextCursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Records) != 1 || second.NextCursor != "" {
		t.Fatalf("second browse page = %#v", second)
	}
	seen := map[string]bool{}
	for _, record := range append(first.Records, second.Records...) {
		if seen[record.Reference] || record.CorrelationID != "alpha" || strings.Contains(record.Preview, "observation 2") {
			t.Fatalf("browse pagination or scope failed: %#v", record)
		}
		seen[record.Reference] = true
	}
	if _, err := ledger.BrowseContextInScope(ctx, scope, "not-a-cursor", 2); err == nil {
		t.Fatal("invalid browse cursor was accepted")
	}
}
