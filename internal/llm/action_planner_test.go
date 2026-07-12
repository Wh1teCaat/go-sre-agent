package llm

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/y2/go-sre-agent/internal/schema"
)

func TestRequestJSONUsesPlannerFieldNames(t *testing.T) {
	data, err := json.Marshal(Request{
		Goal: "诊断登录 500",
		Step: 2,
		TargetContext: map[string]any{
			"backend_base_url": "http://localhost:8080",
		},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	text := string(data)
	for _, want := range []string{`"goal"`, `"step"`, `"target_context"`, `"tools"`, `"observations"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("request json missing %s: %s", want, text)
		}
	}
	for _, notWant := range []string{`"Goal"`, `"Step"`, `"TargetContext"`, `"Tools"`, `"Observations"`, `"Trace"`} {
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
		response: ChatResponse{
			Content: `{"type":"final","thought_summary":"enough evidence","final":{"summary":"planner parsed action"}}`,
		},
	}
	planner := NewActionPlanner(client, ActionPlannerConfig{
		Model:       "gpt-4o-mini",
		Temperature: 0.2,
	})

	action, err := planner.NextAction(context.Background(), Request{
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
	for _, want := range []string{"plan", "coverage", "target_context", "memories", "historical hints only", "login_url", "postgres_ping", "postgres_check", "redis_ping", "websocket_check", "log_read", "中文", "do not finalize"} {
		if !strings.Contains(client.request.Messages[0].Content, want) {
			t.Fatalf("system prompt missing %q:\n%s", want, client.request.Messages[0].Content)
		}
	}
	if client.request.Messages[1].Role != RoleUser {
		t.Fatalf("second role = %q, want user", client.request.Messages[1].Role)
	}
	if !strings.Contains(client.request.Messages[1].Content, "只验证 planner 分层") {
		t.Fatalf("user message missing goal: %q", client.request.Messages[1].Content)
	}
	if !strings.Contains(client.request.Messages[1].Content, `"target_context"`) {
		t.Fatalf("user message missing target context: %q", client.request.Messages[1].Content)
	}
	if action.Type != schema.ActionTypeFinal {
		t.Fatalf("action type = %q, want final", action.Type)
	}
	if action.Final == nil || action.Final.Summary != "planner parsed action" {
		t.Fatalf("final diagnosis = %#v", action.Final)
	}
}

func TestActionPlannerParsesActionFromMarkdownJSONFence(t *testing.T) {
	client := &captureChatClient{
		response: ChatResponse{
			Content: "```json\n{\"type\":\"final\",\"thought_summary\":\"enough evidence\",\"final\":{\"summary\":\"parsed fenced action\"}}\n```",
		},
	}
	planner := NewActionPlanner(client, ActionPlannerConfig{Model: "gpt-4o-mini"})

	action, err := planner.NextAction(context.Background(), Request{
		Goal: "验证 fenced JSON",
		Step: 1,
	})
	if err != nil {
		t.Fatalf("next action: %v", err)
	}

	if action.Type != schema.ActionTypeFinal {
		t.Fatalf("action type = %q, want final", action.Type)
	}
	if action.Final == nil || action.Final.Summary != "parsed fenced action" {
		t.Fatalf("final diagnosis = %#v", action.Final)
	}
}

func TestActionPlannerDecodeErrorIncludesShortContentPreview(t *testing.T) {
	longContent := strings.Repeat("not json ", 80)
	client := &captureChatClient{
		response: ChatResponse{
			Content: longContent,
		},
	}
	planner := NewActionPlanner(client, ActionPlannerConfig{Model: "gpt-4o-mini"})

	_, err := planner.NextAction(context.Background(), Request{
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
	response ChatResponse
	calls    int
}

func (c *captureChatClient) Chat(ctx context.Context, request ChatRequest) (ChatResponse, error) {
	c.calls++
	c.request = request
	return c.response, nil
}
