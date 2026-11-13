package app

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// StoredTurn is one turn in the local projection. Ordinal travels as a
// string (u64 over JSON, design doc §4).
type StoredTurn struct {
	Ordinal       uint64 `json:"ordinal,string"`
	UserText      string `json:"userText"`
	AssistantText string `json:"assistantText,omitempty"`
	Outcome       string `json:"outcome,omitempty"`
	// LastEventID is the applied cursor of this turn (design §6.4: the
	// projection and its cursor are persisted together, as one record).
	LastEventID string `json:"lastEventId,omitempty"`
}

// PendingSubmission survives a StartSessionTurn whose response was lost.
type PendingSubmission struct {
	SubmissionID string `json:"submissionId"`
	Text         string `json:"text"`
	AfterTurn    uint64 `json:"afterTurn,string"`
}

type storeData struct {
	SessionID string             `json:"sessionId"`
	Turns     []StoredTurn       `json:"turns,omitempty"`
	Pending   *PendingSubmission `json:"pending,omitempty"`
}

// Store persists the applied projection plus cursor atomically (write to a
// temp file, rename over the target). Missing or corrupted files start
// empty; the journal on the host remains the source of truth.
type Store struct {
	path string
	mu   sync.Mutex
	data storeData
}

// LoadStore reads the projection file; a missing or unreadable file yields
// an empty store (never an error — the GUI must always open).
func LoadStore(path string) *Store {
	s := &Store{path: path}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &s.data) // malformed JSON: data stays zero
	}
	return s
}

// SessionID returns the persisted Session binding (empty before first save).
func (s *Store) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.SessionID
}

// BindSession stores the Session ID; a DIFFERENT ID clears the projection
// (transcripts of another Session must not leak into this view).
func (s *Store) BindSession(id string) error {
	s.mu.Lock()
	if s.data.SessionID == id {
		s.mu.Unlock()
		return nil
	}
	s.data = storeData{SessionID: id}
	s.mu.Unlock()
	return s.Save()
}

// Turns returns a copy of the projection.
func (s *Store) Turns() []StoredTurn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]StoredTurn(nil), s.data.Turns...)
}

// Pending returns the unresolved submission, if any.
func (s *Store) Pending() *PendingSubmission {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.Pending == nil {
		return nil
	}
	p := *s.data.Pending
	return &p
}

// SetPending records a submission before its request goes out.
func (s *Store) SetPending(p PendingSubmission) error {
	s.mu.Lock()
	s.data.Pending = &p
	s.mu.Unlock()
	return s.Save()
}

// ClearPending drops the record once admission is resolved.
func (s *Store) ClearPending() error {
	s.mu.Lock()
	s.data.Pending = nil
	s.mu.Unlock()
	return s.Save()
}

// UpsertTurn appends or replaces the row for turn.Ordinal and persists the
// projection + cursor as one record.
func (s *Store) UpsertTurn(t StoredTurn) error {
	s.mu.Lock()
	replaced := false
	for i := range s.data.Turns {
		if s.data.Turns[i].Ordinal == t.Ordinal {
			s.data.Turns[i] = t
			replaced = true
			break
		}
	}
	if !replaced {
		s.data.Turns = append(s.data.Turns, t)
	}
	s.mu.Unlock()
	return s.Save()
}

// Save writes the store atomically: temp file in the same directory, fsync,
// rename. A crash mid-save leaves either the old or the new file, never a
// torn projection/cursor pair.
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("store: marshal: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("store: write: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("store: rename: %w", err)
	}
	return nil
}
