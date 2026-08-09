package store_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"altv1/internal/event"
	"altv1/internal/profile"
	"altv1/internal/store"
	builtinprofiles "altv1/profiles"
)

func TestProfileRevisionIsImmutable(t *testing.T) {
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
	if err := ledger.ImportProfile(ctx, document); err != nil {
		t.Fatalf("idempotent import: %v", err)
	}
	changed := document.Profile
	changed.Name = "Changed in place"
	conflict, err := profile.FromValue(changed)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.ImportProfile(ctx, conflict); !errors.Is(err, store.ErrProfileRevisionConflict) {
		t.Fatalf("expected revision conflict, got %v", err)
	}
}

func TestPublishProfileCreatesNextRevisionAndRejectsStaleDraft(t *testing.T) {
	ctx := context.Background()
	ledger, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	value := mustProfile(t).Profile
	value.ID = "published-team"
	value.Revision = 99 // the store, not the mutable draft, owns the revision
	first, err := ledger.PublishProfile(ctx, value, 0)
	if err != nil {
		t.Fatal(err)
	}
	if first.Profile.Revision != 1 {
		t.Fatalf("first revision = %d, want 1", first.Profile.Revision)
	}
	value.Name = "Published Team Revision Two"
	second, err := ledger.PublishProfile(ctx, value, 1)
	if err != nil {
		t.Fatal(err)
	}
	if second.Profile.Revision != 2 {
		t.Fatalf("second revision = %d, want 2", second.Profile.Revision)
	}
	if _, err := ledger.PublishProfile(ctx, value, 1); !errors.Is(err, store.ErrProfileDraftStale) {
		t.Fatalf("stale editor publish = %v, want ErrProfileDraftStale", err)
	}
}

