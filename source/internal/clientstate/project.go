package clientstate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Provenance is a credential fingerprint, never a bearer or diagnostic field.
type ProjectKnowledge struct {
	GenerationID string `json:"generation_id,omitempty"`
	Supported    bool   `json:"supported,omitempty"`
	Terminal     string `json:"terminal,omitempty"`
}

func (s *Store) projectFile(scope string) string {
	sum := sha256.Sum256([]byte(scope))
	return filepath.Join(s.dir, "project-"+hex.EncodeToString(sum[:16])+".json")
}
func (s *Store) LoadProjectKnowledge(scope string) (map[string]ProjectKnowledge, error) {
	data, err := os.ReadFile(s.projectFile(scope))
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]ProjectKnowledge{}, nil
	}
	if err != nil {
		return nil, errors.New("project knowledge is unreadable")
	}
	var out map[string]ProjectKnowledge
	if len(data) > 64<<10 || json.Unmarshal(data, &out) != nil || out == nil {
		return nil, errors.New("project knowledge is corrupt")
	}
	return out, nil
}
func (s *Store) SaveProjectKnowledge(scope string, entries map[string]ProjectKnowledge) error {
	release, err := lockFileWait(s.projectFile(scope)+".lock", 5*time.Second)
	if err != nil {
		return errors.New("project knowledge is locked")
	}
	defer release()
	merged, err := s.LoadProjectKnowledge(scope)
	if err != nil {
		return err
	}
	for k, v := range entries {
		merged[k] = v
	}
	data, err := json.Marshal(merged)
	if err != nil || len(data) > 64<<10 {
		return errors.New("project knowledge is invalid")
	}
	return writeFile(s.projectFile(scope), data)
}
