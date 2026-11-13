// Package fakehost implements an in-process Marlin App Server for tests and
// the dev-mode host binary (cmd/devhost). It speaks the real marlin.app.v1
// wire protocol, so the client layer is tested against actual gRPC traffic
// without the Rust host. The Rust sidecar replaces it in production.
//
// Session semantics mirror the upstream contract (design doc §6.3/§6.4):
// submission-id idempotency, stale-ordinal refusal, exclusive active turn,
// journal events with opaque IDs, and cancellation racing completion.
package fakehost

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	appv1 "marlin-desktop/gen/marlin/app/v1"
)

// errTurnCanceled aborts the gateway stream when cancellation wins.
var errTurnCanceled = errors.New("fakehost: turn canceled")

func statusError(c codes.Code, msg string) error { return status.Error(c, msg) }

// turnState is one journal turn: committed events, live flag, outcome.
type turnState struct {
	sessionID    string
	ordinal      uint64
	submissionID string
	inputText    string
	afterTurn    uint64

	// ctx drives provider-side cancellation: CancelSessionTurn fires cancelFn
	// so an in-flight gateway stream drops (provider effects stay unknown).
	ctx      context.Context
	cancelFn context.CancelFunc

	mu      sync.Mutex
	events  []*appv1.SessionTurnEvent
	active  bool
	canceled bool
	outcome appv1.SessionTurnOutcome
	cancelReceipt *appv1.RuntimeShutdownReceipt
	notify  chan struct{}
}

func newTurn(sessionID string, ordinal uint64, submissionID, text string, afterTurn uint64) *turnState {
	ctx, cancel := context.WithCancel(context.Background())
	return &turnState{
		sessionID: sessionID, ordinal: ordinal, submissionID: submissionID,
		inputText: text, afterTurn: afterTurn, notify: make(chan struct{}),
		ctx: ctx, cancelFn: cancel,
	}
}

// isCanceled reports whether cancellation was requested for this turn.
func (t *turnState) isCanceled() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.canceled
}

// append commits one event and wakes watchers.
func (t *turnState) append(ev *appv1.SessionTurnEvent) {
	t.mu.Lock()
	t.events = append(t.events, ev)
	close(t.notify)
	t.notify = make(chan struct{})
	t.mu.Unlock()
}

// suffixAfter returns committed events strictly after the cursor.
func (t *turnState) suffixAfter(after string) []*appv1.SessionTurnEvent {
	t.mu.Lock()
	defer t.mu.Unlock()
	if after == "" {
		return append([]*appv1.SessionTurnEvent(nil), t.events...)
	}
	for i, ev := range t.events {
		if ev.EventId == after {
			return append([]*appv1.SessionTurnEvent(nil), t.events[i+1:]...)
		}
	}
	return nil
}

// hasEvent reports whether the cursor resolves to a committed record.
func (t *turnState) hasEvent(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, ev := range t.events {
		if ev.EventId == id {
			return true
		}
	}
	return false
}

func (t *turnState) terminal() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return !t.active && len(t.events) > 0 && t.events[len(t.events)-1].GetOutcome() != appv1.SessionTurnOutcome_SESSION_TURN_OUTCOME_UNSPECIFIED
}

func (t *turnState) waitChan() chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.notify
}

// Host is a minimal AppServer implementation with in-memory journal.
// StreamTurn (the stateless model stream) stays Unimplemented: the GUI path
// uses the Session RPCs (design doc §2).
type Host struct {
	appv1.UnimplementedAppServerServer

	gateway    ChatGateway
	mu          sync.Mutex
	sessionID   string
	maxReq      uint32
	maxMsg      uint32
	maxOut      uint32
	turns       map[uint64]*turnState
	bySubmit    map[string]uint64
	latest      uint64
	activeTurn  uint64
	slow        time.Duration // pacing between text chunks; 0 = instant
}

