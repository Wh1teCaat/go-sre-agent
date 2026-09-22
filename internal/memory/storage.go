package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

func (s *Store) ensureRoot() error {
	if err := os.MkdirAll(s.rolloutDir(), 0o700); err != nil {
		return fmt.Errorf("create memory directory: %w", err)
	}
	if err := os.Chmod(s.dir, 0o700); err != nil {
		return fmt.Errorf("secure memory directory: %w", err)
	}
	return os.Chmod(s.rolloutDir(), 0o700)
}

// rolloutDir 返回单次复盘目录。
func (s *Store) rolloutDir() string {
	return filepath.Join(s.dir, rolloutDirName)
}

// rolloutPath 生成安全 run ID 对应的 Markdown 复盘文件路径。
func (s *Store) rolloutPath(runID string) (string, error) {
	if !safeRunID(runID) {
		return "", fmt.Errorf("unsafe run id %q", runID)
	}
	return filepath.Join(s.rolloutDir(), runID+".md"), nil
}

// loadAllRollouts 读取所有来源复盘。任何人工改动或损坏都会阻止重建，避免把未知
// Markdown 混入自动索引。
func (s *Store) loadAllRollouts() ([]rolloutDocument, error) {
	entries, err := os.ReadDir(s.rolloutDir())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read rollout summaries: %w", err)
	}
	documents := make([]rolloutDocument, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		runID := strings.TrimSuffix(entry.Name(), ".md")
		if !safeRunID(runID) {
			return nil, fmt.Errorf("unsafe rollout file %q", entry.Name())
		}
		document, err := s.loadRollout(runID)
		if err != nil {
			return nil, err
		}
		documents = append(documents, document)
	}
	sort.Slice(documents, func(left, right int) bool { return documents[left].RunID < documents[right].RunID })
	return documents, nil
}

// loadRollout 读取并验证由本版本生成的单次复盘。
func (s *Store) loadRollout(runID string) (rolloutDocument, error) {
	path, err := s.rolloutPath(runID)
	if err != nil {
		return rolloutDocument{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return rolloutDocument{}, err
	}
	front, body, err := splitDocument(data)
	if err != nil {
		return rolloutDocument{}, fmt.Errorf("read rollout %q: %w", runID, err)
	}
	var document rolloutDocument
	if err := yaml.Unmarshal(front, &document); err != nil {
		return rolloutDocument{}, fmt.Errorf("decode rollout %q: %w", runID, err)
	}
	if err := verifyRollout(document, body); err != nil {
		return rolloutDocument{}, fmt.Errorf("verify rollout %q: %w", runID, err)
	}
	if document.RunID != runID || !safeRunID(document.RunID) {
		return rolloutDocument{}, fmt.Errorf("rollout %q contains invalid run_id %q", runID, document.RunID)
	}
	return document, nil
}

// loadIndex 读取并验证主题索引。
func (s *Store) loadIndex() (indexDocument, error) {
	path := filepath.Join(s.dir, indexFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return indexDocument{}, err
	}
	front, body, err := splitDocument(data)
	if err != nil {
		return indexDocument{}, err
	}
	var document indexDocument
	if err := yaml.Unmarshal(front, &document); err != nil {
		return indexDocument{}, err
	}
	if err := verifyIndex(document, body); err != nil {
		return indexDocument{}, err
	}
	return document, nil
}

// loadSummary 读取并验证轻量导航文件。
func (s *Store) loadSummary() (summaryDocument, error) {
	path := filepath.Join(s.dir, summaryFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return summaryDocument{}, err
	}
	front, body, err := splitDocument(data)
	if err != nil {
		return summaryDocument{}, err
	}
	var document summaryDocument
	if err := yaml.Unmarshal(front, &document); err != nil {
		return summaryDocument{}, err
	}
	if err := verifySummary(document, body); err != nil {
		return summaryDocument{}, err
	}
	return document, nil
}

// writeRollout 原子写入复盘，并在非强制模式下拒绝覆盖人工编辑。
func (s *Store) writeRollout(document rolloutDocument, force bool) error {
	path, err := s.rolloutPath(document.RunID)
	if err != nil {
		return err
	}
	data, err := renderRollout(document)
	if err != nil {
		return err
	}
	return writeGenerated(path, data, force, func(existing []byte) error {
		front, body, err := splitDocument(existing)
		if err != nil {
			return err
		}
		var current rolloutDocument
		if err := yaml.Unmarshal(front, &current); err != nil {
			return err
		}
		return verifyRollout(current, body)
	})
}

// writeRaw 原子写入候选经验汇总。
func (s *Store) writeRaw(document rawDocument, force bool) error {
	data, err := renderRaw(document)
	if err != nil {
		return err
	}
	return writeGenerated(filepath.Join(s.dir, rawFileName), data, force, func(existing []byte) error {
		front, body, err := splitDocument(existing)
		if err != nil {
			return err
		}
		var current rawDocument
		if err := yaml.Unmarshal(front, &current); err != nil {
			return err
		}
		return verifyRaw(current, body)
	})
}

// writeIndex 原子写入主题化知识索引。
func (s *Store) writeIndex(document indexDocument, force bool) error {
	data, err := renderIndex(document)
	if err != nil {
		return err
	}
	return writeGenerated(filepath.Join(s.dir, indexFileName), data, force, func(existing []byte) error {
		front, body, err := splitDocument(existing)
		if err != nil {
			return err
		}
		var current indexDocument
		if err := yaml.Unmarshal(front, &current); err != nil {
			return err
		}
		return verifyIndex(current, body)
	})
}

// writeSummary 原子写入轻量导航文件。
func (s *Store) writeSummary(document summaryDocument, force bool) error {
	data, err := renderSummary(document)
	if err != nil {
		return err
	}
	return writeGenerated(filepath.Join(s.dir, summaryFileName), data, force, func(existing []byte) error {
		front, body, err := splitDocument(existing)
		if err != nil {
			return err
		}
		var current summaryDocument
		if err := yaml.Unmarshal(front, &current); err != nil {
			return err
		}
		return verifySummary(current, body)
	})
}

// writeGenerated 先验证旧文件仍由本版本未修改地生成，再使用临时文件原子替换。
func writeGenerated(path string, data []byte, force bool, verify func([]byte) error) error {
	if existing, err := os.ReadFile(path); err == nil {
		if !force {
			if err := verify(existing); err != nil {
				return fmt.Errorf("refusing to overwrite manually changed generated file %q: %w", path, err)
			}
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read generated file %q: %w", path, err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create memory output directory: %w", err)
	}
	temporary, err := os.CreateTemp(dir, ".memory-*.tmp")
	if err != nil {
		return fmt.Errorf("create memory temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure memory temporary file: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write memory temporary file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync memory temporary file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close memory temporary file: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace memory generated file: %w", err)
	}
	return nil
}
