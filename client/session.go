package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	appv1 "marlin-desktop/gen/marlin/app/v1"
)

// TurnRef identifies one admitted turn (opaque; never constructed by hand).
type TurnRef struct {
	SessionID string
	Ordinal   uint64
}

// EventKind discriminates SessionTurnEvent payloads (design §6.4).
type EventKind int

const (
	EventStarted EventKind = iota
	EventText
	EventOutcome
)

// SessionEvent is one visible event of a turn, as delivered to the view.
type SessionEvent struct {
	ID      string
	Kind    EventKind
	Text    string // EventText only
	Outcome appv1.SessionTurnOutcome
}

// Summary is the result of GetSessionSummary (design §6.6 startup recovery).
type Summary struct {
	Latest        *TurnRef
	RecoveryState appv1.SessionRecoveryState
}

// CancelReceipt projects the RuntimeShutdownReceipt plus committed outcome
// (design §6.5).
type CancelReceipt struct {
	Completed             bool
	CancellationRequested bool
	RemainingTasks        uint64
	TimedOut              bool // RUNTIME_RETIREMENT_STATUS_TIMED_OUT
	Status                appv1.RuntimeRetirementStatus
	Outcome               appv1.SessionTurnOutcome
}

// SessionManager owns Session semantics over one connection: submission
// retry, ordered event replay with reconnect, cancellation. It holds the
// client-side after_turn watermark and no UI state (design §6).
type SessionManager struct {
	conn      *Conn
	sessionID string

	mu        sync.Mutex
	afterTurn uint64
	// records binds each submission to its ORIGINAL text and after_turn,
	// permanently while the manager lives: a retry of a committed send
	// replays exactly those fields even if the watermark advanced (design
	// §6.3 "Retrying a committed submission returns its original turn").
	records map[string]pendingSubmission
}

type pendingSubmission struct {
	text      string
	afterTurn uint64
}

// NewSessionManager binds a manager to the host's Session binding.
func NewSessionManager(conn *Conn, sessionID string) *SessionManager {
	return &SessionManager{conn: conn, sessionID: sessionID, records: map[string]pendingSubmission{}}
}

// WithSession returns a manager for another Session over the same
// connection. The watermark starts at zero — call SyncFromSummary to
// inherit that Session's committed turns (the switch/restart path).
func (m *SessionManager) WithSession(sessionID string) *SessionManager {
	return NewSessionManager(m.conn, sessionID)
}

// SessionID is the host-bound Session this manager addresses.
func (m *SessionManager) SessionID() string { return m.sessionID }

// AfterTurn is the last observed turn ordinal (client watermark).
func (m *SessionManager) AfterTurn() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.afterTurn
}

// Send submits one user turn at the current watermark. Transient failures
// (host unreachable, lost admission race) retry the EXACT request with the
// same submission_id — idempotent by contract (design §6.3). The caller
// persists the draft and submission_id before calling; a refusal keeps that
// record intact.
func (m *SessionManager) Send(ctx context.Context, submissionID, userText string) (TurnRef, error) {
	return m.SendExact(ctx, submissionID, userText, m.AfterTurn())
}

