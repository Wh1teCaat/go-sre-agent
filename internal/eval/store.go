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

// DefaultResultsDir is the conventional location for generated evaluation
// records. It is intended to be ignored by Git while scenario definitions and
// sanitized fixtures remain versioned separately.
const DefaultResultsDir = "evals/results"

// Store persists evaluation Result values as one JSON document per result.
type Store struct {
	dir string
}

// NewStore creates an evaluation result store. An empty directory uses the
// conventional evals/results location.
func NewStore(dir string) *Store {
	if strings.TrimSpace(dir) == "" {
		dir = DefaultResultsDir
	}
	return &Store{dir: dir}
}

// NewResultID creates a filesystem-safe unique evaluation identifier.
func NewResultID(now time.Time) string {
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	return fmt.Sprintf("eval_%s_%09d_%s", now.Format("20060102_150405"), now.Nanosecond(), randomResultIDSuffix())
}

// randomResultIDSuffix uses cryptographic randomness when available and falls
// back to a timestamp representation solely to preserve identifier uniqueness.
func randomResultIDSuffix() string {
	var bytes [8]byte
	if _, err := rand.Read(bytes[:]); err == nil {
		return hex.EncodeToString(bytes[:])
	}
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

// Save atomically writes a redacted JSON result with 0600 permissions and
// returns its path. It never writes outside the configured result directory.
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

// Load reads a previously persisted evaluation result by its safe identifier.
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

// path validates a result ID before deriving its path below the store root.
func (s *Store) path(resultID string) (string, error) {
	if !safeResultID(resultID) {
		return "", fmt.Errorf("unsafe evaluation result id %q", resultID)
	}
	return filepath.Join(s.dir, resultID+".json"), nil
}

// safeResultID accepts only flat filesystem-safe identifiers.
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

// validateResult rejects result states that could misrepresent a mock or
// unexecuted real-model evaluation before they are persisted.
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

// redactResult copies a result while applying the repository's sensitive-data
// redaction policy to every persisted human-readable field.
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
