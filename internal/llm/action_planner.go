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
1. Plan or replan:
{"type":"plan","thought_summary":"short planning reason","plan":{"reason":"why this plan is needed or changed","items":[{"id":"backend","goal":"检查后端服务是否存活","status":"pending"}]}}
2. Tool call:
{"type":"tool_call","thought_summary":"short evidence-seeking reason","tool":"tool_name","args":{}}
3. Final diagnosis:
{"type":"final","thought_summary":"short reason diagnosis is ready","final":{"summary":"evidence-based conclusion","evidence":[{"step":1,"tool":"tool_name","summary":"specific observed evidence"}],"coverage":[{"plan_item_id":"backend","status":"done","evidence":[{"step":1,"tool":"tool_name","summary":"specific observed evidence"}],"note":"short coverage note"}],"recommendations":["actionable next step"]}}

Rules:
- 所有面向用户的文本字段必须使用中文：thought_summary、plan.reason、plan.items[].goal、plan.items[].reason、final.summary、final.evidence[].summary、final.coverage[].note、final.recommendations[]。
- final.summary 不要过度简短。用 2-5 句说明结论、已排除/未排除的方向、证据不足之处；但不要编造未执行工具的结果。
- If plan is empty and the goal is not a trivial smoke test, return a plan action first.
- You may return another plan action later when observations change the investigation direction. Keep old required items and add/update items instead of silently dropping them.
- When finalizing with a plan, final.coverage must include every plan item id. Use status "done" when backed by evidence, "blocked" when a tool failed, or "insufficient" when evidence is still not enough.
- Every "done" or "blocked" coverage item must include evidence also present in final.evidence. "insufficient" may omit evidence.
- Use only tool names listed in the diagnostic context JSON.
- Tool args must be a JSON object matching the chosen tool schema.
- The target_context field may contain configured defaults such as backend_base_url, login_url, postgres_target, postgres_dsn_configured, redis_addr, websocket_url, log_file, and docker_containers. Use it only as tool-argument context, not as diagnostic evidence.
- The memories field contains historical hints only. Never cite it as current evidence or treat it as proof; verify useful hypotheses with tools in this run.
- If correction is present, the previous output failed parsing or policy validation. Fix exactly that error and return a new valid action JSON.
- For chat_proj backend liveness goals, prefer http_check against backend_base_url or a health endpoint if the goal names one.
- For login 500 goals, prefer http_check against login_url with POST first, then log_read with an ERROR keyword if more evidence is needed. If the observed status is not 500, state that the current run did not reproduce 500 and continue with log_read before finalizing.
- For PostgreSQL production or business diagnosis goals, prefer postgres_check with tables such as ["users"] when table/schema evidence is relevant. If only protocol reachability is needed, use postgres_ping. If postgres_dsn_configured is true, the runtime injects the configured dsn; postgres_target is display context only.
- For Redis connectivity goals, use redis_ping with redis_addr.
- For WebSocket failure goals, use websocket_check with websocket_url, then log_read with a websocket keyword if needed.
- For recent log/error goals, use log_read with log_file and an ERROR keyword.
- For container startup or unexpected-exit goals, use docker_ps first. Then use docker_inspect for exit/health state and docker_logs for recent application errors. Container arguments must copy an exact name from docker_containers.
- Use thought_summary for a short operational summary only; do not reveal hidden chain-of-thought.
- The final diagnosis must be based only on observations in the context, not on target_context.
- final.evidence[].step and tool must copy the exact step and tool from a matching observation.
- If the goal asks to judge multiple named causes such as backend status, PostgreSQL, Redis, WebSocket, or logs, do not finalize until each named area has either a matching tool observation or an explicit evidence-insufficient statement based on an attempted tool call.
- When searching one log file for multiple terms, use one log_read call with keywords instead of repeating the same read for each term.
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
