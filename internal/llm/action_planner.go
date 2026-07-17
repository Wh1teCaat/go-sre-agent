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
	Skill       string
}

// ActionPlanner 是 Provider 和通用 ChatClient 之间的桥梁，负责规划和动作解析。
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

// Plan 创建或更新检查计划；已有计划无需更新时返回 nil。
func (p *ActionPlanner) Plan(ctx context.Context, request Request) (*schema.Plan, error) {
	request.Mode = "plan"
	contextJSON, err := json.MarshalIndent(request, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode plan context: %w", err)
	}
	response, err := p.client.Chat(ctx, ChatRequest{
		Model: p.config.Model,
		Messages: []Message{
			{Role: RoleSystem, Content: p.config.Skill},
			{Role: RoleUser, Content: string(contextJSON)},
		},
		OutputMode:  OutputJSON,
		Temperature: p.config.Temperature,
	})
	if err != nil {
		return nil, err
	}
	content := strings.TrimSpace(response.Content)
	if content == "" {
		return nil, fmt.Errorf("llm plan response content is empty")
	}
	var result struct {
		Plan json.RawMessage `json:"plan"`
	}
	if err := json.NewDecoder(strings.NewReader(extractActionJSON(content))).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode llm plan json: %w; content_preview=%q", err, contentPreview(content, 240))
	}
	if len(result.Plan) == 0 {
		return nil, fmt.Errorf("decode llm plan json: missing plan field")
	}
	if strings.TrimSpace(string(result.Plan)) == "null" {
		return nil, nil
	}
	var plan schema.Plan
	if err := json.Unmarshal(result.Plan, &plan); err != nil {
		return nil, fmt.Errorf("decode llm plan: %w", err)
	}
	return &plan, nil
}

// Next 让模型选择直接执行 action，或者先请求 Planner。
func (p *ActionPlanner) Next(ctx context.Context, request Request) (Decision, error) {
	request.Mode = "decision"
	chatRequest, err := buildActionChatRequest(p.config, request)
	if err != nil {
		return Decision{}, err
	}

	response, err := p.client.Chat(ctx, chatRequest)
	if err != nil {
		return Decision{}, err
	}

	content := strings.TrimSpace(response.Content)
	if content == "" {
		return Decision{}, fmt.Errorf("llm response content is empty")
	}

	var result struct {
		schema.Action
		NeedsPlan bool `json:"needs_plan"`
	}
	// 有些兼容模型即使请求 JSON mode，也可能额外包一层说明或 Markdown fence。
	// 这里先提取出 JSON 起点，后续字段合法性仍交给 policy 层做强校验。
	actionJSON := extractActionJSON(content)
	if err := json.NewDecoder(strings.NewReader(actionJSON)).Decode(&result); err != nil {
		return Decision{}, fmt.Errorf("decode llm decision json: %w; content_preview=%q", err, contentPreview(content, 240))
	}
	if result.NeedsPlan {
		if result.Type != "" {
			return Decision{}, fmt.Errorf("llm decision cannot request planning and include an action")
		}
		return Decision{NeedsPlan: true}, nil
	}
	return Decision{Action: &result.Action}, nil
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
				Content: config.Skill,
			},
			{
				Role:    RoleUser,
				Content: string(contextJSON),
			},
		},
		OutputMode:  OutputJSON,
		Temperature: config.Temperature,
	}, nil
}

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