// SendExact is Send with an explicit after_turn — used to resume a pending
// submission whose ORIGINAL after_turn is the idempotency key's other half
// (design §6.3: a retry keeps the original fields even if the view advanced).
func (m *SessionManager) SendExact(ctx context.Context, submissionID, userText string, afterTurn uint64) (TurnRef, error) {
	c := appv1.NewAppServerClient(m.conn.GRPC())

	// Bind this submission to its original inputs for every later retry.
	m.mu.Lock()
	p, retrying := m.records[submissionID]
	if !retrying {
		p = pendingSubmission{text: userText, afterTurn: afterTurn}
		m.records[submissionID] = p
	}
	m.mu.Unlock()

	req := &appv1.StartSessionTurnRequest{
		SessionId:    m.sessionID,
		UserText:     p.text,
		AfterTurn:    p.afterTurn,
		SubmissionId: submissionID,
	}
	backoff := 100 * time.Millisecond
	calibrations := 0
	for {
		resp, err := c.StartSessionTurn(ctx, req)
		if err == nil {
			ref := TurnRef{SessionID: resp.GetTurn().GetSessionId(), Ordinal: resp.GetTurn().GetOrdinal()}
			m.mu.Lock()
			if ref.Ordinal > m.afterTurn {
				m.afterTurn = ref.Ordinal
			}
			m.mu.Unlock()
			return ref, nil
		}
		lastErr := classify(err, "StartSessionTurn")
		kind, _ := AsErrKind(lastErr)
		switch kind {
		case ErrHostUnavailable, ErrConcurrentAdmission:
			// Retry the exact request: same id, same text, same after_turn.
		case ErrPrecondition:
			// FAILED_PRECONDITION covers stale after_turn AND a busy/unresolved
			// session. Calibrate from the summary; only retry when the refusal
			// was the ordinal (design §6.3). This also recovers from another
			// client committing a turn meanwhile (the acceptance scenario).
			if calibrations >= 2 {
				return TurnRef{}, lastErr
			}
			s, serr := m.Summary(ctx)
			if serr != nil {
				return TurnRef{}, serr
			}
			switch s.RecoveryState {
			case appv1.SessionRecoveryState_SESSION_RECOVERY_STATE_ACTIVE:
				return TurnRef{}, fmt.Errorf("session busy: %w", lastErr)
			case appv1.SessionRecoveryState_SESSION_RECOVERY_STATE_UNRESOLVED,
				appv1.SessionRecoveryState_SESSION_RECOVERY_STATE_TOOL_CONTINUATION_PENDING:
				return TurnRef{}, fmt.Errorf("session unresolved: %w", lastErr)
			}
			if s.Latest != nil {
				m.mu.Lock()
				if s.Latest.Ordinal > m.afterTurn {
					m.afterTurn = s.Latest.Ordinal
				}
				// This submission was never admitted: rebind it to the fresh
				// watermark. A committed submission cannot reach this path
				// (idempotent replays return the original turn instead).
				m.records[submissionID] = pendingSubmission{text: p.text, afterTurn: m.afterTurn}
				req.AfterTurn = m.afterTurn
				m.mu.Unlock()
			}
			calibrations++
			continue
		default:
			// Keep the record: the caller may inspect the refusal and retry
			// later (e.g. limit refusal).
			return TurnRef{}, lastErr
		}
		select {
		case <-ctx.Done():
			return TurnRef{}, ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 2*time.Second {
			backoff *= 2
		}
	}
}

// Follow streams one turn's visible events into ch, reconnecting with the
// event cursor until the terminal Outcome arrives (design §6.4). The cursor
// advances per delivered event; forwarding to ch is synchronous, so receipt
// equals apply in this M2 wiring — persisting projection+cursor together
// atomically is M3 work.
func (m *SessionManager) Follow(ctx context.Context, turn TurnRef, ch chan<- SessionEvent) error {
	if turn.SessionID != m.sessionID {
		return fmt.Errorf("client: turn %v does not belong to session %s", turn, m.sessionID)
	}
	c := appv1.NewAppServerClient(m.conn.GRPC())
	seen := map[string]bool{}
	backoff := 200 * time.Millisecond
	after := "" // exclusive cursor within this turn

	for {
		req := &appv1.WatchSessionTurnRequest{Turn: &appv1.SessionTurnRef{SessionId: turn.SessionID, Ordinal: turn.Ordinal}}
		if after != "" {
			a := after
			req.AfterEventId = &a
		}
		stream, err := c.WatchSessionTurn(ctx, req)
		if err != nil {
			if cerr := recoverOrReturn(ctx, classify(err, "WatchSessionTurn"), &backoff); cerr != nil {
				return cerr
			}
			continue
		}
		for {
			ev, err := stream.Recv()
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if err := recoverOrReturn(ctx, classify(err, "WatchSessionTurn"), &backoff); err != nil {
					return err
				}
				break
			}
			if seen[ev.GetEventId()] {
				continue
			}
			seen[ev.GetEventId()] = true
			se, terminal := toSessionEvent(ev)
			if se != nil {
				select {
				case ch <- *se:
				case <-ctx.Done():
					return ctx.Err()
				}
				after = ev.GetEventId()
				if terminal {
					return nil
				}
			}
		}
		// Stream ended (EOF or error). Reconnect unless ctx is done.
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

// recoverOrReturn: nil means "retry after backoff"; non-nil returns the error.
func recoverOrReturn(ctx context.Context, err error, backoff *time.Duration) error {
	if err == nil {
		return nil
	}
	kind, _ := AsErrKind(err)
	if errors.Is(err, io.EOF) {
		kind = ErrCorruptStream
	}
	switch kind {
	case ErrHostUnavailable, ErrCorruptStream:
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(*backoff):
		}
		if *backoff < 2*time.Second {
			*backoff *= 2
		}
		return nil
	default:
		return err
	}
}

// Cancel requests cancellation and waits for the local receipt (design §6.5).
func (m *SessionManager) Cancel(ctx context.Context, turn TurnRef) (CancelReceipt, error) {
	c := appv1.NewAppServerClient(m.conn.GRPC())
	resp, err := c.CancelSessionTurn(ctx, &appv1.CancelSessionTurnRequest{
		Turn: &appv1.SessionTurnRef{SessionId: turn.SessionID, Ordinal: turn.Ordinal},
	})
	if err != nil {
		return CancelReceipt{}, classify(err, "CancelSessionTurn")
	}
	r := resp.GetRuntime()
	return CancelReceipt{
		Completed:             r.GetCompleted(),
		CancellationRequested: r.GetCancellationRequested(),
		RemainingTasks:        r.GetRemainingTasks(),
		TimedOut:              r.GetStatus() == appv1.RuntimeRetirementStatus_RUNTIME_RETIREMENT_STATUS_TIMED_OUT,
		Status:                r.GetStatus(),
		Outcome:               resp.GetOutcome(),
	}, nil
}

// CancellationEvidence reads recorded cancellation evidence after reconnect
// or restart (design §6.5). HasReceipt is false when evidence is unavailable.
func (m *SessionManager) CancellationEvidence(ctx context.Context, turn TurnRef) (CancelReceipt, bool, error) {
	c := appv1.NewAppServerClient(m.conn.GRPC())
	resp, err := c.GetSessionTurnCancellation(ctx, &appv1.GetSessionTurnCancellationRequest{
		Turn: &appv1.SessionTurnRef{SessionId: turn.SessionID, Ordinal: turn.Ordinal},
	})
	if err != nil {
		return CancelReceipt{}, false, classify(err, "GetSessionTurnCancellation")
	}
	switch ev := resp.GetEvidence().(type) {
	case *appv1.GetSessionTurnCancellationResponse_Recorded:
		r := ev.Recorded
		return CancelReceipt{
			Completed:             r.GetCompleted(),
			CancellationRequested: r.GetCancellationRequested(),
			RemainingTasks:        r.GetRemainingTasks(),
			TimedOut:              r.GetStatus() == appv1.RuntimeRetirementStatus_RUNTIME_RETIREMENT_STATUS_TIMED_OUT,
			Status:                r.GetStatus(),
			Outcome:               resp.GetOutcome(),
		}, true, nil
	default:
		return CancelReceipt{Outcome: resp.GetOutcome()}, false, nil
	}
}

// SyncFromSummary re-reads the host's latest committed turn and advances the
// watermark. Used at startup/reconnect so the watermark survives manager
// recreation (the persisted view is M3 work, design §6.6).
func (m *SessionManager) SyncFromSummary(ctx context.Context) error {
	s, err := m.Summary(ctx)
	if err != nil {
		return err
	}
	if s.Latest != nil {
		m.mu.Lock()
		if s.Latest.Ordinal > m.afterTurn {
			m.afterTurn = s.Latest.Ordinal
		}
		m.mu.Unlock()
	}
	return nil
}

// Summary reads the latest committed turn (design §6.6).
func (m *SessionManager) Summary(ctx context.Context) (Summary, error) {
	c := appv1.NewAppServerClient(m.conn.GRPC())
	resp, err := c.GetSessionSummary(ctx, &appv1.GetSessionSummaryRequest{SessionId: m.sessionID})
	if err != nil {
		return Summary{}, classify(err, "GetSessionSummary")
	}
	s := Summary{RecoveryState: resp.GetRecoveryState()}
	if resp.GetLatestTurn() != nil {
		s.Latest = &TurnRef{SessionID: resp.GetLatestTurn().GetSessionId(), Ordinal: resp.GetLatestTurn().GetOrdinal()}
	}
	return s, nil
}

func toSessionEvent(ev *appv1.SessionTurnEvent) (*SessionEvent, bool) {
	switch e := ev.GetEvent().(type) {
	case *appv1.SessionTurnEvent_Started:
		return &SessionEvent{ID: ev.GetEventId(), Kind: EventStarted}, false
	case *appv1.SessionTurnEvent_TextCheckpoint:
		return &SessionEvent{ID: ev.GetEventId(), Kind: EventText, Text: e.TextCheckpoint.GetText()}, false
	case *appv1.SessionTurnEvent_Outcome:
		return &SessionEvent{ID: ev.GetEventId(), Kind: EventOutcome, Outcome: e.Outcome}, true
	default:
		return nil, false
	}
}
