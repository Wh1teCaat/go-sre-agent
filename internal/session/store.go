// Package session persists the lightweight state and Markdown memory that
// connect multiple diagnostic runs for one investigation.
package session

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/y2/go-sre-agent/internal/tools"
)

// DefaultDir is the conventional root for generated per-session state.
const DefaultDir = ".sessions"

const (
	sessionStateFile  = "session.json"
	sessionMemoryFile = "memory.md"
)

// State identifies one investigation and the runs that belong to it. Goal and
// Environment describe the original session scope; individual run goals stay
// in their respective run records.
type State struct {
	SessionID    string    `json:"session_id"`
	Goal         string    `json:"goal"`
	Environment  string    `json:"environment"`
	RunIDs       []string  `json:"run_ids"`
	MemoryDigest string    `json:"memory_digest,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Store owns one session-root directory and stores every session in a distinct
// directory below it.
type Store struct {
	dir string
}

// NewStore creates a store rooted at dir. An empty directory uses .sessions.
func NewStore(dir string) *Store {
	if strings.TrimSpace(dir) == "" {
		dir = DefaultDir
	}
	return &Store{dir: dir}
}

// NewID creates a filesystem-safe session identifier with a timestamp prefix
// and a random suffix.
func NewID(now time.Time) string {
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	return fmt.Sprintf("session_%s_%09d_%s", now.Format("20060102_150405"), now.Nanosecond(), randomIDSuffix())
}

// Save atomically replaces session.json with a redacted, validated state.
func (s *Store) Save(state State) error {
	if s == nil {
		return fmt.Errorf("session store is nil")
	}
	state = redactState(state)
	if err := validateState(state); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode session state: %w", err)
	}
	path, err := s.statePath(state.SessionID)
	if err != nil {
		return err
	}
	if err := writeAtomic(path, data); err != nil {
		return fmt.Errorf("save session state: %w", err)
	}
	return nil
}

// Load reads and validates session.json for a safe session ID.
func (s *Store) Load(sessionID string) (State, error) {
	if s == nil {
		return State{}, fmt.Errorf("session store is nil")
	}
	path, err := s.statePath(sessionID)
	if err != nil {
		return State{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return State{}, fmt.Errorf("read session state: %w", err)
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, fmt.Errorf("decode session state: %w", err)
	}
	if state.SessionID != sessionID {
		return State{}, fmt.Errorf("session %q contains mismatched session_id %q", sessionID, state.SessionID)
	}
	if err := validateState(state); err != nil {
		return State{}, fmt.Errorf("invalid session state: %w", err)
	}
	return state, nil
}

// SaveMemory atomically replaces the generated Markdown memory for sessionID.
// Content is redacted again at the persistence boundary.
func (s *Store) SaveMemory(sessionID, content string) error {
	if s == nil {
		return fmt.Errorf("session store is nil")
	}
	path, err := s.memoryPath(sessionID)
	if err != nil {
		return err
	}
	if err := writeAtomic(path, []byte(redactedMemory(content))); err != nil {
		return fmt.Errorf("save session memory: %w", err)
	}
	return nil
}

// MemoryDigest returns the digest of exactly the redacted Markdown that
// SaveMemory writes. Session updates use it to refuse silent overwrite of a
// manually changed generated memory file.
func MemoryDigest(content string) string {
	sum := sha256.Sum256([]byte(redactedMemory(content)))
	return hex.EncodeToString(sum[:])
}

// LoadMemory returns an empty string when the generated Markdown file is not
// present, allowing a valid session to recover on its next completed run.
func (s *Store) LoadMemory(sessionID string) (string, error) {
	if s == nil {
		return "", fmt.Errorf("session store is nil")
	}
	path, err := s.memoryPath(sessionID)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read session memory: %w", err)
	}
	return string(data), nil
}

// randomIDSuffix uses cryptographic randomness when available and falls back
// to a timestamp representation only to preserve practical uniqueness.
func randomIDSuffix() string {
	var bytes [8]byte
	if _, err := rand.Read(bytes[:]); err == nil {
		return hex.EncodeToString(bytes[:])
	}
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

// statePath derives the session.json path while preventing a session ID from
// escaping the store root.
func (s *Store) statePath(sessionID string) (string, error) {
	dir, err := s.sessionDir(sessionID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, sessionStateFile), nil
}

// memoryPath derives the memory.md path while preventing a session ID from
// escaping the store root.
func (s *Store) memoryPath(sessionID string) (string, error) {
	dir, err := s.sessionDir(sessionID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, sessionMemoryFile), nil
}

// sessionDir validates a flat identifier before appending it to the configured
// root directory.
func (s *Store) sessionDir(sessionID string) (string, error) {
	if !safeID(sessionID) {
		return "", fmt.Errorf("unsafe session id %q", sessionID)
	}
	return filepath.Join(s.dir, sessionID), nil
}

// safeID accepts only flat filesystem-safe session identifiers.
func safeID(sessionID string) bool {
	if strings.TrimSpace(sessionID) == "" || sessionID != filepath.Base(sessionID) {
		return false
	}
	for _, character := range sessionID {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}

// validateState rejects incomplete session metadata and unsafe run references.
func validateState(state State) error {
	if !safeID(state.SessionID) {
		return fmt.Errorf("unsafe session id %q", state.SessionID)
	}
	if strings.TrimSpace(state.Goal) == "" {
		return fmt.Errorf("session goal is required")
	}
	if strings.TrimSpace(state.Environment) == "" {
		return fmt.Errorf("session environment is required")
	}
	if state.CreatedAt.IsZero() || state.UpdatedAt.IsZero() {
		return fmt.Errorf("session timestamps are required")
	}
	if state.MemoryDigest != "" {
		decoded, err := hex.DecodeString(state.MemoryDigest)
		if err != nil || len(decoded) != sha256.Size {
			return fmt.Errorf("invalid session memory digest")
		}
	}
	seenRuns := make(map[string]struct{}, len(state.RunIDs))
	for _, runID := range state.RunIDs {
		if !safeRunID(runID) {
			return fmt.Errorf("unsafe session run id %q", runID)
		}
		if _, exists := seenRuns[runID]; exists {
			return fmt.Errorf("duplicate session run id %q", runID)
		}
		seenRuns[runID] = struct{}{}
	}
	return nil
}

// safeRunID accepts the same flat identifier shape used by the run store,
// without importing that package's unexported validation helper.
func safeRunID(runID string) bool {
	if strings.TrimSpace(runID) == "" || runID != filepath.Base(runID) {
		return false
	}
	for _, character := range runID {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}

// redactState removes common credential forms before session metadata reaches
// disk, even when the caller accidentally passes an unredacted goal or label.
func redactState(state State) State {
	state.Goal = tools.RedactSensitive(state.Goal)
	state.Environment = tools.RedactSensitive(state.Environment)
	return state
}

// redactedMemory applies the same persistence-boundary redaction used by the
// digest so stored content and its recorded checksum always agree.
func redactedMemory(content string) string {
	return tools.RedactSensitive(content)
}

// writeAtomic writes one private session file through a synced temporary file
// and then replaces the destination in the same directory.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create session dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("secure session dir: %w", err)
	}
	temporary, err := os.CreateTemp(dir, ".session-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary session file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure temporary session file: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write temporary session file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync temporary session file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary session file: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace session file: %w", err)
	}
	return nil
}
