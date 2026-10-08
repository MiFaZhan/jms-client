package endpoint

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/MiFaZhan/jms-client/internal/atomicfile"
)

// StateFileName is the file that holds cross-process endpoint memory
// inside the configuration directory.
const StateFileName = "state.json"

// stateDoc is the on-disk document.
type stateDoc struct {
	Servers map[string]State `json:"servers"`
}

// FileStateStore persists State in a small JSON file.
//
// It is used so that a one-shot CLI invocation still benefits from the
// last-good address without paying for a probe (DESIGN.md「端点故障转移」第 5 点).
// The file is written atomically with mode 0600 on Unix.
type FileStateStore struct {
	path string

	// mu guards concurrent Get/Set from one process. Cross-process
	// atomicity comes from the temp-file + rename write.
	mu sync.Mutex
}

// NewFileStateStore returns a StateStore backed by path. When path is
// empty, StateFileName in the current directory is used.
func NewFileStateStore(path string) StateStore {
	if path == "" {
		path = StateFileName
	}
	return &FileStateStore{path: path}
}

// Path returns the backing file path, or "" on a nil receiver.
func (s *FileStateStore) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// Get returns the recorded state for a server alias. A missing or
// corrupt file is not an error: it reports "not found" and the caller
// pays one extra probe.
func (s *FileStateStore) Get(server string) (State, bool) {
	if s == nil {
		return State{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	doc := s.load()
	if doc == nil {
		return State{}, false
	}
	st, ok := doc.Servers[server]
	return st, ok
}

// Set records the state for a server alias, writing the file atomically.
func (s *FileStateStore) Set(server string, st State) error {
	if s == nil {
		return errors.New("state store is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	doc := s.load()
	if doc == nil {
		doc = &stateDoc{Servers: map[string]State{}}
	}
	if doc.Servers == nil {
		doc.Servers = map[string]State{}
	}
	doc.Servers[server] = st

	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode endpoint state: %w", err)
	}
	data = append(data, '\n')

	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	return atomicfile.WriteFile(s.path, data, 0o600)
}

// load reads and parses the file, returning nil when it is absent or
// unreadable. A corrupt state file is deliberately not an error: it only
// costs one extra probe.
func (s *FileStateStore) load() *stateDoc {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return nil
	}
	var doc stateDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil
	}
	if doc.Servers == nil {
		doc.Servers = map[string]State{}
	}
	return &doc
}

// NewMemoryStateStore returns an in-memory StateStore for tests.
func NewMemoryStateStore() *MemoryStateStore {
	return &MemoryStateStore{servers: map[string]State{}}
}

// MemoryStateStore is an in-memory StateStore.
type MemoryStateStore struct {
	mu      sync.Mutex
	servers map[string]State
}

// Get returns the recorded state for a server alias.
func (m *MemoryStateStore) Get(server string) (State, bool) {
	if m == nil {
		return State{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.servers[server]
	return st, ok
}

// Set records the state for a server alias.
func (m *MemoryStateStore) Set(server string, st State) error {
	if m == nil {
		return errors.New("state store is nil")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.servers == nil {
		m.servers = map[string]State{}
	}
	m.servers[server] = st
	return nil
}

// BackendFor returns the remembered backend for a kind, or "" when none
// is recorded (DESIGN.md「端点故障转移」第 4 点).
func (s State) BackendFor(kind Kind) string {
	if s.Backend == nil {
		return ""
	}
	return s.Backend[kind]
}

// Touch records a successful selection of kind at at, preserving the
// backend memory.
func (s State) Touch(kind Kind, at time.Time) State {
	s.LastGood = kind
	s.At = at
	return s
}
