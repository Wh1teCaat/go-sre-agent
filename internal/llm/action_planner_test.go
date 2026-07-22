package llm

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/y2/go-sre-agent/internal/schema"
)

const testSkillContent = "test skill: return JSON; cover evidence; use X-Request-ID and request_id from observations"

func TestRequestJSONUsesPlannerFieldNames(t *testing.T) {
	data, err := json.Marshal(Request{
		Goal: "诊断登录 500",
		Step: 2,
		TargetContext: map[string]any{
			"backend_base_url": "http://localhost:8080",
		},
		Observations: []schema.Observation{{Step: 1, PlanItemID: "backend", Tool: "http_check", Summary: "returned 200"}},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	text := string(data)
	for _, want := range []string{`"goal"`, `"step"`, `"target_context"`, `"tools"`, `"observations"`, `"plan_item_id":"backend"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("request json missing %s: %s", want, text)
		}
	}
	for _, notWant := range []string{`"Goal"`, `"Step"`, `"TargetContext"`, `"Tools"`, `"Observations"`, `"Trace"`, `"required_summary_sections"`} {
		if strings.Contains(text, notWant) {
			t.Fatalf("request json contains exported field name %s: %s", notWant, text)
		}
	}
	if strings.Contains(text, `"trace"`) {
		t.Fatalf("request json contains trace, want LLM-facing observations only: %s", text)
	}
}

func TestActionPlannerBuildsGenericChatRequestAndParsesAction(t *testing.T) {
	client := &captureChatClient{
		response: `{"type":"final","thought_summary":"enough evidence","final":{"summary":"planner parsed action"}}`,
	}
	planner := NewActionPlanner(client, ActionPlannerConfig{
		Model:       "gpt-4o-mini",
		Temperature: 0.2,
		Skill:       testSkillContent,
	})

	decision, err := planner.Next(context.Background(), Request{
		Goal: "只验证 planner 分层",
		Step: 1,
		TargetContext: map[string]any{
			"backend_base_url": "http://localhost:8080",
		},
	})
	if err != nil {
		t.Fatalf("next action: %v", err)
	}

	if client.calls != 1 {
		t.Fatalf("chat calls = %d, want 1", client.calls)
	}
	if client.request.Model != "gpt-4o-mini" {
		t.Fatalf("model = %q, want gpt-4o-mini", client.request.Model)
	}
	if client.request.OutputMode != OutputJSON {
		t.Fatalf("output mode = %q, want json", client.request.OutputMode)
	}
	if client.request.Temperature != 0.2 {
		t.Fatalf("temperature = %v, want 0.2", client.request.Temperature)
	}
	if len(client.request.Messages) != 2 {
		t.Fatalf("messages length = %d, want 2", len(client.request.Messages))
	}
	if client.request.Messages[0].Role != RoleSystem {
		t.Fatalf("first role = %q, want system", client.request.Messages[0].Role)
	}
	if client.request.Messages[0].Content != testSkillContent {
		t.Fatalf("system skill = %q, want %q", client.request.Messages[0].Content, testSkillContent)
	}
	if client.request.Messages[1].Role != RoleUser {
		t.Fatalf("second role = %q, want user", client.request.Messages[1].Role)
	}
	for _, want := range []string{"只验证 planner 分层", `"mode": "decision"`, `"target_context"`} {
		if !strings.Contains(client.request.Messages[1].Content, want) {
			t.Fatalf("user message missing %q: %q", want, client.request.Messages[1].Content)
		}
	}
	if strings.Contains(client.request.Messages[1].Content, "Diagnostic context JSON:") {
		t.Fatalf("user message contains source prompt prefix: %q", client.request.Messages[1].Content)
	}
	if decision.Action == nil || decision.Action.Type != schema.ActionTypeFinal {
		t.Fatalf("decision = %#v, want final action", decision)
	}
	if decision.Action.Final == nil || decision.Action.Final.Summary != "planner parsed action" {
		t.Fatalf("final diagnosis = %#v", decision.Action.Final)
	}
}

func TestActionPlannerParsesPlanningDecision(t *testing.T) {
	client := &captureChatClient{response: `{"needs_plan":true}`}
	planner := NewActionPlanner(client, ActionPlannerConfig{Model: "gpt-4o-mini", Skill: testSkillContent})

	decision, err := planner.Next(context.Background(), Request{Goal: "检查多个依赖", Step: 1})
	if err != nil {
		t.Fatalf("next decision: %v", err)
	}
	if !decision.NeedsPlan || decision.Action != nil {
		t.Fatalf("decision = %#v, want planning request", decision)
	}

	client.response = `{"needs_plan":true,"type":"final"}`
	if _, err := planner.Next(context.Background(), Request{Goal: "检查多个依赖", Step: 1}); err == nil {
		t.Fatal("mixed planning/action decision succeeded")
	}
}

func TestActionPlannerParsesOptionalPlan(t *testing.T) {
	client := &captureChatClient{response: `{"plan":{"reason":"需要检查后端","items":[{"id":"backend","goal":"检查后端"}]}}`}
	planner := NewActionPlanner(client, ActionPlannerConfig{Model: "gpt-4o-mini", Skill: testSkillContent})

	plan, err := planner.Plan(context.Background(), Request{Goal: "检查后端"})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan == nil || len(plan.Items) != 1 || plan.Items[0].ID != "backend" {
		t.Fatalf("plan = %#v", plan)
	}
	if client.request.Messages[0].Content != testSkillContent {
		t.Fatalf("plan skill = %q, want %q", client.request.Messages[0].Content, testSkillContent)
	}
	if !strings.Contains(client.request.Messages[1].Content, `"mode": "plan"`) {
		t.Fatalf("plan context missing mode: %q", client.request.Messages[1].Content)
	}
}

func TestActionPlannerParsesActionFromMarkdownJSONFence(t *testing.T) {
	client := &captureChatClient{
		response: "```json\n{\"type\":\"final\",\"thought_summary\":\"enough evidence\",\"final\":{\"summary\":\"parsed fenced action\"}}\n```",
	}
	planner := NewActionPlanner(client, ActionPlannerConfig{Model: "gpt-4o-mini", Skill: testSkillContent})

	decision, err := planner.Next(context.Background(), Request{
		Goal: "验证 fenced JSON",
		Step: 1,
	})
	if err != nil {
		t.Fatalf("next action: %v", err)
	}

	if decision.Action == nil || decision.Action.Type != schema.ActionTypeFinal {
		t.Fatalf("decision = %#v, want final action", decision)
	}
	if decision.Action.Final == nil || decision.Action.Final.Summary != "parsed fenced action" {
		t.Fatalf("final diagnosis = %#v", decision.Action.Final)
	}
}

func TestActionPlannerDecodeErrorIncludesShortContentPreview(t *testing.T) {
	longContent := strings.Repeat("not json ", 80)
	client := &captureChatClient{
		response: longContent,
	}
	planner := NewActionPlanner(client, ActionPlannerConfig{Model: "gpt-4o-mini", Skill: testSkillContent})

	_, err := planner.Next(context.Background(), Request{
		Goal: "验证错误提示",
		Step: 1,
	})
	if err == nil {
		t.Fatal("next action succeeded, want decode error")
	}

	errText := err.Error()
	if !strings.Contains(errText, "content_preview=") {
		t.Fatalf("error missing content preview: %v", err)
	}
	if strings.Contains(errText, longContent) {
		t.Fatalf("error contains full model content, want truncated preview: %v", err)
	}
}

type captureChatClient struct {
	request  ChatRequest
	response string
	calls    int
}

func (c *captureChatClient) Chat(ctx context.Context, request ChatRequest) (string, error) {
	c.calls++
	c.request = request
	return c.response, nil
}
