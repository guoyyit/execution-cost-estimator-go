// Package persist durably stores the last committed depth positions and
// plans so the service can resume after restart without drift.
//
// Every save writes a brand-new temp file and renames it over the state
// file, so readers always observe one complete commit, never a torn
// write.
package persist

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/guoyyit/execution-cost-estimator-go/model"
)

// DepthPosition records the last applied update per symbol.
type DepthPosition struct {
	Symbol  string `json:"symbol"`
	LastSeq int64  `json:"last_seq"`
	Stale   bool   `json:"stale"`
}

// State is the complete durable state.
type State struct {
	DepthPositions map[string]DepthPosition `json:"depth_positions"`
	Plans          []*model.Plan            `json:"plans"`
	Version        int                      `json:"version"`
}

// Store atomically persists State snapshots as JSON.
type Store struct {
	mu   sync.Mutex
	path string
}

// NewStore creates/opens a JSON file store at path.
func NewStore(path string) *Store { return &Store{path: path} }

// Save atomically writes a state snapshot and bumps its version.
func (s *Store) Save(st *State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st == nil {
		return errors.New("nil state")
	}
	st.Version++
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("persist: mkdir: %w", err)
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("persist: marshal: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("persist: write tmp: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("persist: rename: %w", err)
	}
	return nil
}

// Load reads the latest committed state. A missing file yields an empty
// state (fresh start) rather than an error.
func (s *Store) Load() (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return &State{DepthPositions: map[string]DepthPosition{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("persist: read: %w", err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("persist: unmarshal (state file corrupt): %w", err)
	}
	if st.DepthPositions == nil {
		st.DepthPositions = map[string]DepthPosition{}
	}
	return &st, nil
}
