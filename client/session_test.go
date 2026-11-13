package client_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	appv1 "marlin-desktop/gen/marlin/app/v1"
	"marlin-desktop/client"
	"marlin-desktop/fakehost"
)

// startSessionHost serves a fake host and returns its addr plus the host
// handle (for pacing controls).
func startSessionHost(t *testing.T, sessionID string) (string, *fakehost.Host) {
	t.Helper()
	ln, err := fakehost.Listen()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	h := fakehost.New(sessionID)
	srv := fakehost.NewServer(h, "")
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	return ln.Addr().String(), h
}

func dial(t *testing.T, addr string) *client.Conn {
	t.Helper()
	conn, err := client.Dial(context.Background(), client.Config{Addr: addr})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func collectUntilOutcome(t *testing.T, ctx context.Context, m *client.SessionManager, turn client.TurnRef) []client.SessionEvent {
	t.Helper()
	ch := make(chan client.SessionEvent, 64)
	go func() { _ = m.Follow(ctx, turn, ch) }()
	var evs []client.SessionEvent
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-ch:
			evs = append(evs, ev)
			if ev.Kind == client.EventOutcome {
				return evs
			}
		case <-deadline:
			t.Fatal("timed out waiting for outcome")
		}
	}
}

func TestSendFollowOutcome(t *testing.T) {
	addr, _ := startSessionHost(t, "gui-session")
	m := client.NewSessionManager(dial(t, addr), "gui-session")

	turn, err := m.Send(context.Background(), "send-1", "你好，Marlin")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if turn.Ordinal != 1 {
		t.Fatalf("first turn ordinal = %d, want 1", turn.Ordinal)
	}
	evs := collectUntilOutcome(t, context.Background(), m, turn)

	var kinds []client.EventKind
	var text strings.Builder
	for _, ev := range evs {
		kinds = append(kinds, ev.Kind)
		if ev.Kind == client.EventText {
			text.WriteString(ev.Text)
		}
	}
	if len(kinds) < 3 || kinds[0] != client.EventStarted || kinds[len(kinds)-1] != client.EventOutcome {
		t.Errorf("event kinds = %v, want Started ... Outcome", kinds)
	}
	if text.String() != "你好，Marlin" {
		t.Errorf("assistant text = %q, want echoed input", text.String())
	}
	if evs[len(evs)-1].Outcome != appv1.SessionTurnOutcome_SESSION_TURN_OUTCOME_COMPLETED {
		t.Errorf("outcome = %v, want COMPLETED", evs[len(evs)-1].Outcome)
	}
	// Watermark advanced.
	if m.AfterTurn() != 1 {
		t.Errorf("AfterTurn = %d, want 1", m.AfterTurn())
	}
}

func TestSubmissionIdempotentRetry(t *testing.T) {
	addr, _ := startSessionHost(t, "gui-session")
	m := client.NewSessionManager(dial(t, addr), "gui-session")

	// Simulate the response being lost: same submission id, manager replays
	// the ORIGINAL inputs even though the watermark advanced.
	turn1, err := m.Send(context.Background(), "send-1", "first")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	turn2, err := m.Send(context.Background(), "send-1", "first")
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if turn1 != turn2 {
		t.Errorf("retry returned %+v, want original %+v", turn2, turn1)
	}
}

func TestSequentialTurnsAdvanceWatermark(t *testing.T) {
	addr, _ := startSessionHost(t, "gui-session")
	m := client.NewSessionManager(dial(t, addr), "gui-session")

	for i := 1; i <= 3; i++ {
		sub := "send-" + string(rune('0'+i))
		turn, err := m.Send(context.Background(), sub, "msg")
		if err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		collectUntilOutcome(t, context.Background(), m, turn)
	}
	if m.AfterTurn() != 3 {
		t.Errorf("AfterTurn = %d, want 3", m.AfterTurn())
	}
}

