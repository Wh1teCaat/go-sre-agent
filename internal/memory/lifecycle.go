package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	runstore "github.com/y2/go-sre-agent/internal/run"
)

func (s *Store) UpdateForRun(state runstore.State) (bool, error) {
	return s.updateForRun(state, false)
}

// UpdateForRunForce 显式用已保存的 run 重建单次复盘及全部索引。它只应在操作者确认
// 丢弃对应生成文件中的人工修改后使用；原始 .runs 事实不受影响。
func (s *Store) UpdateForRunForce(state runstore.State) (bool, error) {
	return s.updateForRun(state, true)
}

// updateForRun 实现普通或显式覆盖的收录流程。
func (s *Store) updateForRun(state runstore.State, force bool) (bool, error) {
	if !eligible(state) {
		return false, nil
	}
	updated := false
	err := s.withWriteLock(func() error {
		if err := s.collectRunLocked(state, force); err != nil {
			return err
		}
		updated = true
		return s.rebuildLocked(force)
	})
	return updated, err
}

// Rebuild 从 rollout_summaries 中重建 raw_memories、MEMORY 和 memory_summary。
// 它不会扫描或改写 .runs，因此索引故障不会丢失原始运行事实。
func (s *Store) Rebuild() error {
	return s.withWriteLock(func() error {
		return s.rebuildLocked(false)
	})
}

// RebuildForce 是覆盖已被人工改动的索引文件的显式操作。它仍拒绝解析被人工改动的
// rollout summary，因为后者是知识来源，不能在未知内容上继续整理。
func (s *Store) RebuildForce() error {
	return s.withWriteLock(func() error {
		return s.rebuildLocked(true)
	})
}

// Invalidate 将指定复盘标记为失效，并从后续索引和模型提示中排除；原始 run 不受影响。
func (s *Store) Invalidate(runID, reason string) error {
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("invalidation reason is required")
	}
	return s.editRollout(runID, func(document *rolloutDocument) error {
		document.CollectionStatus = CollectionInvalidated
		document.InvalidationReason = normalizedText(reason, 500)
		document.UpdatedAt = time.Now().UTC()
		return nil
	})
}

// Correct 降低或维持一份复盘的结论强度，并记录修正说明。整理程序不允许把来源中
// suspected、undetermined 或未记录的结论升级为 identified。
func (s *Store) Correct(runID, conclusionStatus, note string) error {
	conclusionStatus = strings.TrimSpace(conclusionStatus)
	if !validConclusionStatus(conclusionStatus) {
		return fmt.Errorf("unsupported conclusion status %q", conclusionStatus)
	}
	if strings.TrimSpace(note) == "" {
		return fmt.Errorf("correction note is required")
	}
	return s.editRollout(runID, func(document *rolloutDocument) error {
		if conclusionStrength(conclusionStatus) > conclusionStrength(document.SourceConclusionStatus) {
			return fmt.Errorf("cannot upgrade source conclusion %q to %q", document.SourceConclusionStatus, conclusionStatus)
		}
		document.ConclusionOverride = conclusionStatus
		document.ConclusionStatus = conclusionStatus
		document.CorrectionNote = normalizedText(note, 500)
		document.UpdatedAt = time.Now().UTC()
		return nil
	})
}

// Delete 从跨会话知识集中逻辑删除指定复盘，并保留可审计的 tombstone。原始 run 和
// rollout 文件不会物理删除，避免误删恢复和证据回查所需的事实来源。
func (s *Store) Delete(runID, reason string) error {
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("deletion reason is required")
	}
	return s.editRollout(runID, func(document *rolloutDocument) error {
		document.CollectionStatus = CollectionDeleted
		document.InvalidationReason = normalizedText(reason, 500)
		document.UpdatedAt = time.Now().UTC()
		return nil
	})
}

// Search 按服务、环境和故障关键词读取轻量导航、主题索引和匹配复盘。返回内容已受
// 数量和字节预算限制，适合继续转换为模型的历史假设提示。

func (s *Store) collectRunLocked(state runstore.State, force bool) error {
	document, err := rolloutFromRun(state)
	if err != nil {
		return err
	}
	if !force {
		existing, err := s.loadRollout(state.RunID)
		if err == nil {
			document = mergeCollectionState(existing, document)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("refusing to update unverified rollout summary %q: %w", state.RunID, err)
		}
	}
	return s.writeRollout(document, force)
}

