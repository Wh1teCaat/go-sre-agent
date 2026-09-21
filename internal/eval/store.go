package eval

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

	"github.com/y2/go-sre-agent/internal/tools"
)

// DefaultResultsDir 是生成评测记录的约定目录。它应被 Git 忽略，场景定义和脱敏
// 样本则分别纳入版本控制。
const DefaultResultsDir = "evals/results"

// Store 将评测 Result 按每个结果一个 JSON 文档持久化。
type Store struct {
	dir string
}

// NewStore 创建评测结果存储；空目录使用约定的 evals/results 位置。
func NewStore(dir string) *Store {
	if strings.TrimSpace(dir) == "" {
		dir = DefaultResultsDir
	}
	return &Store{dir: dir}
}

// NewResultID 创建文件系统安全的唯一评测标识。
func NewResultID(now time.Time) string {
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	return fmt.Sprintf("eval_%s_%09d_%s", now.Format("20060102_150405"), now.Nanosecond(), randomResultIDSuffix())
}

// randomResultIDSuffix 优先使用密码学随机数；不可用时仅为保持标识唯一性而回退到
// 时间戳表示。
func randomResultIDSuffix() string {
	var bytes [8]byte
	if _, err := rand.Read(bytes[:]); err == nil {
		return hex.EncodeToString(bytes[:])
	}
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

// Save 原子写入权限为 0600 的脱敏 JSON 结果并返回其路径；它绝不写出配置的结果目录。
func (s *Store) Save(result Result) (string, error) {
	if s == nil {
		return "", fmt.Errorf("evaluation result store is nil")
	}
	if err := validateResult(result); err != nil {
		return "", err
	}
	path, err := s.path(result.ID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return "", fmt.Errorf("create evaluation result dir: %w", err)
	}

	data, err := json.MarshalIndent(redactResult(result), "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode evaluation result: %w", err)
	}
	temporary, err := os.CreateTemp(s.dir, ".eval-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create temporary evaluation result: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("secure temporary evaluation result: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("write temporary evaluation result: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("sync temporary evaluation result: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("close temporary evaluation result: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return "", fmt.Errorf("replace evaluation result: %w", err)
	}
	return path, nil
}

// Load 按安全标识读取此前持久化的评测结果。
func (s *Store) Load(resultID string) (Result, error) {
	if s == nil {
		return Result{}, fmt.Errorf("evaluation result store is nil")
	}
	path, err := s.path(resultID)
	if err != nil {
		return Result{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Result{}, fmt.Errorf("read evaluation result: %w", err)
	}
	var result Result
	if err := json.Unmarshal(data, &result); err != nil {
		return Result{}, fmt.Errorf("decode evaluation result: %w", err)
	}
	return result, nil
}

// path 在存储根目录下推导路径前校验结果 ID。
func (s *Store) path(resultID string) (string, error) {
	if !safeResultID(resultID) {
		return "", fmt.Errorf("unsafe evaluation result id %q", resultID)
	}
	return filepath.Join(s.dir, resultID+".json"), nil
}

// safeResultID 只接受扁平且文件系统安全的标识。
func safeResultID(resultID string) bool {
	if strings.TrimSpace(resultID) == "" || resultID != filepath.Base(resultID) {
		return false
	}
	for _, character := range resultID {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}

// validateResult 在持久化前拒绝可能错误表达 mock 或未执行真实模型评测的结果状态。
func validateResult(result Result) error {
	if !safeResultID(result.ID) {
		return fmt.Errorf("unsafe evaluation result id %q", result.ID)
	}
	if strings.TrimSpace(result.ScenarioID) == "" {
		return fmt.Errorf("evaluation result scenario id is required")
	}
	switch result.Mode {
	case ModeMock, ModeReal:
	default:
		return fmt.Errorf("unsupported evaluation mode %q", result.Mode)
	}
	switch result.Status {
	case StatusPassed, StatusFailed, StatusSkipped:
	default:
		return fmt.Errorf("unsupported evaluation status %q", result.Status)
	}
	if result.Mode == ModeMock && result.ExecutedRealModel {
		return fmt.Errorf("mock evaluation cannot execute a real model")
	}
	if result.Mode == ModeReal && result.Status == StatusPassed && !result.ExecutedRealModel {
		return fmt.Errorf("passed real-model evaluation requires an executed model attempt")
	}
	return nil
}

// redactResult 复制结果，并对每个持久化的可读字段应用仓库敏感数据脱敏策略。
func redactResult(input Result) Result {
	output := input
	output.ScenarioName = tools.RedactSensitive(input.ScenarioName)
	output.Command = tools.RedactSensitive(input.Command)
	output.Model = tools.RedactSensitive(input.Model)
	output.Error = tools.RedactSensitive(input.Error)
	output.Assertions = make([]Assertion, len(input.Assertions))
	for index, assertion := range input.Assertions {
		output.Assertions[index] = Assertion{
			Name:   tools.RedactSensitive(assertion.Name),
			Passed: assertion.Passed,
			Detail: tools.RedactSensitive(assertion.Detail),
		}
	}
	return output
}
