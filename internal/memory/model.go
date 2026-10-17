package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/y2/go-sre-agent/internal/llm"
	runstore "github.com/y2/go-sre-agent/internal/run"
)

const modelGeneration = "go-sre-agent/model-memory-v2"

// SourceRef identifies an actual tool observation in a saved run.
type SourceRef struct {
	RunID  string `json:"run_id"`
	Step   int    `json:"step"`
	Tool   string `json:"tool"`
	CallID string `json:"call_id"`
}

type Candidate struct {
	Keywords             []string    `json:"keywords,omitempty"`
	Text                 string      `json:"text"`
	Applicability        string      `json:"applicability"`
	PendingVerifications []string    `json:"pending_verifications"`
	Sources              []SourceRef `json:"sources"`
}

type Extraction struct {
	RolloutSummary   string      `json:"rollout_summary,omitempty"`
	SkippedReason    string      `json:"skipped_reason,omitempty"`
	Generation       string      `json:"generation"`
	Kind             string      `json:"kind"`
	ContentDigest    string      `json:"content_digest"`
	RunID            string      `json:"run_id"`
	SourceDigest     string      `json:"source_digest"`
	RolloutDigest    string      `json:"rollout_digest"`
	Model            string      `json:"model"`
	GeneratedAt      time.Time   `json:"generated_at"`
	Service          string      `json:"service"`
	Environment      string      `json:"environment"`
	Outcome          string      `json:"outcome"`
	ConclusionStatus string      `json:"conclusion_status"`
	Candidates       []Candidate `json:"candidates"`
}

type ConsolidatedItem struct {
	Summary       string   `json:"summary,omitempty"`
	Keywords      []string `json:"keywords,omitempty"`
	Topic         string   `json:"topic"`
	Knowledge     string   `json:"knowledge"`
	Applicability string   `json:"applicability"`
	Conflicts     []string `json:"conflicts"`
	SourceRunIDs  []string `json:"source_run_ids"`
}

type ExtractionInput struct {
	RunID  string `json:"run_id"`
	Digest string `json:"digest"`
}

type Consolidation struct {
	Generation    string             `json:"generation"`
	Kind          string             `json:"kind"`
	ContentDigest string             `json:"content_digest"`
	Service       string             `json:"service"`
	Environment   string             `json:"environment"`
	InputDigest   string             `json:"input_digest"`
	Inputs        []ExtractionInput  `json:"inputs"`
	Model         string             `json:"model"`
	GeneratedAt   time.Time          `json:"generated_at"`
	Items         []ConsolidatedItem `json:"items"`
}

type ProcessOptions struct {
	RunDir             string
	ExtractModel       string
	ConsolidationModel string
	Timeout            time.Duration
	Limit              int
	RunID              string
	DryRun             bool
}

type ProcessStats struct {
	Success int `json:"success"`
	Skipped int `json:"skipped"`
	Stale   int `json:"stale"`
	Failed  int `json:"failed"`
}

type extractResponse struct {
	RolloutSummary string      `json:"rollout_summary,omitempty"`
	SkippedReason  string      `json:"skipped_reason,omitempty"`
	Candidates     []Candidate `json:"candidates"`
}
type consolidateResponse struct {
	Items []ConsolidatedItem `json:"items"`
}

