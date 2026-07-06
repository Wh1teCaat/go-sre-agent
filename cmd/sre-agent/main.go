package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/y2/go-sre-agent/internal/agent"
	appconfig "github.com/y2/go-sre-agent/internal/config"
	"github.com/y2/go-sre-agent/internal/llm"
	"github.com/y2/go-sre-agent/internal/policy"
	"github.com/y2/go-sre-agent/internal/report"
	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
	"github.com/y2/go-sre-agent/internal/tools/httpcheck"
	"github.com/y2/go-sre-agent/internal/tools/logread"
	"github.com/y2/go-sre-agent/internal/tools/postgres"
	"github.com/y2/go-sre-agent/internal/tools/redis"
	"github.com/y2/go-sre-agent/internal/tools/websocket"
	"github.com/y2/go-sre-agent/internal/trace"
)

type diagnoseOptions struct {
	Goal           string
	ConfigPath     string
	MockScenario   string
	BackendBaseURL string
	LogFile        string
	AllowedLogDir  string
	AllowedLogDirs []string
	AllowedHosts   []string
	PostgresDSN    string
	RedisAddr      string
	WebSocketURL   string
	MaxSteps       int
	ToolTimeout    time.Duration
	ToolAllowlist  []string
}

type llmChatOptions struct {
	Message string
}

func main() {
	if len(os.Args) < 2 {
		printUsageAndExit()
	}

	switch os.Args[1] {
	case "diagnose":
		runDiagnoseCommand(os.Args[2:])
	case "llm":
		runLLMCommand(os.Args[2:])
	default:
		printUsageAndExit()
	}
}

func printUsageAndExit() {
	fmt.Fprintln(os.Stderr, "usage: sre-agent diagnose --goal <goal> [--mock-scenario login-500] [--out report.md]")
	fmt.Fprintln(os.Stderr, "       sre-agent llm ping")
	fmt.Fprintln(os.Stderr, "       sre-agent llm chat --message <message>")
	os.Exit(2)
}