func TestSlowSubscriberDoesNotLoseEvents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ledger, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	document := mustProfile(t)
	if err := ledger.ImportProfile(ctx, document); err != nil {
		t.Fatal(err)
	}
	session, err := ledger.CreateSession(ctx, document, "exercise event delivery", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	events, unsubscribe, err := ledger.Subscribe(ctx, session.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer unsubscribe()

	const count = 1200
	for i := 0; i < count; i++ {
		if _, err := ledger.Append(ctx, session.ID, event.Draft{
			Kind: event.ModelUsage, Actor: "test",
			Data: event.ModelUsageData{Model: "fake", TotalTokens: i},
		}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < count; i++ {
		select {
		case item := <-events:
			want := int64(i + 3)
			if item.Sequence != want {
				t.Fatalf("event %d has sequence %d, want %d", i, item.Sequence, want)
			}
		case <-ctx.Done():
			t.Fatalf("received only %d/%d events: %v", i, count, ctx.Err())
		}
	}
}

func TestFileStoreUsesWALAndPersistsWorkspace(t *testing.T) {
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
	workspace := t.TempDir()
	session, err := ledger.CreateSession(ctx, document, "persistent task", workspace)
	if err != nil {
		t.Fatal(err)
	}
	var mode string
	if err := ledger.DB().QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal mode = %q, want wal", mode)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.Session(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Workspace != workspace || got.Title != "persistent task" {
		t.Fatalf("reopened session = %#v", got)
	}
}

func TestContinuationKeepsOneDurableConversation(t *testing.T) {
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
	first, err := ledger.CreateSession(ctx, document, "first turn", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if first.ConversationID != first.ID {
		t.Fatalf("first conversation id = %q, want session id %q", first.ConversationID, first.ID)
	}
	if _, err := ledger.Append(ctx, first.ID, event.Draft{
		Kind: event.FinalCompleted, Actor: "lead",
		Data: event.FinalCompletedData{Answer: "first answer"},
	}); err != nil {
		t.Fatal(err)
	}
	second, err := ledger.CreateContinuation(ctx, first.ID, document, "follow-up turn")
	if err != nil {
		t.Fatal(err)
	}
	if second.ConversationID != first.ConversationID {
		t.Fatalf("continuation conversation = %q, want %q", second.ConversationID, first.ConversationID)
	}
	turns, err := ledger.ConversationSessions(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 || turns[0].Task != "first turn" || turns[1].Task != "follow-up turn" {
		t.Fatalf("conversation turns = %#v", turns)
	}
	conversations, err := ledger.ListSessions(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(conversations) != 1 || conversations[0].ID != second.ID {
		t.Fatalf("session list did not collapse turns into the latest conversation row: %#v", conversations)
	}
	resolved, err := ledger.ResolveSessionID(ctx, shortPrefix(first.ConversationID))
	if err != nil {
		t.Fatal(err)
	}
	if resolved != second.ID {
		t.Fatalf("conversation resolved to %q, want latest turn %q", resolved, second.ID)
	}
	if err := ledger.RenameSession(ctx, second.ID, "shared title"); err != nil {
		t.Fatal(err)
	}
	turns, err = ledger.ConversationSessions(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, turn := range turns {
		if turn.Title != "shared title" {
			t.Fatalf("turn %s title = %q", turn.ID, turn.Title)
		}
	}
}

func TestSessionCursorPaginationRetrievesEveryConversationExactlyOnce(t *testing.T) {
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

	const conversationCount = 37
	expected := make(map[string]struct{}, conversationCount)
	for index := 0; index < conversationCount; index++ {
		session, err := ledger.CreateSession(
			ctx,
			document,
			fmt.Sprintf("conversation %02d", index),
			t.TempDir(),
		)
		if err != nil {
			t.Fatal(err)
		}
		expected[session.ConversationID] = struct{}{}
	}

	seen := make(map[string]struct{}, conversationCount)
	var cursor *store.SessionCursor
	for {
		page, err := ledger.ListSessionPage(ctx, cursor, 7)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Items {
			if _, duplicate := seen[item.ConversationID]; duplicate {
				t.Fatalf("conversation %s appeared in more than one page", item.ConversationID)
			}
			seen[item.ConversationID] = struct{}{}
		}
		if page.Next == nil {
			break
		}
		cursor = page.Next
	}
	if len(seen) != len(expected) {
		t.Fatalf("cursor traversal found %d/%d conversations", len(seen), len(expected))
	}
	for id := range expected {
		if _, found := seen[id]; !found {
			t.Fatalf("cursor traversal omitted conversation %s", id)
		}
	}
}

func TestPromptSnapshotDoesNotShiftWhenNewTurnsAreAppended(t *testing.T) {
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
	for _, prompt := range []string{"oldest", "middle", "snapshot newest"} {
		if _, err := ledger.CreateSession(ctx, document, prompt, t.TempDir()); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := ledger.PromptSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.CreateSession(ctx, document, "appended later", t.TempDir()); err != nil {
		t.Fatal(err)
	}

	want := []string{"snapshot newest", "middle", "oldest"}
	for offset, expected := range want {
		prompt, found, err := ledger.PromptAt(ctx, snapshot, offset)
		if err != nil {
			t.Fatal(err)
		}
		if !found || prompt != expected {
			t.Fatalf("snapshot offset %d = (%q, %t), want %q", offset, prompt, found, expected)
		}
	}
	if _, found, err := ledger.PromptAt(ctx, snapshot, len(want)); err != nil || found {
		t.Fatalf("lookup beyond snapshot = found %t, err %v", found, err)
	}
}

func TestPromptCursorPaginationRetrievesEveryTurn(t *testing.T) {
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
	const count = 53
	for index := 0; index < count; index++ {
		if _, err := ledger.CreateSession(
			ctx,
			document,
			fmt.Sprintf("prompt %02d", index),
			t.TempDir(),
		); err != nil {
			t.Fatal(err)
		}
	}

	seen := make(map[string]struct{}, count)
	var cursor *store.PromptCursor
	for {
		page, err := ledger.ListPromptPage(ctx, cursor, 6)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Items {
			if _, duplicate := seen[item.SessionID]; duplicate {
				t.Fatalf("prompt row %s appeared twice", item.SessionID)
			}
			seen[item.SessionID] = struct{}{}
		}
		if page.Next == nil {
			break
		}
		cursor = page.Next
	}
	if len(seen) != count {
		t.Fatalf("cursor traversal found %d/%d prompt rows", len(seen), count)
	}
}

func TestSubscriptionObservesAnotherProcessStore(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "alt.db")
	first, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	document := mustProfile(t)
	if err := first.ImportProfile(ctx, document); err != nil {
		t.Fatal(err)
	}
	session, err := first.CreateSession(ctx, document, "cross-process cancellation", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	events, unsubscribe, err := first.Subscribe(ctx, session.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer unsubscribe()
	if _, err := second.Append(ctx, session.ID, event.Draft{
		Kind: event.SessionCancelled, Actor: "other-process",
		Data: event.FailureData{Error: "cancelled elsewhere"},
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case item := <-events:
		if item.Kind != event.SessionCancelled {
			t.Fatalf("received %s, want cancellation", item.Kind)
		}
	case <-ctx.Done():
		t.Fatal("cross-process event was not observed")
	}
}

func TestSettingsRoundTripAndReplaceAtomically(t *testing.T) {
	ctx := context.Background()
	ledger, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	if value, found, err := ledger.Setting(ctx, "research.provider"); err != nil || found || value != "" {
		t.Fatalf("missing setting = (%q, %v, %v)", value, found, err)
	}
	if err := ledger.SetSetting(ctx, "research.provider", "exa"); err != nil {
		t.Fatal(err)
	}
	if err := ledger.SetSetting(ctx, "research.provider", "linkup"); err != nil {
		t.Fatal(err)
	}
	value, found, err := ledger.Setting(ctx, "research.provider")
	if err != nil || !found || value != "linkup" {
		t.Fatalf("current setting = (%q, %v, %v)", value, found, err)
	}
}

func mustProfile(t *testing.T) *profile.Document {
	t.Helper()
	document, err := profile.Parse(builtinprofiles.Engineering)
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func shortPrefix(value string) string {
	if len(value) < 8 {
		return value
	}
	return value[:8]
}
