package app

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"marlin-desktop/client"
	"marlin-desktop/fakehost"
)

// startFakeChat wires a ChatService over a live fakehost server, the same
// way main.go and Connect assemble the app. Tests exercise the same send and
// follow paths through a callback in place of the page-owned mygo Channel.
func startFakeChat(t *testing.T, gateways ...fakehost.ChatGateway) (*ChatService, *SessionRegistry) {
	t.Helper()
	ln, err := fakehost.Listen()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	h := fakehost.New("gui-session")
	if len(gateways) > 0 {
		h.SetGateway(gateways[0])
	}
	srv := fakehost.NewServer(h, "")
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)

	dir := t.TempDir()
	registry := LoadSessionRegistry(filepath.Join(dir, "registry.json"))
	chat := NewChatService(LoadStore(dir), registry)
	if err := chat.store.BindSession(registry.Current()); err != nil {
		t.Fatal(err)
	}

	conn, err := client.Dial(context.Background(), client.Config{Addr: ln.Addr().String()})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	mgr := client.NewSessionManager(conn, chat.SessionID())
	if err := mgr.SyncFromSummary(context.Background()); err != nil {
		t.Fatalf("summary: %v", err)
	}
	chat.setManager(mgr)
	return chat, registry
}

type interruptGateway struct{}

func (interruptGateway) StreamTurn(ctx context.Context, history []fakehost.ChatMessage, onText func(string) error) error {
	if history[len(history)-1].Text == "interrupt" {
		if err := onText("partial answer"); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}
	return onText(history[len(history)-1].Text)
}