func runDiagnoseCommand(args []string) {
	fs := flag.NewFlagSet("diagnose", flag.ExitOnError)
	goal := fs.String("goal", "", "diagnostic goal")
	configPath := fs.String("config", "", "config file path")
	mockScenario := fs.String("mock-scenario", "", "mock scenario name")
	backendBaseURL := fs.String("backend-url", "", "backend base URL")
	logFile := fs.String("log-file", "", "log file path")
	allowedLogDir := fs.String("allowed-log-dir", "", "allowed log directory")
	postgresDSN := fs.String("postgres-dsn", "", "PostgreSQL DSN")
	redisAddr := fs.String("redis-addr", "", "Redis address")
	webSocketURL := fs.String("websocket-url", "", "WebSocket URL")
	maxSteps := fs.Int("max-steps", 0, "maximum agent steps")
	toolTimeout := fs.Duration("tool-timeout", 0, "tool execution timeout")
	out := fs.String("out", "", "write markdown report to file")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	if *goal == "" {
		fmt.Fprintln(os.Stderr, "--goal is required")
		os.Exit(2)
	}

	// CLI 层只做参数收集和退出码处理；诊断执行细节都放到 runDiagnose，
	// 这样测试可以直接调用 runDiagnose，避免依赖 os.Args 和 os.Exit。
	markdown, err := runDiagnose(context.Background(), diagnoseOptions{
		Goal:           *goal,
		ConfigPath:     *configPath,
		MockScenario:   *mockScenario,
		BackendBaseURL: *backendBaseURL,
		LogFile:        *logFile,
		AllowedLogDir:  *allowedLogDir,
		PostgresDSN:    *postgresDSN,
		RedisAddr:      *redisAddr,
		WebSocketURL:   *webSocketURL,
		MaxSteps:       *maxSteps,
		ToolTimeout:    *toolTimeout,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if *out != "" {
		// --out 用于脚本或示例生成报告；不指定时保持 Unix CLI 风格直接输出到 stdout。
		if err := os.WriteFile(*out, []byte(markdown), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	fmt.Print(markdown)
}

func runLLMCommand(args []string) {
	if len(args) < 1 {
		printUsageAndExit()
	}

	switch args[0] {
	case "ping":
		content, err := runLLMPing(context.Background())
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(content)
	case "chat":
		fs := flag.NewFlagSet("llm chat", flag.ExitOnError)
		message := fs.String("message", "", "message to send to the configured LLM")
		if err := fs.Parse(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if strings.TrimSpace(*message) == "" {
			fmt.Fprintln(os.Stderr, "--message is required")
			os.Exit(2)
		}
		content, err := runLLMChat(context.Background(), llmChatOptions{Message: *message})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(content)
	default:
		printUsageAndExit()
	}
}

func runLLMPing(ctx context.Context) (string, error) {
	return runLLMChat(ctx, llmChatOptions{
		Message: "这是 LLM 连通性测试。请只回复 pong。",
	})
}

// runLLMChat 只验证底层 ChatClient 连通性，直接返回模型原始文本。
// 诊断链路仍然通过 ActionPlanner 把模型输出解析成结构化 action。
func runLLMChat(ctx context.Context, opts llmChatOptions) (string, error) {
	message := strings.TrimSpace(opts.Message)
	if message == "" {
		return "", fmt.Errorf("message is required")
	}

	llmConfig, err := loadLLMConfig()
	if err != nil {
		return "", err
	}
	if llmConfig.Provider != "openai_compatible" {
		return "", fmt.Errorf("llm chat requires SRE_AGENT_LLM_PROVIDER=openai_compatible, got %q", llmConfig.Provider)
	}

	client := llm.NewOpenAICompatibleChatClient(llmConfig)
	response, err := client.Chat(ctx, llm.ChatRequest{
		Model: llmConfig.Model,
		Messages: []llm.Message{
			{
				Role:    llm.RoleSystem,
				Content: "你是 go-sre-agent 的 LLM 连通性测试助手。直接、简洁地回答用户消息。",
			},
			{
				Role:    llm.RoleUser,
				Content: message,
			},
		},
		OutputMode:  llm.OutputText,
		Temperature: 0.2,
	})
	if err != nil {
		return "", err
	}
	return response.Content, nil
}

// runDiagnose 组装一次诊断所需的 registry、provider、policy、runtime，
// 执行完成后把 diagnosis 和内部 trace 交给报告层生成 Markdown。
func runDiagnose(ctx context.Context, opts diagnoseOptions) (string, error) {
	resolved, err := resolveDiagnoseOptions(opts)
	if err != nil {
		return "", err
	}
	opts = resolved

	// Registry 是模型可选工具的唯一来源。注册时就注入 host/path 等边界，
	// 后续 runtime 只按工具名查找并执行，不再临时拼装工具实例。
	registry := tools.NewRegistry()
	if err := registry.Register(httpcheck.NewWithAllowedHosts(nil, 2048, opts.AllowedHosts)); err != nil {
		return "", err
	}
	if err := registry.Register(logread.New(opts.AllowedLogDirs, 1000)); err != nil {
		return "", err
	}
	if err := registry.Register(postgres.New(nil)); err != nil {
		return "", err
	}
	if err := registry.Register(redis.New(nil)); err != nil {
		return "", err
	}
	if err := registry.Register(websocket.NewWithAllowedHosts(nil, opts.AllowedHosts)); err != nil {
		return "", err
	}

	provider, err := buildLLMProvider(opts)
	if err != nil {
		return "", err
	}

	traceStore := trace.NewMemoryStore()
	// Runtime 把 provider、registry、policy 和 trace 串起来：
	// provider 决定下一步，policy 决定能不能执行，registry 执行工具，trace 留证。
	runtime := agent.NewRuntime(agent.RuntimeConfig{
		MaxSteps:            opts.MaxSteps,
		ToolTimeout:         opts.ToolTimeout,
		InitialObservations: initialObservationsForDiagnose(opts),
	}, provider, registry, policy.NewValidator(policy.Config{
		MaxSteps:      opts.MaxSteps,
		ToolAllowlist: opts.ToolAllowlist,
		ToolTimeout:   opts.ToolTimeout,
		AllowedHosts:  opts.AllowedHosts,
		ToolSchemas:   toolSchemasFromRegistry(registry),
	}), traceStore)

	diagnosis, err := runtime.Run(ctx, opts.Goal)
	if err != nil {
		return "", err
	}

	return report.Markdown(report.Input{
		Goal:      opts.Goal,
		Diagnosis: *diagnosis,
		Trace:     traceStore.List(),
	}), nil
}

// initialObservationsForDiagnose 构造发给 LLM 的目标上下文。
// 这些信息只用于选择工具参数，不写入 trace，也不能作为最终报告证据。
func initialObservationsForDiagnose(opts diagnoseOptions) []schema.Observation {
	loginURL := ""
	if strings.TrimSpace(opts.BackendBaseURL) != "" {
		// login_url 是面向 chat_proj 的便利默认值；目标里给了更具体 URL 时，
		// prompt 会要求模型优先使用目标中的具体地址。
		loginURL = strings.TrimRight(opts.BackendBaseURL, "/") + "/v1/user/login"
	}

	return []schema.Observation{
		{
			Tool:    "target_context",
			Summary: "Configured chat_proj diagnostic targets. Use these values as default tool arguments when the goal does not provide a more specific target. This context is not diagnostic evidence.",
			Data: map[string]any{
				"backend_base_url": opts.BackendBaseURL,
				"login_url":        loginURL,
				"postgres_dsn":     safePostgresDSNForPrompt(opts.PostgresDSN),
				"redis_addr":       opts.RedisAddr,
				"websocket_url":    opts.WebSocketURL,
				"log_file":         opts.LogFile,
				"allowed_hosts":    opts.AllowedHosts,
			},
		},
	}
}

// safePostgresDSNForPrompt 在 DSN 进入 prompt 前移除密码等敏感信息。
// 工具真正执行时仍使用原始 DSN，模型侧只看到可识别目标但不可泄密的版本。
func safePostgresDSNForPrompt(dsn string) string {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return ""
	}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		parsed, err := url.Parse(dsn)
		if err != nil {
			return tools.RedactSensitive(dsn)
		}
		if parsed.User != nil {
			username := parsed.User.Username()
			if username != "" {
				// 保留用户名便于识别连接目标，刻意丢弃密码，避免 prompt 泄密。
				parsed.User = url.User(username)
			} else {
				parsed.User = nil
			}
		}
		return parsed.String()
	}
	return tools.RedactSensitive(dsn)
}

// toolSchemasFromRegistry 把工具注册表中的参数 schema 复制给 policy。
// runtime 先用这些 schema 拦截未知参数和基础类型错误，再真正执行工具。
func toolSchemasFromRegistry(registry *tools.Registry) map[string]tools.ToolSchema {
	schemas := map[string]tools.ToolSchema{}
	for _, spec := range registry.List() {
		schemas[spec.Name] = spec.Schema
	}
	return schemas
}

// resolveDiagnoseOptions 按 CLI 参数优先、配置文件其次、默认值最后的顺序补齐诊断配置。
func resolveDiagnoseOptions(opts diagnoseOptions) (diagnoseOptions, error) {
	if strings.TrimSpace(opts.Goal) == "" {
		return diagnoseOptions{}, fmt.Errorf("goal is required")
	}

	cfg := appconfig.Default()
	if opts.ConfigPath != "" {
		// 配置文件只覆盖写明的字段，未配置字段继续使用 Default。
		loaded, err := appconfig.Load(opts.ConfigPath)
		if err != nil {
			return diagnoseOptions{}, err
		}
		cfg = loaded
	}

	if opts.BackendBaseURL == "" {
		opts.BackendBaseURL = cfg.Targets.BackendBaseURL
	}
	if opts.LogFile == "" {
		opts.LogFile = cfg.Targets.LogFile
	}
	if len(opts.AllowedLogDirs) == 0 {
		if opts.AllowedLogDir != "" {
			opts.AllowedLogDirs = []string{opts.AllowedLogDir}
		} else if len(cfg.Policy.AllowedLogDirs) > 0 {
			opts.AllowedLogDirs = cfg.Policy.AllowedLogDirs
			opts.AllowedLogDir = cfg.Policy.AllowedLogDirs[0]
		} else {
			// 没有显式 allowlist 时，只允许读取配置日志文件所在目录，
			// 避免 log_read 因默认空目录而失去路径边界。
			opts.AllowedLogDir = filepath.Dir(opts.LogFile)
			opts.AllowedLogDirs = []string{opts.AllowedLogDir}
		}
	}
	if opts.AllowedLogDir == "" && len(opts.AllowedLogDirs) > 0 {
		opts.AllowedLogDir = opts.AllowedLogDirs[0]
	}
	if len(opts.AllowedHosts) == 0 {
		opts.AllowedHosts = cfg.Policy.AllowedHosts
	}
	if opts.PostgresDSN == "" {
		opts.PostgresDSN = cfg.Targets.PostgresDSN
	}
	if opts.RedisAddr == "" {
		opts.RedisAddr = cfg.Targets.RedisAddr
	}
	if opts.WebSocketURL == "" {
		opts.WebSocketURL = cfg.Targets.WebSocketURL
	}
	if opts.MaxSteps <= 0 {
		opts.MaxSteps = cfg.Agent.MaxSteps
	}
	if opts.ToolTimeout <= 0 {
		opts.ToolTimeout = cfg.Agent.ToolTimeout
	}
	if len(opts.ToolAllowlist) == 0 {
		opts.ToolAllowlist = cfg.Policy.ToolAllowlist
	}

	return opts, nil
}

// buildLLMProvider 根据环境配置选择 mock provider 或真实 ActionPlanner。
// runtime 只依赖 llm.Provider，因此后续换模型不需要改 agent 主循环。
func buildLLMProvider(opts diagnoseOptions) (llm.Provider, error) {
	llmConfig, err := loadLLMConfig()
	if err != nil {
		return nil, err
	}

	if opts.MockScenario != "" || llmConfig.Provider == "" || llmConfig.Provider == llm.DefaultLLMProvider {
		if opts.MockScenario == "" {
			opts.MockScenario = "skeleton"
		}
		// mock provider 返回固定 action 序列，用于本地演示和确定性测试；
		// 它仍然会经过 runtime、policy 和真实工具执行链路。
		actions, err := scenarioActions(opts)
		if err != nil {
			return nil, err
		}
		return llm.NewMockProvider(actions), nil
	}

	switch llmConfig.Provider {
	case "openai_compatible":
		client := llm.NewOpenAICompatibleChatClient(llmConfig)
		// 真实 provider 只负责产出结构化 action，工具执行和证据校验仍在本进程内完成。
		return llm.NewActionPlanner(client, llm.ActionPlannerConfig{
			Model:       llmConfig.Model,
			Temperature: 0.2,
		}), nil
	default:
		return nil, fmt.Errorf("unknown llm provider %q", llmConfig.Provider)
	}
}

// loadLLMConfig 优先读取当前目录 .env，找不到时回退到进程环境变量。
func loadLLMConfig() (llm.OpenAICompatibleConfig, error) {
	const envPath = ".env"
	if _, err := os.Stat(envPath); err == nil {
		return llm.LoadOpenAICompatibleConfig(envPath)
	} else if err != nil && !os.IsNotExist(err) {
		return llm.OpenAICompatibleConfig{}, fmt.Errorf("stat env file: %w", err)
	}
	return llm.LoadOpenAICompatibleConfig("")
}

// scenarioActions 返回内置 mock 场景的结构化 action 序列，便于无模型环境下测试链路。
func scenarioActions(opts diagnoseOptions) ([]schema.Action, error) {
	switch opts.MockScenario {
	case "skeleton":
		return []schema.Action{
			{
				Type:           schema.ActionTypeFinal,
				ThoughtSummary: "skeleton CLI returns a final placeholder diagnosis",
				Final: &schema.Diagnosis{
					Summary: "项目骨架已初始化。真实工具可通过 mock 场景逐步接入。",
				},
			},
		}, nil
	case "login-500":
		loginURL := strings.TrimRight(opts.BackendBaseURL, "/") + "/v1/user/login"
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
					Path:    opts.LogFile,
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
				Tool:           postgres.Name,
				Args:           rawArgs(postgres.Args{DSN: opts.PostgresDSN}),
			},
			{
				Type:           schema.ActionTypeToolCall,
				ThoughtSummary: "检查 Redis 是否响应 PING",
				Tool:           redis.Name,
				Args:           rawArgs(redis.Args{Addr: opts.RedisAddr}),
			},
			{
				Type:           schema.ActionTypeFinal,
				ThoughtSummary: "依赖连通性证据已收集完成",
				Final: &schema.Diagnosis{
					Summary: "依赖连通性检查完成。当前诊断链路已检查 PostgreSQL 协议层和 Redis PING。",
					Evidence: []schema.Evidence{
						{Step: 1, Tool: postgres.Name, Summary: "PostgreSQL 协议层可达"},
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
				Args:           rawArgs(websocket.Args{URL: opts.WebSocketURL}),
			},
			{
				Type:           schema.ActionTypeToolCall,
				ThoughtSummary: "读取 websocket 相关错误日志",
				Tool:           logread.Name,
				Args: rawArgs(logread.Args{
					Path:    opts.LogFile,
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
		return nil, fmt.Errorf("unknown mock scenario %q", opts.MockScenario)
	}
}

// rawArgs 把测试或 mock 场景中的强类型参数转换成工具 action 使用的 JSON 参数。
func rawArgs(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}