// New builds a host reporting the given Session binding and limits.
// An empty sessionID means "no Session bound" (clients must treat it as a
// configuration error, design §5.2).
func New(sessionID string) *Host {
	return &Host{
		gateway:   EchoGateway{},
		sessionID: sessionID,
		maxReq:    256 * 1024, maxMsg: 128, maxOut: 1024 * 1024,
		turns: map[uint64]*turnState{}, bySubmit: map[string]uint64{},
	}
}

// SetGateway installs the model gateway (tests / devhost).
func (h *Host) SetGateway(g ChatGateway) { h.gateway = g }

// SetSession binds (or rebinds) the Session ID.
func (h *Host) SetSession(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sessionID = id
}

// SetSlow paces turn execution: one text chunk every d. For cancel/reconnect
// tests. Zero (default) commits the whole turn instantly.
func (h *Host) SetSlow(d time.Duration) { h.slow = d }

// GetCapabilities implements the handshake RPC.
func (h *Host) GetCapabilities(ctx context.Context, req *appv1.GetCapabilitiesRequest) (*appv1.GetCapabilitiesResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	resp := &appv1.GetCapabilitiesResponse{
		MaxRequestBytes:    h.maxReq,
		MaxMessages:        h.maxMsg,
		MaxOutputTextBytes: h.maxOut,
	}
	if h.sessionID != "" {
		resp.SessionId = &h.sessionID
	}
	return resp, nil
}

// StartSessionTurn admits one user turn, mirroring the upstream rules:
// same submission id + same input returns the original turn; same id with
// changed input is ALREADY_EXISTS; a busy session refuses new work
// (FAILED_PRECONDITION); a stale after_turn refuses redispatch.
func (h *Host) StartSessionTurn(ctx context.Context, req *appv1.StartSessionTurnRequest) (*appv1.StartSessionTurnResponse, error) {
	if err := validSubmissionID(req.GetSubmissionId()); err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if req.GetSessionId() != h.sessionID {
		return nil, statusError(codes.InvalidArgument, "unknown session")
	}
	if h.activeTurn != 0 {
		return nil, statusError(codes.FailedPrecondition, "session busy: active turn")
	}
	if ord, ok := h.bySubmit[req.GetSubmissionId()]; ok {
		t := h.turns[ord]
		if t.inputText == req.GetUserText() && t.afterTurn == req.GetAfterTurn() {
			return &appv1.StartSessionTurnResponse{Turn: turnRef(h.sessionID, ord)}, nil
		}
		return nil, statusError(codes.AlreadyExists, "submission id reused with different input")
	}
	if req.GetAfterTurn() != h.latest {
		return nil, statusError(codes.FailedPrecondition, "stale after_turn")
	}

	ordinal := h.latest + 1
	t := newTurn(h.sessionID, ordinal, req.GetSubmissionId(), req.GetUserText(), req.GetAfterTurn())
	t.active = true
	h.turns[ordinal] = t
	h.bySubmit[req.GetSubmissionId()] = ordinal
	h.latest = ordinal
	h.activeTurn = ordinal

	go h.execute(t)
	return &appv1.StartSessionTurnResponse{Turn: turnRef(h.sessionID, ordinal)}, nil
}

// WatchSessionTurn streams the turn's committed suffix from the exclusive
// cursor, waiting while the turn is still active. Cursor must resolve to a
// committed record.
func (h *Host) WatchSessionTurn(req *appv1.WatchSessionTurnRequest, stream appv1.AppServer_WatchSessionTurnServer) error {
	t := h.turnOf(req.GetTurn())
	if t == nil {
		return statusError(codes.NotFound, "unknown turn")
	}
	after := req.GetAfterEventId() // nil-safe: optional field
	if after != "" && !t.hasEvent(after) {
		return statusError(codes.InvalidArgument, "invalid cursor: not a committed event")
	}
	for {
		for _, ev := range t.suffixAfter(after) {
			if stream.Context().Err() != nil {
				return stream.Context().Err()
			}
			if err := stream.Send(ev); err != nil {
				return err
			}
			after = ev.EventId
		}
		if t.terminal() {
			return nil
		}
		ch := t.waitChan()
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case <-ch:
		}
	}
}