func TestChatStopThenSendPreservesHistoryAndUncertainty(t *testing.T) {
	chat, _ := startFakeChat(t, interruptGateway{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	emit := func(TurnProgress) error { return nil }
	if err := chat.send(ctx, "remember seahorse", emit); err != nil {
		t.Fatal(err)
	}
	parent := chat.SessionID()
	stopped := false
	err := chat.send(ctx, "interrupt", func(p TurnProgress) error {
		if p.Kind == "text" && !stopped {
			stopped = true
			receipt, err := chat.Stop(ctx)
			if err != nil {
				return err
			}
			if receipt.Outcome != "REMOTE_UNCERTAIN" {
				t.Errorf("cancel outcome = %s", receipt.Outcome)
			}
		}
		return nil
	})
	if err != nil || !stopped {
		t.Fatalf("stop path: stopped=%v err=%v", stopped, err)
	}
	if err := chat.send(ctx, "what did I say?", emit); err != nil {
		t.Fatal(err)
	}
	child := chat.SessionID()
	if child == parent {
		t.Fatal("uncertain journal must not be reused")
	}
	rows := chat.store.Turns()
	if len(rows) != 3 || rows[0].UserText != "remember seahorse" || rows[1].Outcome != "REMOTE_UNCERTAIN" || rows[1].AssistantText != "partial answer" || rows[2].Ordinal != 1 {
		t.Fatalf("continuation lost or overwrote history: %+v", rows)
	}
	if !strings.Contains(rows[2].AssistantText, "remember seahorse") || !strings.Contains(rows[2].AssistantText, "remote effects are unknown") {
		t.Fatal("new model turn did not receive the transcript and uncertainty")
	}
	reloaded := loadForTest(t, chat.store.dir, child)
	if len(reloaded.Turns()) != 3 {
		t.Fatal("continuation history did not persist")
	}
	if err := chat.SwitchSession(parent); err != nil {
		t.Fatal(err)
	}
	if rows := chat.store.Turns(); len(rows) != 2 || rows[1].Outcome != "REMOTE_UNCERTAIN" {
		t.Fatalf("source uncertainty changed: %+v", rows)
	}
}

func TestChatResumeAfterAdmissionLostPage(t *testing.T) {
	chat, _ := startFakeChat(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pageGone := fmt.Errorf("page closed")
	if err := chat.send(ctx, "recover this turn", func(TurnProgress) error { return pageGone }); err != pageGone {
		t.Fatalf("send returned %v", err)
	}
	snap, err := chat.State(ctx)
	if err != nil || snap.PendingText != "recover this turn" || len(snap.History) != 0 {
		t.Fatalf("resume snapshot = %+v, %v", snap, err)
	}
	if err := chat.resume(ctx, func(TurnProgress) error { return nil }); err != nil {
		t.Fatal(err)
	}
	rows := chat.store.Turns()
	if len(rows) != 1 || rows[0].AssistantText != "recover this turn" || rows[0].Outcome != "COMPLETED" {
		t.Fatalf("recovered projection = %+v", rows)
	}
}

func TestChatWatchFailureReturnsAndReleasesSession(t *testing.T) {
	chat, _ := startFakeChat(t)
	m, _ := chat.manager()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := chat.followTurn(ctx, m, client.TurnRef{SessionID: m.SessionID(), Ordinal: 99}, "missing", func(TurnProgress) error { return nil })
	if err == nil || ctx.Err() != nil || chat.busy() {
		t.Fatalf("watch failure: err=%v context=%v busy=%v", err, ctx.Err(), chat.busy())
	}
}

func TestContinuationTextIsBoundedUTF8(t *testing.T) {
	history := []StoredTurn{{UserText: strings.Repeat("海", 100000), AssistantText: strings.Repeat("山", 100000), Outcome: "REMOTE_UNCERTAIN"}}
	for _, text := range []string{"continue", strings.Repeat("a", 260000), strings.Repeat("a", 262144)} {
		wire := continuationText(history, text)
		if len(wire) > 262144 || !utf8.ValidString(wire) || !strings.HasSuffix(wire, text) {
			t.Fatalf("invalid continuation: bytes=%d utf8=%v", len(wire), utf8.ValidString(wire))
		}
	}
}

func TestChatNewSessionSwitchesView(t *testing.T) {
	chat, _ := startFakeChat(t)
	ctx := context.Background()

	sid1 := chat.SessionID()
	if sid1 == "" {
		t.Fatal("registry seed left no current session")
	}

	meta, err := chat.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if chat.SessionID() != meta.ID {
		t.Fatalf("session = %q, want %q", chat.SessionID(), meta.ID)
	}
	snap, err := chat.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.History) != 0 {
		t.Fatalf("new session history = %+v", snap.History)
	}
	if snap.SessionID != meta.ID {
		t.Fatalf("snapshot session = %q", snap.SessionID)
	}

	// The registry lists both, newest first, and the new one is current.
	items, err := chat.Sessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].ID != meta.ID || items[1].ID != sid1 {
		t.Fatalf("sessions = %+v", items)
	}

	// Switching back restores the first session's (empty) view identity.
	if err := chat.SwitchSession(sid1); err != nil {
		t.Fatal(err)
	}
	if chat.SessionID() != sid1 {
		t.Fatalf("session = %q, want %q", chat.SessionID(), sid1)
	}
}

func TestChatSwitchValidatesID(t *testing.T) {
	chat, _ := startFakeChat(t)
	if err := chat.SwitchSession("  "); err == nil {
		t.Fatal("empty session id must be refused")
	}
	if err := chat.SwitchSession("unknown-session"); err == nil {
		t.Fatal("unregistered session must be refused")
	}
}

func TestChatRenameAndDeleteSession(t *testing.T) {
	chat, _ := startFakeChat(t)

	meta, err := chat.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := chat.RenameSession(meta.ID, "重要讨论"); err != nil {
		t.Fatal(err)
	}
	items, err := chat.Sessions()
	if err != nil {
		t.Fatal(err)
	}
	if items[0].Title != "重要讨论" {
		t.Fatalf("title = %q", items[0].Title)
	}
	if err := chat.RenameSession(meta.ID, "  "); err == nil {
		t.Fatal("empty title must be refused")
	}
	if err := chat.RenameSession("nope", "x"); err == nil {
		t.Fatal("unknown session must be refused")
	}

	// The open session cannot be deleted; another one can.
	if err := chat.DeleteSession(meta.ID); err == nil {
		t.Fatal("deleting the open session must be refused")
	}
	if err := chat.SwitchSession("gui-session"); err != nil {
		t.Fatal(err)
	}
	if err := chat.DeleteSession(meta.ID); err != nil {
		t.Fatal(err)
	}
	if items, _ := chat.Sessions(); len(items) != 1 || items[0].ID != "gui-session" {
		t.Fatalf("sessions after delete = %+v", items)
	}
}

func TestChatSwitchRefusedWhileTurnActive(t *testing.T) {
	chat, _ := startFakeChat(t)
	// A second session must exist so the switch crosses sessions.
	meta, err := chat.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := chat.SwitchSession("gui-session"); err != nil {
		t.Fatal(err)
	}
	chat.mu.Lock()
	chat.active = &client.TurnRef{SessionID: chat.SessionID(), Ordinal: 1}
	chat.mu.Unlock()

	if _, err := chat.NewSession(); err == nil {
		t.Fatal("NewSession must be refused while a turn is active")
	}
	if err := chat.SwitchSession(meta.ID); err == nil {
		t.Fatal("SwitchSession must be refused while a turn is active")
	}
}
