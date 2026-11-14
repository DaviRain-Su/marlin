package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
// temp file, rename over the target). Each Session owns a projection file
// (sessions/<id>.json under the store directory); missing or corrupted
// files start empty — the journal on the host remains the source of truth.
type Store struct {
	dir  string
	mu   sync.Mutex
	data storeData
}

// LoadStore prepares the store directory; a legacy single-file projection
// (session.json) migrates into its per-session file on first load.
func LoadStore(dir string) *Store {
	s := &Store{dir: dir}
	legacy := filepath.Join(dir, "session.json")
	if b, err := os.ReadFile(legacy); err == nil {
		var old storeData
		if json.Unmarshal(b, &old) == nil && old.SessionID != "" {
			if _, err := os.Stat(s.sessionPath(old.SessionID)); os.IsNotExist(err) {
				_ = os.MkdirAll(filepath.Join(dir, "sessions"), 0o700)
				_ = os.WriteFile(s.sessionPath(old.SessionID), b, 0o600)
			}
		}
	}
	return s
}

func (s *Store) sessionPath(id string) string {
	return filepath.Join(s.dir, "sessions", id+".json")
}

// SessionID returns the Session bound in memory (the one being viewed).
func (s *Store) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.SessionID
}

// BindSession switches the viewed Session: the current projection is already
// persisted, and the target's file (or an empty view) loads in its place.
func (s *Store) BindSession(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data := storeData{SessionID: id}
	if b, err := os.ReadFile(s.sessionPath(id)); err == nil {
		_ = json.Unmarshal(b, &data) // malformed JSON: the view starts empty
	}
	s.data = data
	return nil
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
	if s.data.SessionID == "" {
		return fmt.Errorf("store: no session bound")
	}
	if err := os.MkdirAll(filepath.Join(s.dir, "sessions"), 0o700); err != nil {
		return fmt.Errorf("store: mkdir: %w", err)
	}
	path := s.sessionPath(s.data.SessionID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("store: write: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("store: rename: %w", err)
	}
	return nil
}
