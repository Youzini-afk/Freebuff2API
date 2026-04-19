package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ManagedToken is a persisted auth token record managed via the admin WebUI.
type ManagedToken struct {
	ID        string    `json:"id"`
	Label     string    `json:"label"`
	Token     string    `json:"token"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// storeFile is the on-disk representation of the token store.
type storeFile struct {
	Version int            `json:"version"`
	Tokens  []ManagedToken `json:"tokens"`
}

// TokenStore persists managed tokens to disk and provides CRUD operations.
type TokenStore struct {
	path string

	mu     sync.RWMutex
	tokens []ManagedToken
}

// NewTokenStore returns a store backed by the given JSON file. The file and
// its parent directory are created automatically. If the file is missing or
// empty, seedTokens are imported on first run (typically from the
// AUTH_TOKENS env var / config file).
func NewTokenStore(path string, seedTokens []string) (*TokenStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("token store path is required")
	}
	dir := filepath.Dir(path)
	if dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create token store dir: %w", err)
		}
	}

	store := &TokenStore{path: path}

	loaded, err := store.loadFromDisk()
	if err != nil {
		return nil, err
	}

	if !loaded && len(seedTokens) > 0 {
		now := time.Now().UTC()
		for index, raw := range seedTokens {
			value := strings.TrimSpace(raw)
			if value == "" {
				continue
			}
			store.tokens = append(store.tokens, ManagedToken{
				ID:        newTokenID(),
				Label:     fmt.Sprintf("token-%d", index+1),
				Token:     value,
				Enabled:   true,
				CreatedAt: now,
				UpdatedAt: now,
			})
		}
		if err := store.persistLocked(); err != nil {
			return nil, err
		}
	}

	return store, nil
}

func (s *TokenStore) loadFromDisk() (bool, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("read token store: %w", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return false, nil
	}

	var file storeFile
	if err := json.Unmarshal(data, &file); err != nil {
		return false, fmt.Errorf("parse token store: %w", err)
	}
	s.tokens = file.Tokens
	return true, nil
}

// Path returns the on-disk file path backing this store.
func (s *TokenStore) Path() string { return s.path }

// List returns a copy of all persisted tokens in deterministic order.
func (s *TokenStore) List() []ManagedToken {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ManagedToken, len(s.tokens))
	copy(out, s.tokens)
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// Enabled returns only enabled tokens.
func (s *TokenStore) Enabled() []ManagedToken {
	all := s.List()
	out := make([]ManagedToken, 0, len(all))
	for _, t := range all {
		if t.Enabled && strings.TrimSpace(t.Token) != "" {
			out = append(out, t)
		}
	}
	return out
}

// Get returns the token record matching id, or false.
func (s *TokenStore) Get(id string) (ManagedToken, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, t := range s.tokens {
		if t.ID == id {
			return t, true
		}
	}
	return ManagedToken{}, false
}

// Create inserts a new token. Duplicate token values are rejected.
func (s *TokenStore) Create(label, token string) (ManagedToken, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return ManagedToken{}, errors.New("token value is required")
	}
	label = strings.TrimSpace(label)

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, existing := range s.tokens {
		if existing.Token == token {
			return ManagedToken{}, errors.New("token already exists")
		}
	}

	now := time.Now().UTC()
	if label == "" {
		label = fmt.Sprintf("token-%d", len(s.tokens)+1)
	}
	record := ManagedToken{
		ID:        newTokenID(),
		Label:     label,
		Token:     token,
		Enabled:   true,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.tokens = append(s.tokens, record)
	if err := s.persistLocked(); err != nil {
		// Roll back the in-memory append so callers see a consistent state.
		s.tokens = s.tokens[:len(s.tokens)-1]
		return ManagedToken{}, err
	}
	return record, nil
}

// Update modifies mutable fields. Only non-nil pointers are applied.
func (s *TokenStore) Update(id string, label *string, enabled *bool) (ManagedToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.tokens {
		if s.tokens[i].ID != id {
			continue
		}
		if label != nil {
			trimmed := strings.TrimSpace(*label)
			if trimmed != "" {
				s.tokens[i].Label = trimmed
			}
		}
		if enabled != nil {
			s.tokens[i].Enabled = *enabled
		}
		s.tokens[i].UpdatedAt = time.Now().UTC()
		updated := s.tokens[i]
		if err := s.persistLocked(); err != nil {
			return ManagedToken{}, err
		}
		return updated, nil
	}
	return ManagedToken{}, errors.New("token not found")
}

// Delete removes a token record by ID.
func (s *TokenStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.tokens {
		if s.tokens[i].ID != id {
			continue
		}
		s.tokens = append(s.tokens[:i], s.tokens[i+1:]...)
		return s.persistLocked()
	}
	return errors.New("token not found")
}

// persistLocked writes the current in-memory state to disk. Caller must hold
// the write lock.
func (s *TokenStore) persistLocked() error {
	file := storeFile{Version: 1, Tokens: s.tokens}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("encode token store: %w", err)
	}

	tmpPath := s.path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return fmt.Errorf("write token store: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("commit token store: %w", err)
	}
	return nil
}

func newTokenID() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		// Fall back to a time-based ID; should almost never happen.
		return fmt.Sprintf("tok_%d", time.Now().UnixNano())
	}
	return "tok_" + hex.EncodeToString(buf)
}

// MaskToken returns a safe-to-display version of the raw token value.
func MaskToken(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	if len(token) <= 8 {
		return strings.Repeat("*", len(token))
	}
	return token[:4] + strings.Repeat("*", len(token)-8) + token[len(token)-4:]
}