// CancelSessionTurn stops the local task scope and reports the receipt.
// A completion that already committed stays committed (fake: execution
// checks the cancel flag between chunks).
func (h *Host) CancelSessionTurn(ctx context.Context, req *appv1.CancelSessionTurnRequest) (*appv1.CancelSessionTurnResponse, error) {
	t := h.turnOf(req.GetTurn())
	if t == nil {
		return nil, statusError(codes.NotFound, "unknown turn")
	}
	t.mu.Lock()
	wasActive := t.active
	if wasActive {
		t.canceled = true
	}
	t.mu.Unlock()
	if wasActive {
		t.cancelFn() // drop the in-flight provider stream
	}

	// Wait for the local scope to retire (execute honors the flag).
	deadline := time.Now().Add(2 * time.Second)
	for {
		t.mu.Lock()
		done := !t.active
		t.mu.Unlock()
		if done {
			break
		}
		select {
		case <-ctx.Done():
			return nil, statusError(codes.DeadlineExceeded, "retirement wait timed out")
		case <-t.waitChan():
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return nil, statusError(codes.DeadlineExceeded, "retirement wait timed out")
		}
	}

	t.mu.Lock()
	receipt := &appv1.RuntimeShutdownReceipt{
		Completed:            true,
		CancellationRequested: wasActive,
		TrackerClosed:        true,
		RemainingTasks:       0,
		Status:               appv1.RuntimeRetirementStatus_RUNTIME_RETIREMENT_STATUS_COMPLETED,
	}
	t.cancelReceipt = receipt
	outcome := t.outcome
	t.mu.Unlock()
	return &appv1.CancelSessionTurnResponse{Turn: turnRef(t.sessionID, t.ordinal), Runtime: receipt, Outcome: outcome}, nil
}

