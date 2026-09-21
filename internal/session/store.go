// session 包持久化将同一排障问题的多个诊断 run 串联起来的轻量状态和 Markdown
// 记忆。
package session

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/y2/go-sre-agent/internal/tools"
)

// DefaultDir 是生成的每会话状态约定根目录。
const DefaultDir = ".sessions"

const (
	sessionStateFile  = "session.json"
	sessionMemoryFile = "memory.md"
)

// State 标识一个排障问题及其所属 run。Goal 和 Environment 描述原始会话范围；每个
// run 的目标保存在其各自记录中。
type State struct {
	SessionID    string    `json:"session_id"`
	Goal         string    `json:"goal"`
	Environment  string    `json:"environment"`
	RunIDs       []string  `json:"run_ids"`
	MemoryDigest string    `json:"memory_digest,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Store 管理一个会话根目录，并在其下的独立目录中保存每个会话。
type Store struct {
	dir string
}

// NewStore 创建以 dir 为根目录的存储；空目录使用 .sessions。
func NewStore(dir string) *Store {
	if strings.TrimSpace(dir) == "" {
		dir = DefaultDir
	}
	return &Store{dir: dir}
}

// NewID 创建带时间戳前缀和随机后缀的文件系统安全会话标识。
func NewID(now time.Time) string {
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	return fmt.Sprintf("session_%s_%09d_%s", now.Format("20060102_150405"), now.Nanosecond(), randomIDSuffix())
}

// Save 使用已脱敏、已校验的状态原子替换 session.json。
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

// Load 按安全会话 ID 读取并校验 session.json。
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

// List 读取会话根目录下全部有效 session，并按最近更新时间倒序返回。空或尚未创建的
// 根目录返回空列表；无法验证的会话文件会返回错误，避免交互切换到不完整的上下文。
func (s *Store) List() ([]State, error) {
	if s == nil {
		return nil, fmt.Errorf("session store is nil")
	}
	entries, err := os.ReadDir(s.dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read session directory: %w", err)
	}
	sessions := make([]State, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !safeID(entry.Name()) {
			continue
		}
		state, err := s.Load(entry.Name())
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, state)
	}
	sort.Slice(sessions, func(left, right int) bool {
		if !sessions[left].UpdatedAt.Equal(sessions[right].UpdatedAt) {
			return sessions[left].UpdatedAt.After(sessions[right].UpdatedAt)
		}
		return sessions[left].SessionID > sessions[right].SessionID
	})
	return sessions, nil
}

// SaveMemory 原子替换 sessionID 对应的生成 Markdown 记忆；内容会在持久化边界再次
// 脱敏。
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

// MemoryDigest 返回 SaveMemory 实际写入的脱敏 Markdown 摘要；会话更新使用它拒绝
// 静默覆盖被人工修改的生成记忆文件。
func MemoryDigest(content string) string {
	sum := sha256.Sum256([]byte(redactedMemory(content)))
	return hex.EncodeToString(sum[:])
}

// LoadMemory 在生成 Markdown 文件不存在时返回空字符串，使有效会话可在下一次完成的
// run 时恢复。
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

// randomIDSuffix 优先使用密码学随机数；不可用时仅为保持实际唯一性而回退到时间戳表示。
func randomIDSuffix() string {
	var bytes [8]byte
	if _, err := rand.Read(bytes[:]); err == nil {
		return hex.EncodeToString(bytes[:])
	}
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

// statePath 推导 session.json 路径，同时防止 session ID 逃逸出存储根目录。
func (s *Store) statePath(sessionID string) (string, error) {
	dir, err := s.sessionDir(sessionID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, sessionStateFile), nil
}

// memoryPath 推导 memory.md 路径，同时防止 session ID 逃逸出存储根目录。
func (s *Store) memoryPath(sessionID string) (string, error) {
	dir, err := s.sessionDir(sessionID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, sessionMemoryFile), nil
}

// sessionDir 在将扁平标识追加到配置根目录前校验它。
func (s *Store) sessionDir(sessionID string) (string, error) {
	if !safeID(sessionID) {
		return "", fmt.Errorf("unsafe session id %q", sessionID)
	}
	return filepath.Join(s.dir, sessionID), nil
}

// safeID 只接受扁平且文件系统安全的会话标识。
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

// validateState 拒绝不完整的会话元数据和不安全的 run 引用。
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

// safeRunID 接受与 run store 相同的扁平标识形状，但不导入该包未导出的校验辅助函数。
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

// redactState 在会话元数据落盘前移除常见凭据形式，即使调用方意外传入未脱敏目标或标签。
func redactState(state State) State {
	state.Goal = tools.RedactSensitive(state.Goal)
	state.Environment = tools.RedactSensitive(state.Environment)
	return state
}

// redactedMemory 应用与摘要相同的持久化边界脱敏，使存储内容与记录的校验和始终一致。
func redactedMemory(content string) string {
	return tools.RedactSensitive(content)
}

// writeAtomic 通过已同步的临时文件写入一个私有会话文件，再在同目录替换目标文件。
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
