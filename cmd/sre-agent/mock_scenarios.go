package main

import (
	"encoding/json"
	"fmt"

	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools/httpcheck"
	"github.com/y2/go-sre-agent/internal/tools/logread"
	"github.com/y2/go-sre-agent/internal/tools/postgres"
	"github.com/y2/go-sre-agent/internal/tools/redis"
	"github.com/y2/go-sre-agent/internal/tools/websocket"
)

// scenarioActions 返回内置 mock 场景的结构化 action 序列，便于无模型环境下测试链路。
// 参数: mockScenario 为场景名，cfg 提供目标参数；返回: action 序列或未知场景错误。
func scenarioActions(mockScenario string, cfg diagnoseOptions) ([]schema.Action, error) {
	switch mockScenario {
	case "skeleton":
		return []schema.Action{
			{
				Type:           schema.ActionTypeFinal,
				ThoughtSummary: "验证 CLI、runtime 和报告生成链路",
				Final: &schema.Diagnosis{
					Summary: "MVP 诊断闭环验证完成。CLI 已成功运行 runtime 并生成诊断报告。",
				},
			},
		}, nil
	case "login-500":
		loginURL := loginURLForDiagnose(cfg)
		return []schema.Action{
			{
				Type:           schema.ActionTypeToolCall,
				ThoughtSummary: "先复现登录接口状态，确认是否返回 500",
				Tool:           httpcheck.Name,
				Args:           rawArgs(httpcheck.Args{URL: loginURL, Method: "POST"}),
			},
			{
				Type:           schema.ActionTypeToolCall,
				ThoughtSummary: "读取最近错误日志，寻找登录失败相关证据",
				Tool:           logread.Name,
				Args: rawArgs(logread.Args{
					Path:    cfg.LogFile,
					Lines:   50,
					Keyword: "ERROR",
				}),
			},
			{
				Type:           schema.ActionTypeFinal,
				ThoughtSummary: "HTTP 状态和日志证据已足够生成初步诊断",
				Final: &schema.Diagnosis{
					Summary: "登录接口返回 500，当前诊断链路已完成 HTTP 复现和错误日志读取。请结合下方证据确认具体异常。",
					Evidence: []schema.Evidence{
						{Step: 1, Tool: httpcheck.Name, Summary: "登录接口返回 500"},
						{Step: 2, Tool: logread.Name, Summary: "日志包含 ERROR"},
					},
					Recommendations: []string{
						"优先查看 Step 2 中的错误日志，确认是否为数据库、Redis、鉴权或请求参数问题。",
						"如果日志指向数据库或缓存异常，可以运行 dependency-check 场景检查 PostgreSQL 和 Redis 连通性。",
					},
				},
			},
		}, nil
	case "dependency-check":
		return []schema.Action{
			{
				Type:           schema.ActionTypeToolCall,
				ThoughtSummary: "检查 PostgreSQL 协议层是否可达",
				Tool:           postgres.PingName,
				Args:           rawArgs(postgres.PingArgs{DSN: cfg.PostgresDSN}),
			},
			{
				Type:           schema.ActionTypeToolCall,
				ThoughtSummary: "检查 Redis 是否响应 PING",
				Tool:           redis.Name,
				Args:           rawArgs(redis.Args{Addr: cfg.RedisAddr}),
			},
			{
				Type:           schema.ActionTypeFinal,
				ThoughtSummary: "依赖连通性证据已收集完成",
				Final: &schema.Diagnosis{
					Summary: "依赖连通性检查完成。当前诊断链路已检查 PostgreSQL 协议层和 Redis PING。",
					Evidence: []schema.Evidence{
						{Step: 1, Tool: postgres.PingName, Summary: "PostgreSQL 协议层可达"},
						{Step: 2, Tool: redis.Name, Summary: "Redis 返回 PONG"},
					},
					Recommendations: []string{
						"如果登录接口仍返回 500，请继续结合应用日志定位 SQL、迁移或鉴权错误。",
						"PostgreSQL 当前检查为 startup-message 协议层检查，后续可升级为带认证的 SQL ping。",
					},
				},
			},
		}, nil
	case "websocket":
		return []schema.Action{
			{
				Type:           schema.ActionTypeToolCall,
				ThoughtSummary: "尝试 WebSocket 握手，确认升级请求是否成功",
				Tool:           websocket.Name,
				Args:           rawArgs(websocket.Args{URL: cfg.WebSocketURL}),
			},
			{
				Type:           schema.ActionTypeToolCall,
				ThoughtSummary: "读取 websocket 相关错误日志",
				Tool:           logread.Name,
				Args: rawArgs(logread.Args{
					Path:    cfg.LogFile,
					Lines:   50,
					Keyword: "websocket",
				}),
			},
			{
				Type:           schema.ActionTypeFinal,
				ThoughtSummary: "WebSocket 握手和日志证据已收集完成",
				Final: &schema.Diagnosis{
					Summary: "WebSocket 诊断链路已完成。当前链路已尝试握手并读取 websocket 相关日志。",
					Evidence: []schema.Evidence{
						{Step: 1, Tool: websocket.Name, Summary: "WebSocket 握手失败或成功状态已记录"},
						{Step: 2, Tool: logread.Name, Summary: "日志包含 websocket 错误"},
					},
					Recommendations: []string{
						"如果握手返回 401/403，优先检查认证 token、cookie 或鉴权中间件。",
						"如果握手返回 404/500，优先检查路由注册、反向代理 Upgrade 头和后端日志。",
					},
				},
			},
		}, nil
	default:
		return nil, fmt.Errorf("unknown mock scenario %q", mockScenario)
	}
}

// rawArgs 把测试或 mock 场景中的强类型参数转换成工具 action 使用的 JSON 参数。
// 参数: value 为可 JSON 编码参数；返回: JSON 原始字节，编码失败时 panic。
func rawArgs(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}
