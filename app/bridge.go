// Package app bridges the Marlin Session to the web frontend through mygo
// typed IPC. It owns the applied projection and its cursor (the client-side
// view state, design doc §6.4/§6.6) and persists them atomically; it makes
// no protocol decisions — those live in the client layer.
package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/egoist/mygo"

	appv1 "marlin-desktop/gen/marlin/app/v1"
	"marlin-desktop/client"
)

// TurnProgress is one visible event forwarded to the page. u64 values travel
// as strings (TS numbers are not exact past 2^53, design doc §4).
type TurnProgress struct {
	EventID string `json:"eventId"`
	// Kind: "started" | "text" | "outcome".
	Kind    string `json:"kind"`
	Text    string `json:"text,omitempty"`
	Ordinal string `json:"ordinal,omitempty"`
	// Outcome is the SessionTurnOutcome name, e.g. "COMPLETED".
	Outcome string `json:"outcome,omitempty"`
}

// CancelInfo summarizes a Stop receipt for the page.
type CancelInfo struct {
	Completed             bool   `json:"completed"`
	CancellationRequested bool   `json:"cancellationRequested"`
	Outcome               string `json:"outcome"`
}

// Snapshot is the session state the page pulls at startup and after
// reconnection banners.
type Snapshot struct {
	Ready         bool         `json:"ready"`
	SessionID     string       `json:"sessionId,omitempty"`
	AfterTurn     string       `json:"afterTurn,omitempty"`
	ActiveOrdinal string       `json:"activeOrdinal,omitempty"`
	History       []StoredTurn `json:"history,omitempty"`
	PendingText   string       `json:"pendingText,omitempty"`
}

// ConnState drives the page's status banner.
type ConnState struct {
	// State: "starting" | "ready" | "reconnecting" | "offline" | "error" |
	// "unresolved".
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
}

// Status broadcasts connection state to every page.
var Status = mygo.NewEvent[ConnState]("status")

// ChatService is bound to the frontend: ChatService.send / stop / state /
// resume in the generated client.
type ChatService struct {
	store *Store

	mu     sync.Mutex
	mgr    *client.SessionManager
	active *client.TurnRef
}

// NewChatService builds the bound service over the projection store;
// setManager installs the session manager once the sidecar host is ready.
func NewChatService(store *Store) *ChatService {
	return &ChatService{store: store}
}

// setManager installs (or clears, with nil) the session manager. Unexported:
// the page must never be able to swap the backend connection.
func (c *ChatService) setManager(m *client.SessionManager) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mgr = m
	if m == nil {
		c.active = nil
	}
}

func (c *ChatService) manager() (*client.SessionManager, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mgr == nil {
		return nil, fmt.Errorf("marlin host is not ready yet")
	}
	return c.mgr, nil
}

// Send submits one user turn and streams the turn's visible events to out.
// The submission is persisted BEFORE the request goes out; on any failure
// the pending record stays and the draft returns to the page (design §6.3).
// Closing the page cancels only the watch; the turn keeps executing.
func (c *ChatService) Send(ctx context.Context, text string, out *mygo.Channel[TurnProgress]) error {
	m, err := c.manager()
	if err != nil {
		return err
	}
	if text == "" {
		return fmt.Errorf("empty message")
	}

	sub := newSubmissionID()
	if err := c.store.SetPending(PendingSubmission{SubmissionID: sub, Text: text, AfterTurn: m.AfterTurn()}); err != nil {
		slog.Warn("pending persist failed", "err", err) // recovery degrades, send proceeds
	}
	return c.streamTurn(ctx, m, sub, text, m.AfterTurn(), out, true)
}

// Resume re-admits the pending submission after a restart: the SAME id,
// text and original after_turn resolve to the original turn by idempotency
// (design §6.3). Nothing to do when no pending record exists.
func (c *ChatService) Resume(ctx context.Context, out *mygo.Channel[TurnProgress]) error {
	m, err := c.manager()
	if err != nil {
		return err
	}
	p := c.store.Pending()
	if p == nil {
		return nil
	}
	return c.streamTurn(ctx, m, p.SubmissionID, p.Text, p.AfterTurn, out, false)
}

