package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"marlin-desktop/client"

	appv1 "marlin-desktop/gen/marlin/app/v1"
)

func TestStoreRoundtripAndOrdinalAsString(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")

	s := LoadStore(path)
	if len(s.Turns()) != 0 || s.Pending() != nil || s.SessionID() != "" {
		t.Fatalf("fresh store not empty: %+v", s.Turns())
	}

	if err := s.BindSession("sess-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPending(PendingSubmission{SubmissionID: "send-1", Text: "hi", AfterTurn: 7}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertTurn(StoredTurn{Ordinal: 18446744073709551615, UserText: "hi", AssistantText: "yo", Outcome: "COMPLETED", LastEventID: "ev-9"}); err != nil {
		t.Fatal(err)
	}

	// Reload from disk: everything must come back.
	s2 := LoadStore(path)
	if s2.SessionID() != "sess-1" {
		t.Fatalf("session = %q", s2.SessionID())
	}
	if p := s2.Pending(); p == nil || p.SubmissionID != "send-1" || p.AfterTurn != 7 {
		t.Fatalf("pending = %+v", p)
	}
	turns := s2.Turns()
	if len(turns) != 1 || turns[0].Ordinal != 18446744073709551615 || turns[0].LastEventID != "ev-9" {
		t.Fatalf("turns = %+v", turns)
	}

	// u64 ordinals must travel as JSON strings (TS precision, design §4).
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"ordinal": "18446744073709551615"`) {
		t.Fatalf("ordinal not string-encoded: %s", b)
	}
}

func TestStoreUpsertReplacesByOrdinal(t *testing.T) {
	s := LoadStore(filepath.Join(t.TempDir(), "session.json"))
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.UpsertTurn(StoredTurn{Ordinal: 3, UserText: "q"}))
	must(s.UpsertTurn(StoredTurn{Ordinal: 3, UserText: "q", AssistantText: "a"}))
	must(s.UpsertTurn(StoredTurn{Ordinal: 4, UserText: "q2"}))

	turns := s.Turns()
	if len(turns) != 2 || turns[0].Ordinal != 3 || turns[0].AssistantText != "a" {
		t.Fatalf("turns = %+v", turns)
	}
}

func TestStoreClearPending(t *testing.T) {
	s := LoadStore(filepath.Join(t.TempDir(), "session.json"))
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.SetPending(PendingSubmission{SubmissionID: "s", Text: "t", AfterTurn: 1}))
	must(s.ClearPending())
	if s.Pending() != nil {
		t.Fatal("pending survived clear")
	}
	if p := LoadStore(filepath.Join(s.path, "..", "session.json")).Pending(); p != nil {
		t.Fatal("pending survived clear on disk")
	}
}

func TestStoreCorruptFileFallsBackEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := LoadStore(path)
	if len(s.Turns()) != 0 || s.Pending() != nil {
		t.Fatal("corrupt file should start empty")
	}
	// The store must still be writable after fallback.
	if err := s.UpsertTurn(StoredTurn{Ordinal: 1, UserText: "ok"}); err != nil {
		t.Fatal(err)
	}
	if len(LoadStore(path).Turns()) != 1 {
		t.Fatal("save after corrupt fallback failed")
	}
}

func TestStoreBindSessionClearsForeignTurns(t *testing.T) {
	s := LoadStore(filepath.Join(t.TempDir(), "session.json"))
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.BindSession("sess-a"))
	must(s.UpsertTurn(StoredTurn{Ordinal: 1, UserText: "q"}))
	must(s.BindSession("sess-a"))
	if len(s.Turns()) != 1 {
		t.Fatal("same-session bind must keep turns")
	}
	must(s.BindSession("sess-b"))
	if len(s.Turns()) != 0 || s.SessionID() != "sess-b" {
		t.Fatalf("bind to new session = %+v", s.Turns())
	}
}

func TestStoreSaveLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")
	s := LoadStore(path)
	if err := s.UpsertTurn(StoredTurn{Ordinal: 1, UserText: "q"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestApplyEventFoldsAndDedupes(t *testing.T) {
	row := StoredTurn{Ordinal: 1, UserText: "q"}

	if !applyEvent(&row, client.SessionEvent{ID: "ev-1", Kind: client.EventStarted}) {
		t.Fatal("first event must apply")
	}
	if !applyEvent(&row, client.SessionEvent{ID: "ev-2", Kind: client.EventText, Text: "hel"}) {
		t.Fatal("text event must apply")
	}
	if !applyEvent(&row, client.SessionEvent{ID: "ev-3", Kind: client.EventText, Text: "lo"}) {
		t.Fatal("text event must apply")
	}
	if !applyEvent(&row, client.SessionEvent{ID: "ev-4", Kind: client.EventOutcome, Outcome: appv1.SessionTurnOutcome_SESSION_TURN_OUTCOME_COMPLETED}) {
		t.Fatal("outcome event must apply")
	}
	if row.AssistantText != "hello" || row.Outcome != "COMPLETED" || row.LastEventID != "ev-4" {
		t.Fatalf("row = %+v", row)
	}

	// Crash-skew replay: re-delivering the LAST applied event (the only kind
	// of replay the journal produces — it streams events after the cursor)
	// must be a no-op, regardless of payload.
	if applyEvent(&row, client.SessionEvent{ID: "ev-4", Kind: client.EventOutcome, Outcome: appv1.SessionTurnOutcome_SESSION_TURN_OUTCOME_COMPLETED}) {
		t.Fatal("replayed last event applied twice")
	}
	if row.AssistantText != "hello" {
		t.Fatalf("assistant text corrupted by replay: %q", row.AssistantText)
	}
}
