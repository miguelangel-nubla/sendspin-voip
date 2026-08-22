package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/miguelangel-nubla/sendspin-voip/internal/app"
)

// FileStore implements app.StateStorePort persisting player state to a JSON file on disk.
type FileStore struct {
	mu       sync.RWMutex
	filePath string
	states   map[string]app.PlayerStateRecord
}

// NewFileStore creates a FileStore and loads any existing persisted state from disk.
// If loading fails due to a corrupt file or read error, an error is returned.
func NewFileStore(filePath string) (*FileStore, error) {
	store := &FileStore{
		filePath: filePath,
		states:   make(map[string]app.PlayerStateRecord),
	}
	if err := store.load(); err != nil {
		return store, err
	}
	return store, nil
}

func (s *FileStore) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.filePath == "" {
		return nil
	}

	data, err := os.ReadFile(s.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var loaded map[string]app.PlayerStateRecord
	if err := json.Unmarshal(data, &loaded); err != nil {
		return fmt.Errorf("failed to unmarshal state file %s: %w", s.filePath, err)
	}

	// A state file containing JSON "null" (or an empty file that a crashed write
	// left behind) unmarshals into a nil map. Assigning that would make the very
	// next SetPlayerState panic with "assignment to entry in nil map".
	if loaded == nil {
		loaded = make(map[string]app.PlayerStateRecord)
	}

	s.states = loaded
	return nil
}

func (s *FileStore) save() error {
	if s.filePath == "" {
		return nil
	}

	data, err := json.MarshalIndent(s.states, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(s.filePath)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}

	tmpFile := s.filePath + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0644); err != nil {
		return err
	}

	return os.Rename(tmpFile, s.filePath)
}

// GetPlayerState retrieves the persisted state for a given player ID.
func (s *FileStore) GetPlayerState(playerID string) (app.PlayerStateRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rec, ok := s.states[playerID]
	return rec, ok
}

// SetPlayerState updates and persists player state to disk atomically.
func (s *FileStore) SetPlayerState(playerID string, state app.PlayerStateRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.states[playerID] = state
	return s.save()
}
