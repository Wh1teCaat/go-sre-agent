package memory

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	runstore "github.com/y2/go-sre-agent/internal/run"
	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/trace"
)

// TestUpdateForRunBuildsDeterministicKnowledgeFilesAndFiltersHints 验证脱敏、幂等、范围隔离和相关性排序。
func TestUpdateForRunBuildsDeterministicKnowledgeFilesAndFiltersHints(t *testing.T) {
	store := NewStore(t.TempDir())
	first := memoryTestRun("run_redis", "go-chat", "local", "suspected")
	first.Goal = `诊断 Redis 连接超时 token=top-secret-token`
	first.Trace[0].Result.Data = map[string]any{"token": "top-secret-token"}
	updated, err := store.UpdateForRun(first)
	if err != nil || !updated {
		t.Fatalf("update first run = %v / %v", updated, err)
	}
	if updated, err := store.UpdateForRun(first); err != nil || !updated {
		t.Fatalf("repeat update first run = %v / %v", updated, err)
	}
	second := memoryTestRun("run_postgres", "other-service", "local", "undetermined")
	if updated, err := store.UpdateForRun(second); err != nil || !updated {
		t.Fatalf("update second run = %v / %v", updated, err)
	}
	third := memoryTestRun("run_postgres_same_scope", "go-chat", "local", "undetermined")
	third.Goal = "诊断 PostgreSQL 连接超时"
	third.Trace[0].ToolName = "postgres_ping"
	third.Trace[0].Result.Tool = "postgres_ping"
	third.Trace[0].Result.Summary = "PostgreSQL 连接被拒绝"
	third.Diagnosis.Summary = "PostgreSQL 当前不可达。"
	third.Diagnosis.RootCause.Statement = "PostgreSQL 连接问题需要当前环境继续验证。"
	third.Diagnosis.Evidence[0].Tool = "postgres_ping"
	third.Diagnosis.Evidence[0].Summary = "PostgreSQL 连接被拒绝"
	if updated, err := store.UpdateForRun(third); err != nil || !updated {
		t.Fatalf("update third run = %v / %v", updated, err)
	}

	for _, name := range []string{summaryFileName, indexFileName, rawFileName, filepath.Join(rolloutDirName, first.RunID+".md")} {
		path := filepath.Join(store.dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if strings.Contains(string(data), "top-secret-token") {
			t.Fatalf("memory file %s leaked secret: %s", name, data)
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("memory file mode %s = %v / %v", name, info, err)
		}
	}
	index, err := os.ReadFile(filepath.Join(store.dir, indexFileName))
	if err != nil || !strings.Contains(string(index), "可复用知识") {
		t.Fatalf("index reusable knowledge = %q / %v", index, err)
	}
	raw, err := os.ReadFile(filepath.Join(store.dir, rawFileName))
	if err != nil || !strings.Contains(string(raw), "适用条件") {
		t.Fatalf("raw applicability = %q / %v", raw, err)
	}

	hints, err := store.Hints(Query{Service: "go-chat", Environment: "local", Goal: "Redis 连接异常", MaxMatches: 1, MaxBytes: 4096})
	if err != nil {
		t.Fatalf("load hints: %v", err)
	}
	if len(hints) != 1 || hints[0].SourceRunID != first.RunID || !strings.Contains(hints[0].Content, "结论强度 suspected") {
		t.Fatalf("hints = %#v", hints)
	}
	if strings.Contains(hints[0].Content, "other-service") || strings.Contains(hints[0].Content, "top-secret-token") {
		t.Fatalf("hint scope/redaction failure: %q", hints[0].Content)
	}

	matches, err := store.Search(Query{Service: "go-chat", Environment: "local", Goal: "Redis", MaxMatches: 1, MaxBytes: 300})
	if err != nil {
		t.Fatalf("search memory: %v", err)
	}
	if len(matches) != 1 || matches[0].RunID != first.RunID || len(matches[0].Content) > 300 {
		t.Fatalf("budgeted matches = %#v", matches)
	}
	documents, err := store.loadAllRollouts()
	if err != nil || len(documents) != 3 {
		t.Fatalf("idempotent rollout collection = %#v / %v", documents, err)
	}
}

// TestCollectionLifecyclePreservesSourceAndNeverUpgradesConclusion 验证失效、删除和结论强度约束。
func TestCollectionLifecyclePreservesSourceAndNeverUpgradesConclusion(t *testing.T) {
	store := NewStore(t.TempDir())
	state := memoryTestRun("run_lifecycle", "go-chat", "local", "suspected")
	if _, err := store.UpdateForRun(state); err != nil {
		t.Fatalf("collect run: %v", err)
	}
	if err := store.Correct(state.RunID, "undetermined", "缺少当前实例身份核对"); err != nil {
		t.Fatalf("correct memory: %v", err)
	}
	document, err := store.loadRollout(state.RunID)
	if err != nil {
		t.Fatalf("load corrected rollout: %v", err)
	}
	if document.SourceConclusionStatus != "suspected" || document.ConclusionStatus != "undetermined" || document.CorrectionNote == "" {
		t.Fatalf("corrected document = %#v", document)
	}
	if err := store.Correct(state.RunID, "identified", "不能在整理时升级"); err == nil || !strings.Contains(err.Error(), "cannot upgrade") {
		t.Fatalf("upgrade error = %v", err)
	}
	if err := store.Invalidate(state.RunID, "目标实例已迁移"); err != nil {
		t.Fatalf("invalidate memory: %v", err)
	}
	if hints, err := store.Hints(Query{Service: "go-chat", Environment: "local"}); err != nil || len(hints) != 0 {
		t.Fatalf("invalidated hints = %#v / %v", hints, err)
	}
	if err := store.Delete(state.RunID, "移除过期排障经验"); err != nil {
		t.Fatalf("delete memory: %v", err)
	}
	document, err = store.loadRollout(state.RunID)
	if err != nil || document.CollectionStatus != CollectionDeleted {
		t.Fatalf("deleted document = %#v / %v", document, err)
	}
	if _, err := store.UpdateForRun(state); err != nil {
		t.Fatalf("recollect deleted run: %v", err)
	}
	document, err = store.loadRollout(state.RunID)
	if err != nil || document.CollectionStatus != CollectionDeleted {
		t.Fatalf("automatic recollection restored deleted knowledge: %#v / %v", document, err)
	}
}

// TestRebuildRefusesManualIndexEditsUntilExplicitForce 验证人工修改必须经显式覆盖才能替换。
func TestRebuildRefusesManualIndexEditsUntilExplicitForce(t *testing.T) {
	store := NewStore(t.TempDir())
	state := memoryTestRun("run_manual", "go-chat", "local", "undetermined")
	if _, err := store.UpdateForRun(state); err != nil {
		t.Fatalf("collect run: %v", err)
	}
	indexPath := filepath.Join(store.dir, indexFileName)
	data, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	if err := os.WriteFile(indexPath, append(data, []byte("\n人工编辑\n")...), 0o600); err != nil {
		t.Fatalf("edit index: %v", err)
	}
	if err := store.Rebuild(); err == nil || !strings.Contains(err.Error(), "manually changed") {
		t.Fatalf("rebuild error = %v", err)
	}
	current, err := os.ReadFile(indexPath)
	if err != nil || !strings.Contains(string(current), "人工编辑") {
		t.Fatalf("manual index edit was overwritten: %q / %v", current, err)
	}
	if err := store.RebuildForce(); err != nil {
		t.Fatalf("force rebuild: %v", err)
	}
	current, err = os.ReadFile(indexPath)
	if err != nil || strings.Contains(string(current), "人工编辑") {
		t.Fatalf("force rebuild did not replace generated index: %q / %v", current, err)
	}
	rolloutPath, err := store.rolloutPath(state.RunID)
	if err != nil {
		t.Fatalf("rollout path: %v", err)
	}
	rollout, err := os.ReadFile(rolloutPath)
	if err != nil {
		t.Fatalf("read rollout: %v", err)
	}
	if err := os.WriteFile(rolloutPath, append(rollout, []byte("\n人工编辑复盘\n")...), 0o600); err != nil {
		t.Fatalf("edit rollout: %v", err)
	}
	if _, err := store.UpdateForRun(state); err == nil || !strings.Contains(err.Error(), "unverified rollout") {
		t.Fatalf("normal recollection error = %v", err)
	}
	if _, err := store.UpdateForRunForce(state); err != nil {
		t.Fatalf("force recollection: %v", err)
	}
	current, err = os.ReadFile(rolloutPath)
	if err != nil || strings.Contains(string(current), "人工编辑复盘") {
		t.Fatalf("force recollection did not replace rollout: %q / %v", current, err)
	}
}

// TestConcurrentUpdatesSerializeIndexRebuilds 验证多个存储实例不会互相覆盖索引。
func TestConcurrentUpdatesSerializeIndexRebuilds(t *testing.T) {
	dir := t.TempDir()
	states := []runstore.State{
		memoryTestRun("run_parallel_a", "go-chat", "local", "suspected"),
		memoryTestRun("run_parallel_b", "go-chat", "local", "undetermined"),
	}
	start := make(chan struct{})
	errors := make(chan error, len(states))
	var group sync.WaitGroup
	for _, state := range states {
		state := state
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, err := NewStore(dir).UpdateForRun(state)
			errors <- err
		}()
	}
	close(start)
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatalf("concurrent update: %v", err)
		}
	}
	documents, err := NewStore(dir).loadAllRollouts()
	if err != nil || len(documents) != 2 {
		t.Fatalf("rollouts after concurrent updates = %#v / %v", documents, err)
	}
	if err := NewStore(dir).Rebuild(); err != nil {
		t.Fatalf("idempotent rebuild: %v", err)
	}
}

