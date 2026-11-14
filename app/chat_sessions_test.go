package app

import (
	"context"
	"path/filepath"
	"testing"

	"marlin-desktop/client"
	"marlin-desktop/fakehost"
)

// startFakeChat wires a ChatService over a live fakehost server, the same
// way main.go assembles the app (the mygo Channel cannot be constructed in
// Go tests, so turns themselves are covered by the client/fakehost suites).
func startFakeChat(t *testing.T) (*ChatService, *SessionRegistry) {
	t.Helper()
	ln, err := fakehost.Listen()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	h := fakehost.New("gui-session")
	srv := fakehost.NewServer(h, "")
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)

	dir := t.TempDir()
	registry := LoadSessionRegistry(filepath.Join(dir, "registry.json"))
	chat := NewChatService(LoadStore(dir), registry)

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
