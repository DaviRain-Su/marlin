// Package app bridges the Marlin Session to the web frontend through mygo
// typed IPC. It makes no protocol decisions: everything delegates to the
// client layer (design doc §7).
package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/egoist/mygo"

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
	Completed            bool   `json:"completed"`
	CancellationRequested bool  `json:"cancellationRequested"`
	Outcome              string `json:"outcome"`
}

// Snapshot is the session state the page pulls at startup and after
// reconnection banners.
type Snapshot struct {
	Ready         bool   `json:"ready"`
	SessionID     string `json:"sessionId,omitempty"`
	AfterTurn     string `json:"afterTurn,omitempty"`
	ActiveOrdinal string `json:"activeOrdinal,omitempty"`
}

// ConnState drives the page's status banner.
type ConnState struct {
	// State: "starting" | "ready" | "reconnecting" | "offline" | "error".
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
}

// Status broadcasts connection state to every page.
var Status = mygo.NewEvent[ConnState]("status")

// ChatService is bound to the frontend: Chat.send / Chat.stop / Chat.state
// in the generated client.
type ChatService struct {
	mu     sync.Mutex
	mgr    *client.SessionManager
	active *client.TurnRef
}

// NewChatService builds the bound service; SetManager installs the session
// manager once the sidecar host is ready.
func NewChatService() *ChatService { return &ChatService{} }

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
// On any failure the draft stays with the page (design §6.3). Closing the
// page cancels only the watch; the turn keeps executing in the host.
func (c *ChatService) Send(ctx context.Context, text string, out *mygo.Channel[TurnProgress]) error {
	m, err := c.manager()
	if err != nil {
		return err
	}
	if text == "" {
		return fmt.Errorf("empty message")
	}

	turn, err := m.Send(ctx, newSubmissionID(), text)
	if err != nil {
		return err
	}
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
		if err := out.Send(toProgress(turn.Ordinal, ev)); err != nil {
			return err // page stopped listening
		}
		if ev.Kind == client.EventOutcome {
			break
		}
	}
	cancel()
	return <-followErr
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
		Completed:            r.Completed,
		CancellationRequested: r.CancellationRequested,
		Outcome:              r.Outcome.String(),
	}, nil
}

// State returns the current session snapshot for the page.
func (c *ChatService) State(ctx context.Context) (Snapshot, error) {
	m, err := c.manager()
	if err != nil {
		return Snapshot{Ready: false}, nil // not an error for the page
	}
	c.mu.Lock()
	var active string
	if c.active != nil {
		active = fmt.Sprintf("%d", c.active.Ordinal)
	}
	c.mu.Unlock()
	return Snapshot{
		Ready:         true,
		SessionID:     m.SessionID(),
		AfterTurn:     fmt.Sprintf("%d", m.AfterTurn()),
		ActiveOrdinal: active,
	}, nil
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
		p.Outcome = ev.Outcome.String()
	}
	p.Ordinal = fmt.Sprintf("%d", ordinal)
	return p
}

// newSubmissionID mints a fresh per-send identifier: one user action, one ID.
func newSubmissionID() string {
	var b [2]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("send-%d-%s", time.Now().UnixMilli(), hex.EncodeToString(b[:]))
}