// rebuildLocked 从全部已验证复盘构造可重建索引；调用方必须已持有写锁。
func (s *Store) rebuildLocked(force bool) error {
	documents, err := s.loadAllRollouts()
	if err != nil {
		return err
	}
	raw := rawDocument{Generation: memoryGeneration, Kind: "raw_memories"}
	for _, document := range documents {
		raw.Entries = append(raw.Entries, rawEntry{
			RunID:            document.RunID,
			Service:          document.Service,
			Environment:      document.Environment,
			Outcome:          document.Outcome,
			ConclusionStatus: document.ConclusionStatus,
			CollectionStatus: document.CollectionStatus,
			Keywords:         append([]string(nil), document.Keywords...),
			Experience:       append([]string(nil), document.CandidateExperience...),
			FailureLessons:   append([]string(nil), document.FailureLessons...),
			Applicability:    applicabilityFor(document),
			Source:           "rollout_summaries/" + document.RunID + ".md",
		})
	}
	index := indexDocument{Generation: memoryGeneration, Kind: "memory_index", Topics: topicsFromRollouts(documents)}
	summary := summaryDocument{Generation: memoryGeneration, Kind: "memory_summary", Topics: append([]topic(nil), index.Topics...)}
	if err := s.writeRaw(raw, force); err != nil {
		return err
	}
	if err := s.writeIndex(index, force); err != nil {
		return err
	}
	return s.writeSummary(summary, force)
}

// editRollout 修改单份已验证复盘后重建索引。每种生命周期操作都显式经过该路径。
func (s *Store) editRollout(runID string, edit func(*rolloutDocument) error) error {
	if !safeRunID(runID) {
		return fmt.Errorf("unsafe run id %q", runID)
	}
	return s.withWriteLock(func() error {
		document, err := s.loadRollout(runID)
		if err != nil {
			return err
		}
		if err := edit(&document); err != nil {
			return err
		}
		if err := s.writeRollout(document, false); err != nil {
			return err
		}
		return s.rebuildLocked(false)
	})
}

// withWriteLock 防止不同 session 同时重建索引而互相覆盖。拿不到锁会返回错误，
// 调用方保留已经保存的 run，稍后可安全执行 memory rebuild 恢复索引。
func (s *Store) withWriteLock(action func() error) error {
	if s == nil {
		return fmt.Errorf("memory store is nil")
	}
	if err := s.ensureRoot(); err != nil {
		return err
	}
	lockPath := filepath.Join(s.dir, lockFileName)
	deadline := time.Now().Add(3 * time.Second)
	lockToken := strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	for {
		lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			if _, writeErr := lock.WriteString(lockToken); writeErr != nil {
				_ = lock.Close()
				_ = os.Remove(lockPath)
				return fmt.Errorf("write memory lock: %w", writeErr)
			}
			if syncErr := lock.Sync(); syncErr != nil {
				_ = lock.Close()
				_ = os.Remove(lockPath)
				return fmt.Errorf("sync memory lock: %w", syncErr)
			}
			if closeErr := lock.Close(); closeErr != nil {
				_ = os.Remove(lockPath)
				return fmt.Errorf("close memory lock: %w", closeErr)
			}
			defer releaseWriteLock(lockPath, lockToken)
			return action()
		}
		if !os.IsExist(err) {
			return fmt.Errorf("create memory lock: %w", err)
		}
		if info, statErr := os.Stat(lockPath); statErr == nil && time.Since(info.ModTime()) > lockStaleAfter {
			removeErr := os.Remove(lockPath)
			if removeErr == nil || os.IsNotExist(removeErr) {
				continue
			}
			return fmt.Errorf("remove stale memory lock: %w", removeErr)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("memory index is busy; retry rebuild after the active update completes")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// releaseWriteLock 只删除仍属于当前持锁者的锁，避免异常陈旧锁回收后误删新锁。
func releaseWriteLock(path, token string) {
	data, err := os.ReadFile(path)
	if err != nil || string(data) != token {
		return
	}
	_ = os.Remove(path)
}