func digestJSON(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func (e Extraction) checksum() string              { e.ContentDigest = ""; return digestJSON(e) }
func (c Consolidation) checksum() string           { c.ContentDigest = ""; return digestJSON(c) }
func scopeHash(service, environment string) string { return digestJSON([]string{service, environment}) }
func (s *Store) extractionPath(runID string) string {
	return filepath.Join(s.dir, "extractions", runID+".json")
}
func (s *Store) consolidationPath(service, environment string) string {
	return filepath.Join(s.dir, "consolidations", scopeHash(service, environment)+".json")
}

func decodeStrict(data []byte, target any) error {
	if len(data) > 512*1024 {
		return fmt.Errorf("model memory JSON exceeds size limit")
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("trailing JSON data")
	}
	return nil
}
func limited(value string, max int) bool {
	return strings.TrimSpace(value) != "" && len(value) <= max && value == memoryText(value)
}
func validateCandidates(items []Candidate, state runstore.State) error {
	if len(items) > 128 {
		return fmt.Errorf("candidate count must be 0..128")
	}
	for _, item := range items {
		if !limited(item.Text, 8192) || !limited(item.Applicability, 2048) || len(item.PendingVerifications) > 16 || len(item.Sources) == 0 || len(item.Sources) > 64 || !validKeywords(item.Keywords) {
			return fmt.Errorf("invalid candidate size or missing sources")
		}
		for _, pending := range item.PendingVerifications {
			if !limited(pending, 2048) {
				return fmt.Errorf("invalid pending verification")
			}
		}
		for _, source := range item.Sources {
			if source.RunID != state.RunID {
				return fmt.Errorf("cross-run source")
			}
			found := false
			for _, entry := range state.Trace {
				if entry.Step == source.Step && entry.ToolName == source.Tool && entry.CallID == source.CallID && entry.ToolName != "" {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("fabricated source step/tool/call_id")
			}
		}
	}
	return nil
}
func validateItems(items []ConsolidatedItem, valid map[string]Extraction) error {
	if items == nil || len(items) > 64 {
		return fmt.Errorf("items must be an explicit array with 0..64 entries")
	}
	seen := map[string]bool{}
	for _, item := range items {
		if seen[item.Topic] {
			return fmt.Errorf("duplicate consolidated topic")
		}
		seen[item.Topic] = true
		if !limited(item.Topic, 240) || !limited(item.Knowledge, 16384) || !limited(item.Applicability, 2048) || len(item.Conflicts) > 16 || len(item.SourceRunIDs) == 0 || len(item.SourceRunIDs) > 128 || !validKeywords(item.Keywords) || (item.Summary != "" && !limited(item.Summary, 1200)) {
			return fmt.Errorf("invalid consolidated item")
		}
		for _, conflict := range item.Conflicts {
			if !limited(conflict, 2048) {
				return fmt.Errorf("invalid conflict")
			}
		}
		for _, runID := range item.SourceRunIDs {
			if _, ok := valid[runID]; !ok {
				return fmt.Errorf("consolidation cites run outside current scope: %s", runID)
			}
		}
	}
	return nil
}
func (s *Store) loadExtraction(runID string) (Extraction, error) {
	if !safeRunID(runID) {
		return Extraction{}, fmt.Errorf("unsafe run id")
	}
	data, err := os.ReadFile(s.extractionPath(runID))
	if err != nil {
		return Extraction{}, err
	}
	var e Extraction
	if err := decodeStrict(data, &e); err != nil {
		return Extraction{}, err
	}
	if (e.Generation != modelGeneration && e.Generation != "go-sre-agent/model-memory-v1") || e.Kind != "extraction" || e.RunID != runID || e.ContentDigest != e.checksum() {
		return Extraction{}, fmt.Errorf("invalid extraction digest or metadata")
	}
	return e, nil
}
func (s *Store) loadConsolidation(service, environment string) (Consolidation, error) {
	data, err := os.ReadFile(s.consolidationPath(service, environment))
	if err != nil {
		return Consolidation{}, err
	}
	var c Consolidation
	if err := decodeStrict(data, &c); err != nil {
		return Consolidation{}, err
	}
	if (c.Generation != modelGeneration && c.Generation != "go-sre-agent/model-memory-v1") || c.Kind != "consolidation" || c.Service != service || c.Environment != environment || c.ContentDigest != c.checksum() {
		return Consolidation{}, fmt.Errorf("invalid consolidation digest or metadata")
	}
	return c, nil
}
func (s *Store) validExtraction(doc rolloutDocument, runDir string) (Extraction, bool) {
	if doc.CollectionStatus != CollectionActive {
		return Extraction{}, false
	}
	state, err := runstore.NewStore(runDir).Load(doc.RunID)
	if err != nil || !eligible(state) || stateDigest(state) != doc.SourceDigest || scopeValue(state.Service) != doc.Service || scopeValue(state.Environment) != doc.Environment {
		return Extraction{}, false
	}
	e, err := s.loadExtraction(doc.RunID)
	if err != nil || e.Generation != modelGeneration || e.SourceDigest != doc.SourceDigest || e.RolloutDigest != doc.ContentDigest || e.Service != doc.Service || e.Environment != doc.Environment || e.Outcome != doc.Outcome || e.ConclusionStatus != doc.ConclusionStatus || validateExtraction(extractResponse{Candidates: e.Candidates, RolloutSummary: e.RolloutSummary, SkippedReason: e.SkippedReason}, state) != nil {
		return Extraction{}, false
	}
	return e, true
}
func inputList(valid map[string]Extraction) []ExtractionInput {
	inputs := make([]ExtractionInput, 0, len(valid))
	for id, e := range valid {
		inputs = append(inputs, ExtractionInput{RunID: id, Digest: e.ContentDigest})
	}
	sort.Slice(inputs, func(i, j int) bool { return inputs[i].RunID < inputs[j].RunID })
	return inputs
}
func (s *Store) validScope(docs []rolloutDocument, runDir, service, environment string) (map[string]Extraction, []ExtractionInput) {
	valid := map[string]Extraction{}
	for _, doc := range docs {
		if doc.Service == service && doc.Environment == environment {
			if e, ok := s.validExtraction(doc, runDir); ok {
				valid[doc.RunID] = e
			}
		}
	}
	return valid, inputList(valid)
}
func (s *Store) validConsolidation(docs []rolloutDocument, runDir, service, environment string) (Consolidation, bool) {
	valid, inputs := s.validScope(docs, runDir, service, environment)
	if len(inputs) == 0 {
		return Consolidation{}, false
	}
	c, err := s.loadConsolidation(service, environment)
	if err != nil || c.Generation != modelGeneration || c.InputDigest != digestJSON(inputs) || digestJSON(c.Inputs) != digestJSON(inputs) || validateItems(c.Items, valid) != nil {
		return Consolidation{}, false
	}
	return c, true
}

func lease(path string, duration time.Duration) (func(), bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			token := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
			_, _ = f.WriteString(token)
			_ = f.Close()
			return func() { releaseWriteLock(path, token) }, true, nil
		}
		if !os.IsExist(err) {
			return nil, false, err
		}
		if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) > duration*2+time.Minute {
			_ = os.Remove(path)
			continue
		}
		return nil, false, nil
	}
	return nil, false, nil
}
func callModel(ctx context.Context, client llm.ChatClient, model, instruction string, input any, timeout time.Duration, output any) error {
	data, err := json.Marshal(input)
	if err != nil {
		return err
	}
	if len(data) > 256*1024 {
		return fmt.Errorf("model input exceeds 256 KiB; source was not silently truncated")
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	response, err := client.Chat(requestCtx, llm.ChatRequest{Model: model, OutputMode: llm.OutputJSON, Messages: []llm.Message{{Role: llm.RoleSystem, Content: instruction}, {Role: llm.RoleUser, Content: string(data)}}})
	if err != nil {
		return err
	}
	return decodeStrict([]byte(response), output)
}

// Process 每次从已验证文件中确定待处理任务，先提取单次运行经验，再按服务和环境归纳。
// 两阶段共享本轮处理额度；租约被占用时跳过，单项失败记录到统计中并继续处理其他任务。
func (s *Store) Process(ctx context.Context, client llm.ChatClient, opts ProcessOptions) (ProcessStats, error) {
	var stats ProcessStats
	if opts.RunDir == "" {
		opts.RunDir = s.runDir
	}
	// 只为本次处理设置运行目录，避免修改共享 Store 的配置。
	working := *s
	working.runDir = opts.RunDir
	s = &working
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Second
	}
	if opts.Limit <= 0 {
		opts.Limit = 2
	}
	if !opts.DryRun {
		if client == nil {
			return stats, fmt.Errorf("chat client is required")
		}
		if strings.TrimSpace(opts.ExtractModel) == "" || strings.TrimSpace(opts.ConsolidationModel) == "" {
			return stats, fmt.Errorf("extract and consolidation model names are required")
		}
	}
	docs, err := s.loadAllRollouts()
	if err != nil {
		return stats, err
	}
	// 第一阶段：仅处理 active 且缺少有效提取结果的复盘；已有损坏文件不自动覆盖。
	// attempts 统计进入处理的任务，后续归纳继续使用这一额度，而非重新计数。
	attempts := 0
	for _, doc := range docs {
		if attempts >= opts.Limit || ctx.Err() != nil {
			break
		}
		if doc.CollectionStatus != CollectionActive || opts.RunID != "" && opts.RunID != doc.RunID {
			continue
		}
		if _, ok := s.validExtraction(doc, opts.RunDir); ok {
			continue
		}
		if _, err := s.loadExtraction(doc.RunID); err != nil && !os.IsNotExist(err) {
			stats.Failed++
			continue
		}
		// 核对原始 run 与复盘的来源校验值及适用范围，避免使用过期复盘提取经验。
		state, err := runstore.NewStore(opts.RunDir).Load(doc.RunID)
		if err != nil || !eligible(state) || stateDigest(state) != doc.SourceDigest || scopeValue(state.Service) != doc.Service || scopeValue(state.Environment) != doc.Environment {
			stats.Stale++
			continue
		}
		attempts++
		// 预演只统计待处理任务，不调用模型、不写入结果。
		if opts.DryRun {
			stats.Skipped++
			continue
		}
		// 按 run 获取租约，并在取得后复查结果，避免其他处理者已完成时重复调用模型。
		release, acquired, err := lease(s.extractionPath(doc.RunID)+".lease", opts.Timeout)
		if err != nil {
			stats.Failed++
			continue
		}
		if !acquired {
			stats.Skipped++
			continue
		}
		if _, ok := s.validExtraction(doc, opts.RunDir); ok {
			release()
			stats.Skipped++
			continue
		}
		// 模型请求期间不持有全局写锁；输入已脱敏限长，输出必须通过格式和证据引用校验。
		response, err := extractRun(ctx, client, opts, state, doc)
		if err == nil {
			e := Extraction{Generation: modelGeneration, Kind: "extraction", RunID: doc.RunID, SourceDigest: doc.SourceDigest, RolloutDigest: doc.ContentDigest, Model: opts.ExtractModel, GeneratedAt: time.Now().UTC(), Service: doc.Service, Environment: doc.Environment, Outcome: doc.Outcome, ConclusionStatus: doc.ConclusionStatus, Candidates: response.Candidates, RolloutSummary: response.RolloutSummary, SkippedReason: response.SkippedReason}
			e.ContentDigest = e.checksum()
			// 模型返回后持写锁复查来源，若等待期间 run 或复盘变化，则丢弃本次过期结果。
			err = s.withWriteLock(func() error {
				current, err := s.loadRollout(doc.RunID)
				if err != nil {
					return err
				}
				latest, err := runstore.NewStore(opts.RunDir).Load(doc.RunID)
				if err != nil {
					return err
				}
				if current.ContentDigest != doc.ContentDigest || stateDigest(latest) != doc.SourceDigest || current.CollectionStatus != CollectionActive {
					return errStaleInput
				}
				encoded, err := json.MarshalIndent(e, "", "  ")
				if err != nil {
					return err
				}
				if len(encoded) > 512*1024 {
					return fmt.Errorf("extraction exceeds 512 KiB; no partial result saved")
				}
				// Store the detailed model review in the source rollout too. Rebind
				// the extraction to its new digest under the same write lock.
				current.ModelSummary = response.RolloutSummary
				if err := s.writeRollout(current, false); err != nil {
					return err
				}
				updated, err := s.loadRollout(doc.RunID)
				if err != nil {
					return err
				}
				e.RolloutDigest = updated.ContentDigest
				e.ContentDigest = e.checksum()
				data, _ := json.MarshalIndent(e, "", "  ")
				if err := writeGenerated(s.extractionPath(doc.RunID), data, false, func(old []byte) error {
					var prior Extraction
					if err := decodeStrict(old, &prior); err != nil {
						return err
					}
					if prior.ContentDigest != prior.checksum() {
						return fmt.Errorf("invalid extraction checksum")
					}
					return nil
				}); err != nil {
					return err
				}
				// 保存提取结果后刷新派生索引，使后续检索可以使用新增经验。
				return s.rebuildLocked(false)
			})
		}
		release()
		if errors.Is(err, errStaleInput) {
			stats.Stale++
		} else if err != nil {
			stats.Failed++
		} else {
			stats.Success++
		}
	}
	// 第二阶段：重新扫描，使本轮新增的有效提取结果也能参与归纳；提取已用完额度则留待下轮。
	docs, err = s.loadAllRollouts()
	if err != nil {
		return stats, err
	}
	seen := map[string]bool{}
	for _, doc := range docs {
		if attempts >= opts.Limit || ctx.Err() != nil {
			break
		}
		if doc.CollectionStatus != CollectionActive || opts.RunID != "" && opts.RunID != doc.RunID {
			continue
		}
		key := scopeHash(doc.Service, doc.Environment)
		if seen[key] {
			continue
		}
		seen[key] = true
		// 每个服务和环境只归纳一次，输入限定为该范围内有效的提取结果。
		valid, inputs := s.validScope(docs, opts.RunDir, doc.Service, doc.Environment)
		if len(inputs) == 0 {
			continue
		}
		if _, ok := s.validConsolidation(docs, opts.RunDir, doc.Service, doc.Environment); ok {
			continue
		}
		attempts++
		if opts.DryRun {
			stats.Skipped++
			continue
		}
		if _, err := s.loadConsolidation(doc.Service, doc.Environment); err != nil && !os.IsNotExist(err) {
			stats.Failed++
			continue
		}
		// 超过输入数量上限时记为失败，不静默截取部分 run 生成不完整的归纳。
		if len(inputs) > 128 {
			stats.Failed++
			continue
		}
		// 与 raw_memories.md 使用同一份完整原始经验投影，按来源摘要验证后整合。
		raw := rawDocument{}
		for _, source := range docs {
			if e, ok := valid[source.RunID]; ok {
				raw.Entries = append(raw.Entries, rawEntryFromExtraction(source, e))
			}
		}
		// 归纳租约按服务和环境隔离；取得后重新检查是否已有其他处理者生成有效结果。
		release, acquired, err := lease(s.consolidationPath(doc.Service, doc.Environment)+".lease", opts.Timeout)
		if err != nil {
			stats.Failed++
			continue
		}
		if !acquired {
			stats.Skipped++
			continue
		}
		currentDocs, scanErr := s.loadAllRollouts()
		if scanErr != nil {
			release()
			stats.Failed++
			continue
		}
		if _, ok := s.validConsolidation(currentDocs, opts.RunDir, doc.Service, doc.Environment); ok {
			release()
			stats.Skipped++
			continue
		}
		// 让模型对候选经验去重归纳、保留适用条件和冲突，再校验输出及来源 run 引用。
		var response consolidateResponse
		err = callModel(ctx, client, opts.ConsolidationModel, consolidationPrompt, map[string]any{"service": doc.Service, "environment": doc.Environment, "raw_memories": renderRawBody(raw)}, opts.Timeout, &response)
		if err == nil {
			normalizeConsolidation(&response)
			err = validateItems(response.Items, valid)
		}
		if err == nil {
			c := Consolidation{Generation: modelGeneration, Kind: "consolidation", Service: doc.Service, Environment: doc.Environment, InputDigest: digestJSON(inputs), Inputs: inputs, Model: opts.ConsolidationModel, GeneratedAt: time.Now().UTC(), Items: response.Items}
			c.ContentDigest = c.checksum()
			err = s.withWriteLock(func() error {
				current, err := s.loadAllRollouts()
				if err != nil {
					return err
				}
				// 写入前复查整个输入集合；新增、失效或变化的提取结果都会使本次归纳过期。
				_, now := s.validScope(current, opts.RunDir, doc.Service, doc.Environment)
				if digestJSON(now) != c.InputDigest {
					return errStaleInput
				}
				data, err := json.MarshalIndent(c, "", "  ")
				if err != nil {
					return err
				}
				if len(data) > 512*1024 {
					return fmt.Errorf("consolidation exceeds 512 KiB")
				}
				if err := writeGenerated(s.consolidationPath(doc.Service, doc.Environment), data, false, func(old []byte) error {
					var prior Consolidation
					if err := decodeStrict(old, &prior); err != nil {
						return err
					}
					if prior.ContentDigest != prior.checksum() {
						return fmt.Errorf("invalid consolidation checksum")
					}
					return nil
				}); err != nil {
					return err
				}
				return s.rebuildLocked(false)
			})
		}
		release()
		if errors.Is(err, errStaleInput) {
			stats.Stale++
		} else if err != nil {
			stats.Failed++
		} else {
			stats.Success++
		}
	}
	// 单项失败通过 stats.Failed 返回；此处另行返回上下文取消或超时错误。
	return stats, ctx.Err()
}