// streamTurn admits and follows one turn, keeping the persisted projection
// in step with the applied cursor.
func (c *ChatService) streamTurn(ctx context.Context, m *client.SessionManager, sub, text string, afterTurn uint64, out *mygo.Channel[TurnProgress], clearPendingOnOutcome bool) error {
	turn, err := m.SendExact(ctx, sub, text, afterTurn)
	if err != nil {
		return err // pending record persists; the page keeps the draft
	}
	if err := c.store.ClearPending(); err != nil {
		slog.Warn("pending clear failed", "err", err)
	}
	row := StoredTurn{Ordinal: turn.Ordinal, UserText: text}
	_ = c.store.UpsertTurn(row) // show the user row even if later saves fail

	c.mu.Lock()
	c.active = &turn
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.active = nil
		c.mu.Unlock()
	}()

	events := make(chan client.SessionEvent, 64)
	followCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	followErr := make(chan error, 1)
	go func() { followErr <- m.Follow(followCtx, turn, events) }()

	for ev := range events {
		// Apply to the projection first (idempotent by event id — the cursor
		// may have advanced past what was persisted before a crash), persist
		// projection+cursor as one record, then forward the delta.
		if applyEvent(&row, ev) {
			if err := c.store.UpsertTurn(row); err != nil {
				slog.Warn("projection persist failed", "err", err)
			}
		}
		if err := out.Send(toProgress(turn.Ordinal, ev)); err != nil {
			return err // page stopped listening
		}
		if ev.Kind == client.EventOutcome {
			break
		}
	}
	cancel()
	if clearPendingOnOutcome {
		_ = c.store.ClearPending()
	}
	return <-followErr
}

// applyEvent folds one event into the projection. Returns false when the
// event was already applied (dedupe by event id, design §6.4).
func applyEvent(row *StoredTurn, ev client.SessionEvent) bool {
	if row.LastEventID == ev.ID {
		return false
	}
	switch ev.Kind {
	case client.EventText:
		row.AssistantText += ev.Text
	case client.EventOutcome:
		row.Outcome = outcomeName(ev.Outcome)
	case client.EventStarted:
		// The user row already carries the admission; nothing to fold.
	}
	row.LastEventID = ev.ID
	return true
}

// Stop requests cancellation of the active turn and returns its receipt.
func (c *ChatService) Stop(ctx context.Context) (CancelInfo, error) {
	c.mu.Lock()
	m, turn := c.mgr, c.active
	c.mu.Unlock()
	if m == nil {
		return CancelInfo{}, fmt.Errorf("marlin host is not ready yet")
	}
	if turn == nil {
		return CancelInfo{}, fmt.Errorf("no active turn")
	}
	r, err := m.Cancel(ctx, *turn)
	if err != nil {
		return CancelInfo{}, err
	}
	return CancelInfo{
		Completed:             r.Completed,
		CancellationRequested: r.CancellationRequested,
		Outcome:               outcomeName(r.Outcome),
	}, nil
}

// State returns the current session snapshot for the page.
func (c *ChatService) State(ctx context.Context) (Snapshot, error) {
	m, err := c.manager()
	snap := Snapshot{History: c.store.Turns()}
	if p := c.store.Pending(); p != nil {
		snap.PendingText = p.Text
	}
	if err != nil {
		return snap, nil // not ready is not an error for the page
	}
	c.mu.Lock()
	var active string
	if c.active != nil {
		active = fmt.Sprintf("%d", c.active.Ordinal)
	}
	c.mu.Unlock()
	snap.Ready = true
	snap.SessionID = m.SessionID()
	snap.AfterTurn = fmt.Sprintf("%d", m.AfterTurn())
	snap.ActiveOrdinal = active
	return snap, nil
}

func toProgress(ordinal uint64, ev client.SessionEvent) TurnProgress {
	p := TurnProgress{EventID: ev.ID}
	switch ev.Kind {
	case client.EventStarted:
		p.Kind = "started"
	case client.EventText:
		p.Kind = "text"
		p.Text = ev.Text
	case client.EventOutcome:
		p.Kind = "outcome"
		p.Outcome = outcomeName(ev.Outcome)
	}
	p.Ordinal = fmt.Sprintf("%d", ordinal)
	return p
}

// outcomeName is the short enum name ("COMPLETED"), without the protobuf
// prefix — the page switches on these names and the projection stores them.
func outcomeName(o appv1.SessionTurnOutcome) string {
	return strings.TrimPrefix(o.String(), "SESSION_TURN_OUTCOME_")
}

// newSubmissionID mints a fresh per-send identifier: one user action, one ID.
func newSubmissionID() string {
	var b [2]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("send-%d-%s", time.Now().UnixMilli(), hex.EncodeToString(b[:]))
}
