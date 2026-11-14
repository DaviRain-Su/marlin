package app

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SessionMeta is one entry in the conversation list the sidebar renders.
type SessionMeta struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// UpdatedAt is unix millis as a string (u64 over JSON).
	UpdatedAt string `json:"updatedAt,omitempty"`
}

// sessionRegistryFile is the persisted shape of the registry.
type sessionRegistryFile struct {
	Current string        `json:"current"`
	Items   []SessionMeta `json:"items"`
}

// SessionRegistry owns the conversation list and which session is open.
// It persists next to the per-session projections (registry.json); the
// projections live in sessions/<id>.json beside it.
type SessionRegistry struct {
	path string
	mu   sync.Mutex
	data sessionRegistryFile
}

// LoadSessionRegistry reads the registry; a missing file seeds one session
// ("gui-session") so existing journals and fresh installs both open onto a
// usable conversation.
func LoadSessionRegistry(path string) *SessionRegistry {
	r := &SessionRegistry{path: path}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &r.data) // malformed JSON: data stays zero
	}
	if len(r.data.Items) == 0 {
		r.data.Items = []SessionMeta{{ID: "gui-session", Title: "新对话"}}
	}
	if r.data.Current == "" {
		r.data.Current = r.data.Items[0].ID
	}
	return r
}

// Current returns the open Session ID (never empty).
func (r *SessionRegistry) Current() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.data.Current
}

// List returns a copy of the conversation list, newest first.
func (r *SessionRegistry) List() []SessionMeta {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]SessionMeta(nil), r.data.Items...)
}

// SetCurrent opens another existing session.
func (r *SessionRegistry) SetCurrent(id string) error {
	r.mu.Lock()
	if !r.hasLocked(id) {
		r.mu.Unlock()
		return fmt.Errorf("session registry: unknown session %q", id)
	}
	r.data.Current = id
	r.mu.Unlock()
	return r.Save()
}

// NewSession mints a session id, puts it at the top of the list and opens
// it. IDs are client-minted by design (the real host resolves any id
// through its session lookup).
func (r *SessionRegistry) NewSession() (SessionMeta, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return SessionMeta{}, fmt.Errorf("session registry: mint id: %w", err)
	}
	meta := SessionMeta{ID: "s-" + hex.EncodeToString(b[:]), Title: "新对话"}
	r.mu.Lock()
	r.data.Items = append([]SessionMeta{meta}, r.data.Items...)
	r.data.Current = meta.ID
	r.mu.Unlock()
	if err := r.Save(); err != nil {
		return SessionMeta{}, err
	}
	return meta, nil
}

// Touch updates a session's title (first user text) and recency.
func (r *SessionRegistry) Touch(id, title string) {
	r.mu.Lock()
	for i := range r.data.Items {
		if r.data.Items[i].ID != id {
			continue
		}
		if r.data.Items[i].Title == "" || r.data.Items[i].Title == "新对话" {
			if t := truncateTitle(title); t != "" {
				r.data.Items[i].Title = t
			}
		}
		r.data.Items[i].UpdatedAt = fmt.Sprintf("%d", time.Now().UnixMilli())
		break
	}
	r.mu.Unlock()
	_ = r.Save()
}

// Rename sets a session's display title.
func (r *SessionRegistry) Rename(id, title string) error {
	title = truncateTitle(title)
	if title == "" {
		return fmt.Errorf("session registry: empty title")
	}
	r.mu.Lock()
	if !r.hasLocked(id) {
		r.mu.Unlock()
		return fmt.Errorf("session registry: unknown session %q", id)
	}
	for i := range r.data.Items {
		if r.data.Items[i].ID == id {
			r.data.Items[i].Title = title
			break
		}
	}
	r.mu.Unlock()
	return r.Save()
}

// Delete removes a session from the list. The current session cannot be
// deleted (switch first); the host journal copy stays untouched.
func (r *SessionRegistry) Delete(id string) error {
	r.mu.Lock()
	if r.data.Current == id {
		r.mu.Unlock()
		return fmt.Errorf("session registry: cannot delete the open session")
	}
	kept := r.data.Items[:0]
	found := false
	for _, it := range r.data.Items {
		if it.ID == id {
			found = true
			continue
		}
		kept = append(kept, it)
	}
	if !found {
		r.mu.Unlock()
		return fmt.Errorf("session registry: unknown session %q", id)
	}
	r.data.Items = kept
	r.mu.Unlock()
	return r.Save()
}

func (r *SessionRegistry) hasLocked(id string) bool {
	for _, it := range r.data.Items {
		if it.ID == id {
			return true
		}
	}
	return false
}

func truncateTitle(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = strings.Join(strings.Fields(s), " ")
	runes := []rune(s)
	if len(runes) > 40 {
		return string(runes[:40]) + "…"
	}
	return s
}

// Save persists the registry atomically (temp + rename, like the store).
func (r *SessionRegistry) Save() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, err := json.MarshalIndent(r.data, "", "  ")
	if err != nil {
		return fmt.Errorf("session registry: marshal: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return fmt.Errorf("session registry: mkdir: %w", err)
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("session registry: write: %w", err)
	}
	if err := os.Rename(tmp, r.path); err != nil {
		return fmt.Errorf("session registry: rename: %w", err)
	}
	return nil
}
