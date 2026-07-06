package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/y2/go-sre-agent/internal/schema"
)

type ActionPlannerConfig struct {
	Model       string
	Temperature float64
}

// ActionPlanner 是 Agent 级 Provider 接口和通用 ChatClient 接口之间的桥梁，
// 负责 prompt 构造和 action JSON 解析。
type ActionPlanner struct {
	client ChatClient
	config ActionPlannerConfig
}

func NewActionPlanner(client ChatClient, config ActionPlannerConfig) *ActionPlanner {
	if config.Temperature == 0 {
		config.Temperature = 0.2
	}
	return &ActionPlanner{
		client: client,
		config: config,
	}
}

// NextAction 把当前诊断上下文转换成 chat 请求，调用配置好的模型 client，
// 再把返回的 JSON 解析成结构化 action。
func (p *ActionPlanner) NextAction(ctx context.Context, request Request) (schema.Action, error) {
	chatRequest, err := buildActionChatRequest(p.config, request)
	if err != nil {
		return schema.Action{}, err
	}

	response, err := p.client.Chat(ctx, chatRequest)
	if err != nil {
		return schema.Action{}, err
	}

	content := strings.TrimSpace(response.Content)
	if content == "" {
		return schema.Action{}, fmt.Errorf("llm response content is empty")
	}

	var action schema.Action
	// 有些兼容模型即使请求 JSON mode，也可能额外包一层说明或 Markdown fence。
	// 这里先提取出 JSON 起点，后续字段合法性仍交给 policy 层做强校验。
	actionJSON := extractActionJSON(content)
	if err := json.NewDecoder(strings.NewReader(actionJSON)).Decode(&action); err != nil {
		return schema.Action{}, fmt.Errorf("decode llm action json: %w; content_preview=%q", err, contentPreview(content, 240))
	}
	return action, nil
}

// buildActionChatRequest 故意保持 provider-neutral。厂商特有字段由后续选中的
// ChatClient 实现负责补齐。
func buildActionChatRequest(config ActionPlannerConfig, request Request) (ChatRequest, error) {
	// request 包含 goal、可用工具 schema、历史 observation。缩进后的 JSON
	// 更利于人工排查 prompt，也让测试中的断言更直观。
	contextJSON, err := json.MarshalIndent(request, "", "  ")
	if err != nil {
		return ChatRequest{}, fmt.Errorf("encode llm context: %w", err)
	}

	return ChatRequest{
		Model: config.Model,
		Messages: []Message{
			{
				Role:    RoleSystem,
				Content: actionPlannerSystemPrompt,
			},
			{
				Role:    RoleUser,
				Content: "Diagnostic context JSON:\n" + string(contextJSON),
			},
		},
		OutputMode:  OutputJSON,
		Temperature: config.Temperature,
	}, nil
}

const actionPlannerSystemPrompt = `You are an SRE diagnostic agent.

Return exactly one valid JSON object. Do not wrap it in Markdown.

Allowed JSON shapes:
1. Tool call:
{"type":"tool_call","thought_summary":"short evidence-seeking reason","tool":"tool_name","args":{}}
2. Final diagnosis:
{"type":"final","thought_summary":"short reason diagnosis is ready","final":{"summary":"evidence-based conclusion","evidence":[{"step":1,"tool":"tool_name","summary":"specific observed evidence"}],"recommendations":["actionable next step"]}}

Rules:
- Use only tool names listed in the diagnostic context JSON.
- Tool args must be a JSON object matching the chosen tool schema.
- A target_context observation may contain configured defaults such as backend_base_url, login_url, postgres_dsn, redis_addr, websocket_url, and log_file. Use it only as tool-argument context, not as diagnostic evidence.
- For chat_proj backend liveness goals, prefer http_check against backend_base_url or a health endpoint if the goal names one.
- For login 500 goals, prefer http_check against login_url with POST first, then log_read with an ERROR keyword if more evidence is needed.
- For PostgreSQL connectivity goals, use postgres_ping with postgres_dsn.
- For Redis connectivity goals, use redis_ping with redis_addr.
- For WebSocket failure goals, use websocket_check with websocket_url, then log_read with a websocket keyword if needed.
- For recent log/error goals, use log_read with log_file and an ERROR keyword.
- Use thought_summary for a short operational summary only; do not reveal hidden chain-of-thought.
- The final diagnosis must be based only on observations in the context.
- If a previous observation has an error field, treat it as evidence and either choose another useful read-only tool or produce a final diagnosis explaining the failed check.
- If evidence is insufficient, call one useful read-only tool instead of guessing.
- If no tool call is needed for a smoke test or the goal explicitly asks for a final-only response, return a final diagnosis.`

// extractActionJSON 对模型输出做轻量容错：去掉 Markdown 代码块，并从第一个 JSON 对象开始解析。
// 真正的结构和字段合法性仍交给 json decoder 与 policy validator 处理。
func extractActionJSON(content string) string {
	text := strings.TrimSpace(content)
	if fenced, ok := extractMarkdownFence(text); ok {
		text = strings.TrimSpace(fenced)
	}
	if index := strings.IndexByte(text, '{'); index >= 0 {
		text = text[index:]
	}
	return text
}

// extractMarkdownFence 提取模型偶尔包裹的 ```json 代码块内容。
// prompt 已要求不要包 Markdown，但这里保留兼容，避免小格式偏差导致整轮失败。
func extractMarkdownFence(content string) (string, bool) {
	start := strings.Index(content, "```")
	if start < 0 {
		return "", false
	}

	afterFence := content[start+3:]
	newline := strings.IndexByte(afterFence, '\n')
	if newline < 0 {
		return "", false
	}
	afterFence = afterFence[newline+1:]

	end := strings.Index(afterFence, "```")
	if end < 0 {
		return afterFence, true
	}
	return afterFence[:end], true
}

// contentPreview 压缩并截断模型原始输出，供解析错误使用，避免日志里塞入过长响应。
func contentPreview(content string, limit int) string {
	if limit <= 0 {
		return ""
	}

	preview := strings.Join(strings.Fields(content), " ")
	runes := []rune(preview)
	if len(runes) <= limit {
		return preview
	}
	return string(runes[:limit]) + "..."
}
