package run

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/trace"
)

type Status string

const (
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
)

// State 是一次诊断运行的可持久化快照。
// 它保存 trace 和最终 diagnosis，供后续 status/report/resume 使用。
type State struct {
	RunID string `json:"run_id"`
	// SessionID optionally links this run to a phase-1 session. Its omission
	// preserves compatibility with runs written before session memory existed.
	SessionID string            `json:"session_id,omitempty"`
	Goal      string            `json:"goal"`
	Status    Status            `json:"status"`
	Plan      schema.Plan       `json:"plan,omitempty"`
	Diagnosis *schema.Diagnosis `json:"diagnosis,omitempty"`
	Trace     []trace.Entry     `json:"trace"`
	Error     string            `json:"error,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

type Store struct {
	dir string
}

func NewStore(dir string) *Store {
	return &Store{dir: dir}
}

func NewRunID(now time.Time) string {
	now = now.UTC()
	return fmt.Sprintf("run_%s_%09d_%s", now.Format("20060102_150405"), now.Nanosecond(), randomRunIDSuffix())
}

func randomRunIDSuffix() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

// Save 以 JSON 文件保存 run state。文件名只由 run id 决定，避免调用方传入任意路径。
// 数据先写入同目录临时文件再原子替换，进程中断时不会留下半截 JSON。
func (s *Store) Save(state State) error {
	path, err := s.path(state.RunID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("create run dir: %w", err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode run state: %w", err)
	}
	temporary, err := os.CreateTemp(s.dir, ".run-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary run state: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("secure temporary run state: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("write temporary run state: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync temporary run state: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary run state: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace run state: %w", err)
	}
	return nil
}

func (s *Store) Load(runID string) (State, error) {
	path, err := s.path(runID)
	if err != nil {
		return State{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return State{}, fmt.Errorf("read run state: %w", err)
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, fmt.Errorf("decode run state: %w", err)
	}
	return state, nil
}

func (s *Store) path(runID string) (string, error) {
	if !safeRunID(runID) {
		return "", fmt.Errorf("unsafe run id %q", runID)
	}
	return filepath.Join(s.dir, runID+".json"), nil
}

func safeRunID(runID string) bool {
	if strings.TrimSpace(runID) == "" {
		return false
	}
	if runID != filepath.Base(runID) {
		return false
	}
	for _, r := range runID {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}
