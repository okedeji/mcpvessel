// Package secrets stores the operator's named secret values in a 0600 file
// under ~/.agentcage. Provider endpoint keys and an agent's SECRETS entries
// both resolve against it by name.
package secrets

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/okedeji/agentcage/internal/env"
)

// Store holds named secret values. The redacting methods below keep values
// from leaking through %v, %#v, or a stray json.Marshal; persistence goes
// through the inner map directly.
type Store struct {
	values map[string]string
}

func (s Store) String() string               { return fmt.Sprintf("secrets.Store(%d names)", len(s.values)) }
func (s Store) GoString() string             { return s.String() }
func (s Store) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

// Load reads the secret store. A missing file is an empty store; a malformed
// file fails closed rather than silently dropping every secret.
func Load() (*Store, error) {
	path, err := secretsPath()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Store{values: map[string]string{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading secret store: %w", err)
	}
	values := map[string]string{}
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("parsing secret store: %w", err)
	}
	return &Store{values: values}, nil
}

// Save writes the store back with 0600 permissions. It marshals the inner
// map to bypass the redacting MarshalJSON.
func (s *Store) Save() error {
	path, err := secretsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	raw, err := json.MarshalIndent(s.values, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding secret store: %w", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("writing secret store: %w", err)
	}
	return nil
}

// Set stores value under name, replacing any previous value.
func (s *Store) Set(name, value string) {
	if s.values == nil {
		s.values = map[string]string{}
	}
	s.values[name] = value
}

// Get returns the value for name and whether it was present.
func (s *Store) Get(name string) (string, bool) {
	v, ok := s.values[name]
	return v, ok
}

// Names returns the stored names, sorted. Never values.
func (s *Store) Names() []string {
	names := make([]string, 0, len(s.values))
	for n := range s.values {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Remove deletes name, reporting whether it was present.
func (s *Store) Remove(name string) bool {
	if _, ok := s.values[name]; !ok {
		return false
	}
	delete(s.values, name)
	return true
}

func secretsPath() (string, error) {
	if home := strings.TrimSpace(os.Getenv(env.Home)); home != "" {
		return filepath.Join(home, "secrets.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating home directory: %w", err)
	}
	return filepath.Join(home, ".agentcage", "secrets.json"), nil
}