func TestCancelRemoteUncertain(t *testing.T) {
	addr, h := startSessionHost(t, "gui-session")
	h.SetSlow(30 * time.Millisecond)
	conn := dial(t, addr)
	m := client.NewSessionManager(conn, "gui-session")

	turn, err := m.Send(context.Background(), "send-1", "a longer message that keeps streaming for a while")
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	ch := make(chan client.SessionEvent, 64)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = m.Follow(ctx, turn, ch) }()

	// Wait for the turn to start, then cancel mid-stream.
	select {
	case ev := <-ch:
		if ev.Kind != client.EventStarted {
			t.Fatalf("first event = %v, want Started", ev.Kind)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("turn did not start")
	}
	receipt, err := m.Cancel(context.Background(), turn)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if !receipt.Completed || !receipt.CancellationRequested {
		t.Errorf("receipt = %+v, want completed with cancellation_requested=true", receipt)
	}
	if receipt.Outcome != appv1.SessionTurnOutcome_SESSION_TURN_OUTCOME_REMOTE_UNCERTAIN {
		t.Errorf("cancel outcome = %v, want REMOTE_UNCERTAIN", receipt.Outcome)
	}
	// The watch still reaches a committed (uncertain) terminal outcome.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Kind == client.EventOutcome {
				if ev.Outcome != appv1.SessionTurnOutcome_SESSION_TURN_OUTCOME_REMOTE_UNCERTAIN {
					t.Errorf("committed outcome = %v, want REMOTE_UNCERTAIN", ev.Outcome)
				}
				return
			}
		case <-deadline:
			t.Fatal("no terminal outcome after cancel")
		}
	}
}

// The acceptance scenario: another client committed a turn meanwhile; our
// stale after_turn must be calibrated from the summary, not surfaced.
func TestCalibratesAfterAnotherClientCommits(t *testing.T) {
	addr, _ := startSessionHost(t, "gui-session")
	conn := dial(t, addr)
	mA := client.NewSessionManager(conn, "gui-session")
	mB := client.NewSessionManager(conn, "gui-session") // watermark 0

	turn1, err := mA.Send(context.Background(), "send-A", "from client A")
	if err != nil {
		t.Fatalf("A send: %v", err)
	}
	collectUntilOutcome(t, context.Background(), mA, turn1)

	turn2, err := mB.Send(context.Background(), "send-B", "from client B")
	if err != nil {
		t.Fatalf("B send after calibration: %v", err)
	}
	if turn2.Ordinal != 2 {
		t.Errorf("B turn ordinal = %d, want 2", turn2.Ordinal)
	}
	if mB.AfterTurn() != 2 {
		t.Errorf("B watermark = %d, want 2", mB.AfterTurn())
	}
}

// A busy session must surface the refusal, not spin: active turn blocks new
// work (design §6.3 FAILED_PRECONDITION branch).
func TestPreconditionBusySurfacesRefusal(t *testing.T) {
	addr, h := startSessionHost(t, "gui-session")
	h.SetSlow(200 * time.Millisecond)
	mA := client.NewSessionManager(dial(t, addr), "gui-session")
	mB := client.NewSessionManager(dial(t, addr), "gui-session")

	turn, err := mA.Send(context.Background(), "send-A", "long running")
	if err != nil {
		t.Fatalf("A send: %v", err)
	}
	defer collectUntilOutcome(t, context.Background(), mA, turn)

	_, err = mB.Send(context.Background(), "send-B", "while busy")
	if err == nil {
		t.Fatal("expected refusal while a turn is active")
	}
	kind, ok := client.AsErrKind(err)
	if !ok || kind != client.ErrPrecondition {
		t.Errorf("kind = %v (ok=%v), want ErrPrecondition", kind, ok)
	}
	if !strings.Contains(err.Error(), "session busy") {
		t.Errorf("error should state the session is busy: %v", err)
	}
}