// GetSessionTurnCancellation reads recorded cancellation evidence.
func (h *Host) GetSessionTurnCancellation(ctx context.Context, req *appv1.GetSessionTurnCancellationRequest) (*appv1.GetSessionTurnCancellationResponse, error) {
	t := h.turnOf(req.GetTurn())
	if t == nil {
		return nil, statusError(codes.NotFound, "unknown turn")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	resp := &appv1.GetSessionTurnCancellationResponse{Turn: turnRef(t.sessionID, t.ordinal), Outcome: t.outcome}
	if t.cancelReceipt != nil {
		resp.Evidence = &appv1.GetSessionTurnCancellationResponse_Recorded{Recorded: t.cancelReceipt}
	} else {
		resp.Evidence = &appv1.GetSessionTurnCancellationResponse_Unavailable{Unavailable: &appv1.CancellationEvidenceUnavailable{}}
	}
	return resp, nil
}

// GetSessionSummary reads the latest committed turn and its recovery state.
func (h *Host) GetSessionSummary(ctx context.Context, req *appv1.GetSessionSummaryRequest) (*appv1.GetSessionSummaryResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	resp := &appv1.GetSessionSummaryResponse{RecoveryState: appv1.SessionRecoveryState_SESSION_RECOVERY_STATE_EMPTY}
	if h.latest == 0 {
		return resp, nil
	}
	resp.LatestTurn = turnRef(h.sessionID, h.latest)
	if h.activeTurn == h.latest {
		resp.RecoveryState = appv1.SessionRecoveryState_SESSION_RECOVERY_STATE_ACTIVE
	} else {
		resp.RecoveryState = appv1.SessionRecoveryState_SESSION_RECOVERY_STATE_FINISHED
	}
	return resp, nil
}

// execute streams one turn through the model gateway: Started, streamed text
// checkpoints, then the committed outcome. Cancellation drops the in-flight
// provider stream (REMOTE_UNCERTAIN); a gateway failure with zero committed
// text commits a pre-dispatch refusal.
func (h *Host) execute(t *turnState) {
	defer t.cancelFn() // release gateway resources at turn retirement
	t.append(&appv1.SessionTurnEvent{EventId: t.nextID(), Event: &appv1.SessionTurnEvent_Started{Started: &appv1.TurnStarted{}}})

	gateway := h.gateway
	if gateway == nil {
		gateway = EchoGateway{}
	}
	chunks := 0
	err := gateway.StreamTurn(t.ctx, h.historyFor(t.ordinal), func(chunk string) error {
		if t.isCanceled() {
			return errTurnCanceled
		}
		if h.slow > 0 {
			time.Sleep(h.slow)
		}
		t.append(&appv1.SessionTurnEvent{
			EventId: t.nextID(),
			Event:   &appv1.SessionTurnEvent_TextCheckpoint{TextCheckpoint: &appv1.TextCheckpoint{Text: chunk}},
		})
		chunks++
		return nil
	})

	outcome := appv1.SessionTurnOutcome_SESSION_TURN_OUTCOME_COMPLETED
	switch {
	case err == nil && !t.isCanceled():
		// A completion that wins the race against cancellation commits.
	case errors.Is(err, errTurnCanceled) || t.isCanceled():
		outcome = appv1.SessionTurnOutcome_SESSION_TURN_OUTCOME_REMOTE_UNCERTAIN
	case chunks == 0:
		outcome = appv1.SessionTurnOutcome_SESSION_TURN_OUTCOME_REJECTED_BEFORE_DISPATCH
	default:
		// Provider failed after dispatch: keep the partial text, report that
		// the provider's final state is unknown.
		outcome = appv1.SessionTurnOutcome_SESSION_TURN_OUTCOME_REMOTE_UNCERTAIN
	}
	t.append(&appv1.SessionTurnEvent{EventId: t.nextID(), Event: &appv1.SessionTurnEvent_Outcome{Outcome: outcome}})

	t.mu.Lock()
	t.active = false
	t.outcome = outcome
	t.mu.Unlock()

	h.mu.Lock()
	if h.activeTurn == t.ordinal {
		h.activeTurn = 0
	}
	h.mu.Unlock()
}

// nextID mints an opaque per-turn event ID.
func (t *turnState) nextID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return fmt.Sprintf("e%d.%d", t.ordinal, len(t.events)+1)
}

func (h *Host) turnOf(ref *appv1.SessionTurnRef) *turnState {
	if ref == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if ref.SessionId != h.sessionID {
		return nil
	}
	return h.turns[ref.Ordinal]
}

// historyFor assembles the model conversation from the journal: every
// committed turn up to and including the running one, user text followed by
// the assistant's committed text (route/provider credentials stay host-side,
// design doc §1).
func (h *Host) historyFor(ordinal uint64) []ChatMessage {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []ChatMessage
	for i := uint64(1); i <= ordinal; i++ {
		t := h.turns[i]
		if t == nil {
			continue
		}
		out = append(out, ChatMessage{Role: "user", Text: t.inputText})
		if at := t.assistantText(); at != "" {
			out = append(out, ChatMessage{Role: "assistant", Text: at})
		}
	}
	return out
}

// assistantText concatenates the committed text checkpoints.
func (t *turnState) assistantText() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var b strings.Builder
	for _, ev := range t.events {
		if tc := ev.GetTextCheckpoint(); tc != nil {
			b.WriteString(tc.GetText())
		}
	}
	return b.String()
}

func turnRef(sessionID string, ordinal uint64) *appv1.SessionTurnRef {
	return &appv1.SessionTurnRef{SessionId: sessionID, Ordinal: ordinal}
}

func validSubmissionID(id string) error {
	if id == "" || len(id) > 128 {
		return statusError(codes.InvalidArgument, "submission_id must be 1-128 chars")
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return statusError(codes.InvalidArgument, "submission_id allows ASCII letters, digits, hyphens, underscores")
		}
	}
	return nil
}
