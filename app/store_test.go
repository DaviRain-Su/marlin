package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"marlin-desktop/client"

	appv1 "marlin-desktop/gen/marlin/app/v1"
)

// loadForTest binds a fresh store in dir to session id and returns it.
func loadForTest(t *testing.T, dir, id string) *Store {
	t.Helper()
	s := LoadStore(dir)
	if err := s.BindSession(id); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStoreRoundtripAndOrdinalAsString(t *testing.T) {
	dir := t.TempDir()
	s := loadForTest(t, dir, "sess-1")

	if err := s.SetPending(PendingSubmission{SubmissionID: "send-1", Text: "hi", AfterTurn: 7}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertTurn(StoredTurn{Ordinal: 18446744073709551615, UserText: "hi", AssistantText: "yo", Outcome: "COMPLETED", LastEventID: "ev-9"}); err != nil {
		t.Fatal(err)
	}

	// Reload from disk: everything must come back.
	s2 := loadForTest(t, dir, "sess-1")
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
	b, err := os.ReadFile(filepath.Join(dir, "sessions", "sess-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"ordinal": "18446744073709551615"`) {
		t.Fatalf("ordinal not string-encoded: %s", b)
	}
}

func TestStoreUpsertReplacesByOrdinal(t *testing.T) {
	s := loadForTest(t, t.TempDir(), "sess-1")
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
	dir := t.TempDir()
	s := loadForTest(t, dir, "sess-1")
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
	if p := loadForTest(t, dir, "sess-1").Pending(); p != nil {
		t.Fatal("pending survived clear on disk")
	}
}

func TestStoreCorruptFileFallsBackEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "sessions", "sess-1.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := loadForTest(t, dir, "sess-1")
	if len(s.Turns()) != 0 || s.Pending() != nil {
		t.Fatal("corrupt file should start empty")
	}
	// The store must still be writable after fallback.
	if err := s.UpsertTurn(StoredTurn{Ordinal: 1, UserText: "ok"}); err != nil {
		t.Fatal(err)
	}
	if len(loadForTest(t, dir, "sess-1").Turns()) != 1 {
		t.Fatal("save after corrupt fallback failed")
	}
}

func TestStoreSessionsAreIsolatedAndPersistent(t *testing.T) {
	dir := t.TempDir()
	a := loadForTest(t, dir, "sess-a")
	if err := a.UpsertTurn(StoredTurn{Ordinal: 1, UserText: "q-a"}); err != nil {
		t.Fatal(err)
	}

	// A different session's view starts empty and never sees sess-a's rows.
	b := loadForTest(t, dir, "sess-b")
	if len(b.Turns()) != 0 || b.SessionID() != "sess-b" {
		t.Fatalf("foreign session view = %+v", b.Turns())
	}

	// Binding back to sess-a restores its persisted projection.
	if err := b.BindSession("sess-a"); err != nil {
		t.Fatal(err)
	}
	turns := b.Turns()
	if len(turns) != 1 || turns[0].UserText != "q-a" {
		t.Fatalf("sess-a turns after rebind = %+v", turns)
	}
}

func TestStoreLegacySingleFileMigrates(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"sessionId":"old-sess","turns":[{"ordinal":"1","userText":"q","lastEventId":"e1"}]}`
	if err := os.WriteFile(filepath.Join(dir, "session.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	s := loadForTest(t, dir, "old-sess")
	turns := s.Turns()
	if len(turns) != 1 || turns[0].UserText != "q" {
		t.Fatalf("migrated turns = %+v", turns)
	}
}

func TestStoreSaveLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	s := loadForTest(t, dir, "sess-1")
	if err := s.UpsertTurn(StoredTurn{Ordinal: 1, UserText: "q"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestSessionRegistrySeedsAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	r := LoadSessionRegistry(path)
	if r.Current() != "gui-session" || len(r.List()) != 1 {
		t.Fatalf("seeded registry = %+v", r.List())
	}

	meta, err := r.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if r.Current() != meta.ID {
		t.Fatal("new session must become current")
	}
	r.Touch(meta.ID, "  hello   world  ")
	r2 := LoadSessionRegistry(path)
	if got := r2.Current(); got != meta.ID {
		t.Fatalf("current = %q, want %q", got, meta.ID)
	}
	items := r2.List()
	if items[0].ID != meta.ID || items[0].Title != "hello world" || items[0].UpdatedAt == "" {
		t.Fatalf("items = %+v", items)
	}

	if err := r2.SetCurrent("nope"); err == nil {
		t.Fatal("unknown session must be refused")
	}
	if err := r2.SetCurrent("gui-session"); err != nil {
		t.Fatal(err)
	}
	if LoadSessionRegistry(path).Current() != "gui-session" {
		t.Fatal("SetCurrent did not persist")
	}
}

func TestSessionRegistryTouchKeepsExistingTitle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	r := LoadSessionRegistry(path)
	meta, err := r.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	r.Touch(meta.ID, "第一个标题")
	r.Touch(meta.ID, "第二条消息不该改标题")
	items := r.List()
	if items[0].Title != "第一个标题" {
		t.Fatalf("title = %q", items[0].Title)
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