func TestSummaryStates(t *testing.T) {
	addr, h := startSessionHost(t, "gui-session")
	m := client.NewSessionManager(dial(t, addr), "gui-session")

	s, err := m.Summary(context.Background())
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if s.RecoveryState != appv1.SessionRecoveryState_SESSION_RECOVERY_STATE_EMPTY {
		t.Fatalf("empty state = %v", s.RecoveryState)
	}

	// Active turn.
	h.SetSlow(50 * time.Millisecond)
	turn, err := m.Send(context.Background(), "send-1", "hello")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	s, err = m.Summary(context.Background())
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if s.RecoveryState != appv1.SessionRecoveryState_SESSION_RECOVERY_STATE_ACTIVE {
		t.Errorf("active state = %v, want ACTIVE", s.RecoveryState)
	}

	// After completion.
	collectUntilOutcome(t, context.Background(), m, turn)
	s, err = m.Summary(context.Background())
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if s.RecoveryState != appv1.SessionRecoveryState_SESSION_RECOVERY_STATE_FINISHED {
		t.Errorf("finished state = %v, want FINISHED", s.RecoveryState)
	}
}

// Watch-level protocol checks (cursor semantics) via the raw stream RPC.
func TestWatchCursorReturnsCommittedSuffixOnly(t *testing.T) {
	addr, _ := startSessionHost(t, "gui-session")
	conn := dial(t, addr)
	m := client.NewSessionManager(conn, "gui-session")

	turn, err := m.Send(context.Background(), "send-1", "hello world from fakehost")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	// Wait for the turn to be fully committed.
	collectUntilOutcome(t, context.Background(), m, turn)

	// Find the first text event's ID from a full replay.
	ch := make(chan client.SessionEvent, 64)
	go func() { _ = m.Follow(context.Background(), turn, ch) }()
	var firstTextID string
	deadline := time.After(5 * time.Second)
loop:
	for {
		select {
		case ev := <-ch:
			if ev.Kind == client.EventText {
				firstTextID = ev.ID
				break loop
			}
			if ev.Kind == client.EventOutcome {
				t.Fatal("no text event before outcome")
			}
		case <-deadline:
			t.Fatal("timed out")
		}
	}

	// Replay with after_event_id = firstTextID: the suffix must NOT contain it.
	suffix, err := watchSuffix(context.Background(), conn, turn, firstTextID)
	if err != nil {
		t.Fatalf("watch suffix: %v", err)
	}
	for _, ev := range suffix {
		if ev.GetEventId() == firstTextID {
			t.Errorf("suffix contains the exclusive cursor event %s", firstTextID)
		}
	}
	if len(suffix) == 0 {
		t.Error("suffix empty; expected remaining text + outcome")
	}
	last := suffix[len(suffix)-1]
	if last.GetOutcome() == appv1.SessionTurnOutcome_SESSION_TURN_OUTCOME_UNSPECIFIED {
		t.Error("suffix does not end with the outcome event")
	}

	// An unknown cursor is rejected, not silently replayed.
	_, err = watchSuffix(context.Background(), conn, turn, "nonexistent")
	if err == nil {
		t.Fatal("expected invalid cursor rejection")
	}
	if st, ok := status.FromError(err); !ok || st.Code() != codes.InvalidArgument {
		t.Errorf("error = %v, want InvalidArgument", err)
	}
}

func watchSuffix(ctx context.Context, conn *client.Conn, turn client.TurnRef, after string) ([]*appv1.SessionTurnEvent, error) {
	c := appv1.NewAppServerClient(conn.GRPC())
	req := &appv1.WatchSessionTurnRequest{Turn: &appv1.SessionTurnRef{SessionId: turn.SessionID, Ordinal: turn.Ordinal}}
	a := after
	req.AfterEventId = &a
	stream, err := c.WatchSessionTurn(ctx, req)
	if err != nil {
		return nil, err
	}
	var out []*appv1.SessionTurnEvent
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, ev)
	}
}