var errStaleInput = errors.New("model memory input changed during processing")

// modelHint reads only currently valid generated material; stale index entries cannot inject it.
func (s *Store) modelHint(doc rolloutDocument) string {
	e, ok := s.validExtraction(doc, s.runDir)
	if !ok {
		return ""
	}
	var out strings.Builder
	out.WriteString("历史模型候选经验（需用当前工具证据验证）：\n")
	for _, candidate := range e.Candidates {
		fmt.Fprintf(&out, "- %s；适用条件：%s；来源：", memoryText(candidate.Text), memoryText(candidate.Applicability))
		for i, ref := range candidate.Sources {
			if i > 0 {
				out.WriteString("、")
			}
			fmt.Fprintf(&out, "%s step %d tool %s call %s", ref.RunID, ref.Step, ref.Tool, ref.CallID)
		}
		out.WriteString("\n")
		for _, pending := range candidate.PendingVerifications {
			fmt.Fprintf(&out, "  待验证：%s\n", memoryText(pending))
		}
	}
	docs, err := s.loadAllRollouts()
	if err != nil {
		return out.String()
	}
	if c, ok := s.validConsolidation(docs, s.runDir, doc.Service, doc.Environment); ok {
		for _, item := range c.Items {
			contains := false
			for _, id := range item.SourceRunIDs {
				if id == doc.RunID {
					contains = true
					break
				}
			}
			if !contains {
				continue
			}
			fmt.Fprintf(&out, "- 整合主题 %s：%s；适用条件：%s；来源 run：%s\n", markdownText(item.Topic), memoryText(item.Knowledge), memoryText(item.Applicability), strings.Join(item.SourceRunIDs, ", "))
			for _, conflict := range item.Conflicts {
				fmt.Fprintf(&out, "  冲突：%s\n", memoryText(conflict))
			}
		}
	}
	return out.String()
}
