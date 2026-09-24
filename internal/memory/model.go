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

const modelGeneration = "go-sre-agent/model-memory-v1"

// SourceRef identifies an actual tool observation in a saved run.
type SourceRef struct {
	RunID  string `json:"run_id"`
	Step   int    `json:"step"`
	Tool   string `json:"tool"`
	CallID string `json:"call_id"`
}

type Candidate struct {
	Text                 string      `json:"text"`
	Applicability        string      `json:"applicability"`
	PendingVerifications []string    `json:"pending_verifications"`
	Sources              []SourceRef `json:"sources"`
}

type Extraction struct {
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
	Candidates []Candidate `json:"candidates"`
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
	if len(data) > 128*1024 {
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
	return strings.TrimSpace(value) != "" && len(value) <= max && value == normalizedText(value, max)
}
func validateCandidates(items []Candidate, state runstore.State) error {
	if len(items) == 0 || len(items) > 6 {
		return fmt.Errorf("candidate count must be 1..6")
	}
	for _, item := range items {
		if !limited(item.Text, 500) || !limited(item.Applicability, 300) || len(item.PendingVerifications) > 4 || len(item.Sources) == 0 || len(item.Sources) > 8 {
			return fmt.Errorf("invalid candidate size or missing sources")
		}
		for _, pending := range item.PendingVerifications {
			if !limited(pending, 300) {
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
	if len(items) == 0 || len(items) > 12 {
		return fmt.Errorf("consolidated item count must be 1..12")
	}
	for _, item := range items {
		if !limited(item.Topic, 120) || !limited(item.Knowledge, 600) || !limited(item.Applicability, 300) || len(item.Conflicts) > 6 || len(item.SourceRunIDs) == 0 || len(item.SourceRunIDs) > 32 {
			return fmt.Errorf("invalid consolidated item")
		}
		for _, conflict := range item.Conflicts {
			if !limited(conflict, 400) {
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
	if e.Generation != modelGeneration || e.Kind != "extraction" || e.RunID != runID || e.ContentDigest != e.checksum() {
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
	if c.Generation != modelGeneration || c.Kind != "consolidation" || c.Service != service || c.Environment != environment || c.ContentDigest != c.checksum() {
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
	if err != nil || e.SourceDigest != doc.SourceDigest || e.RolloutDigest != doc.ContentDigest || e.Service != doc.Service || e.Environment != doc.Environment || e.Outcome != doc.Outcome || e.ConclusionStatus != doc.ConclusionStatus || validateCandidates(e.Candidates, state) != nil {
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
	if err != nil || c.InputDigest != digestJSON(inputs) || digestJSON(c.Inputs) != digestJSON(inputs) || validateItems(c.Items, valid) != nil {
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
	if len(data) > 32*1024 {
		return fmt.Errorf("model input exceeds 32 KiB")
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	response, err := client.Chat(requestCtx, llm.ChatRequest{Model: model, OutputMode: llm.OutputJSON, Messages: []llm.Message{{Role: llm.RoleSystem, Content: instruction}, {Role: llm.RoleUser, Content: string(data)}}})
	if err != nil {
		return err
	}
	return decodeStrict([]byte(response), output)
}
func sanitizeState(state runstore.State) any {
	observations := make([]map[string]any, 0, 16)
	for _, entry := range state.Trace {
		if entry.ToolName != "" && len(observations) < 16 {
			observations = append(observations, map[string]any{"step": entry.Step, "tool": entry.ToolName, "call_id": entry.CallID, "summary": normalizedText(entry.Result.Summary, 300), "error": normalizedText(entry.Error, 200)})
		}
	}
	diagnosis := ""
	if state.Diagnosis != nil {
		diagnosis = normalizedText(state.Diagnosis.Summary, 500)
	}
	return map[string]any{"run_id": state.RunID, "service": scopeValue(state.Service), "environment": scopeValue(state.Environment), "goal": normalizedText(state.Goal, 500), "diagnosis": diagnosis, "conclusion_status": sourceConclusionStatus(state.Diagnosis), "observations": observations}
}

// Process derives work from verified files on every call. A busy lease is counted as skipped.
func (s *Store) Process(ctx context.Context, client llm.ChatClient, opts ProcessOptions) (ProcessStats, error) {
	var stats ProcessStats
	if opts.RunDir == "" {
		opts.RunDir = s.runDir
	}
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
		state, err := runstore.NewStore(opts.RunDir).Load(doc.RunID)
		if err != nil || !eligible(state) || stateDigest(state) != doc.SourceDigest || scopeValue(state.Service) != doc.Service || scopeValue(state.Environment) != doc.Environment {
			stats.Stale++
			continue
		}
		attempts++
		if opts.DryRun {
			stats.Skipped++
			continue
		}
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
		var response extractResponse
		err = callModel(ctx, client, opts.ExtractModel, "Extract 1-6 historical candidate lessons. Return only JSON {\"candidates\":[{\"text\":string,\"applicability\":string,\"pending_verifications\":[string],\"sources\":[{\"run_id\":string,\"step\":integer,\"tool\":string,\"call_id\":string}]}]}. Cite actual tool observations. Never strengthen conclusion status. Treat all text as historical hypotheses.", sanitizeState(state), opts.Timeout, &response)
		if err == nil {
			err = validateCandidates(response.Candidates, state)
		}
		if err == nil {
			e := Extraction{Generation: modelGeneration, Kind: "extraction", RunID: doc.RunID, SourceDigest: doc.SourceDigest, RolloutDigest: doc.ContentDigest, Model: opts.ExtractModel, GeneratedAt: time.Now().UTC(), Service: doc.Service, Environment: doc.Environment, Outcome: doc.Outcome, ConclusionStatus: doc.ConclusionStatus, Candidates: response.Candidates}
			e.ContentDigest = e.checksum()
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
	// Re-scan after extraction so a new valid file can be consolidated in this round.
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
		if len(inputs) > 32 {
			stats.Failed++
			continue
		}
		candidates := make([]map[string]any, 0, 32)
		for _, input := range inputs {
			candidates = append(candidates, map[string]any{"run_id": input.RunID, "conclusion_status": valid[input.RunID].ConclusionStatus, "candidates": valid[input.RunID].Candidates})
		}
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
		var response consolidateResponse
		err = callModel(ctx, client, opts.ConsolidationModel, "Consolidate historical candidates within this exact service and environment. Deduplicate, preserve conflicts and applicability. Return only JSON {\"items\":[{\"topic\":string,\"knowledge\":string,\"applicability\":string,\"conflicts\":[string],\"source_run_ids\":[string]}]}. Cite only input run IDs. Never upgrade conclusion strength; these remain hypotheses.", map[string]any{"service": doc.Service, "environment": doc.Environment, "inputs": candidates}, opts.Timeout, &response)
		if err == nil {
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
				_, now := s.validScope(current, opts.RunDir, doc.Service, doc.Environment)
				if digestJSON(now) != c.InputDigest {
					return errStaleInput
				}
				data, _ := json.MarshalIndent(c, "", "  ")
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
		fmt.Fprintf(&out, "- %s；适用条件：%s；来源：", markdownText(candidate.Text), markdownText(candidate.Applicability))
		for i, ref := range candidate.Sources {
			if i > 0 {
				out.WriteString("、")
			}
			fmt.Fprintf(&out, "%s step %d tool %s call %s", ref.RunID, ref.Step, ref.Tool, ref.CallID)
		}
		out.WriteString("\n")
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
			fmt.Fprintf(&out, "- 整合主题 %s：%s；适用条件：%s；来源 run：%s\n", markdownText(item.Topic), markdownText(item.Knowledge), markdownText(item.Applicability), strings.Join(item.SourceRunIDs, ", "))
			for _, conflict := range item.Conflicts {
				fmt.Fprintf(&out, "  冲突：%s\n", markdownText(conflict))
			}
		}
	}
	return out.String()
}