// TestRebuildRecoversFromStaleLock 验证异常退出遗留的陈旧锁不会永久阻塞索引恢复。
func TestRebuildRecoversFromStaleLock(t *testing.T) {
	store := NewStore(t.TempDir())
	if err := store.ensureRoot(); err != nil {
		t.Fatalf("create memory root: %v", err)
	}
	lockPath := filepath.Join(store.dir, lockFileName)
	if err := os.WriteFile(lockPath, []byte("abandoned-lock"), 0o600); err != nil {
		t.Fatalf("write stale lock: %v", err)
	}
	staleAt := time.Now().Add(-lockStaleAfter - time.Second)
	if err := os.Chtimes(lockPath, staleAt, staleAt); err != nil {
		t.Fatalf("age stale lock: %v", err)
	}
	if err := store.Rebuild(); err != nil {
		t.Fatalf("rebuild with stale lock: %v", err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("stale lock remains after rebuild: %v", err)
	}
}

// TestIneligibleRunDoesNotCreateKnowledge 验证没有实际工具观察的 run 不会被收录。
func TestIneligibleRunDoesNotCreateKnowledge(t *testing.T) {
	store := NewStore(t.TempDir())
	state := memoryTestRun("run_empty", "go-chat", "local", "undetermined")
	state.Trace = nil
	updated, err := store.UpdateForRun(state)
	if err != nil || updated {
		t.Fatalf("ineligible update = %v / %v", updated, err)
	}
	if _, err := os.Stat(store.rolloutDir()); !os.IsNotExist(err) {
		t.Fatalf("ineligible run created rollout directory: %v", err)
	}
}

// memoryTestRun 构造含脱敏边界和来源引用的最小可收录终态 run。
func memoryTestRun(runID, service, environment, conclusionStatus string) runstore.State {
	createdAt := time.Unix(100, 0).UTC()
	updatedAt := time.Unix(200, 0).UTC()
	return runstore.State{
		RunID:       runID,
		SessionID:   "session_memory_test",
		Service:     service,
		Environment: environment,
		Goal:        "检查 Redis 连接状态",
		Status:      runstore.StatusCompleted,
		Trace: []trace.Entry{{
			Step:     1,
			CallID:   "call_" + runID,
			ToolName: "redis_ping",
			Result: schema.Observation{
				Tool:    "redis_ping",
				Summary: "Redis 返回 PONG",
			},
		}},
		Diagnosis: &schema.Diagnosis{
			Summary:   "Redis 连接可达，但历史结论仍需重查当前实例。",
			RootCause: &schema.RootCause{Status: conclusionStatus, Statement: "Redis 实例身份需要结合当前配置核对。"},
			Evidence:  []schema.Evidence{{Step: 1, Tool: "redis_ping", Summary: "Redis 返回 PONG"}},
			PendingVerifications: []schema.PendingVerification{{
				Question: "当前应用是否连接预期 Redis 实例？",
			}},
		},
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	}
}

func TestMemoryTextNormalizesInvalidUTF8(t *testing.T) {
	input := "Redis " + string([]byte{0xd3, 0xc3, 0xbb, 0xa7}) + " 连接失败"
	got := normalizedText(input, 100)
	if !utf8.ValidString(got) {
		t.Fatalf("normalized memory text is invalid UTF-8: %q", got)
	}
	if !strings.Contains(got, "�") {
		t.Fatalf("invalid bytes were not marked: %q", got)
	}
	if clipped := truncateBytes("正常中文文本", 8); !utf8.ValidString(clipped) {
		t.Fatalf("truncated memory text is invalid UTF-8: %q", clipped)
	}
}
