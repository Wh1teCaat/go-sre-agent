package main

import (
	"time"

	"github.com/y2/go-sre-agent/internal/agent"
	runstore "github.com/y2/go-sre-agent/internal/run"
)

type diagnoseOptions struct {
	Goal                  string
	ConfigPath            string
	Service               string
	BackendBaseURL        string
	AllowedPostURLs       []string
	LogFile               string
	AllowedLogDirs        []string
	AllowedHosts          []string
	AllowedContainers     []string
	RedisKeyPrefixes      []string
	PostgresDSN           string
	RedisAddr             string
	KafkaAddr             string
	KafkaTopic            string
	SmokeCommand          []string
	SmokeDir              string
	SmokeTimeout          time.Duration
	WebSocketURL          string
	MaxSteps              int
	LLMTimeout            time.Duration
	ToolTimeout           time.Duration
	TaskTimeout           time.Duration
	MaxToolCalls          int
	MaxParallelTools      int
	ContextBudgetBytes    int
	ToolOutputBudgetBytes int
	// Progress 仅供命令层消费 runtime 进度，不能影响诊断决策。
	Progress      func(agent.ProgressEvent)
	SkillPath     string
	ToolAllowlist []string
	RunDir        string
	SessionID     string
	SessionDir    string
	// MemoryDir 为空时不加载或更新跨会话知识；CLI 固定传入 memories。
	MemoryDir              string
	Environment            string
	NewSession             bool
	OverwriteSessionMemory bool
	ReportDir              string
}

type runOptions struct {
	RunID      string
	ConfigPath string
	RunDir     string
}

type resumeOptions struct {
	RunID                  string
	RunDir                 string
	SessionDir             string
	MemoryDir              string
	Environment            string
	OverwriteSessionMemory bool
	ConfigPath             string
	MaxSteps               int
	LLMTimeout             time.Duration
	ToolTimeout            time.Duration
	TaskTimeout            time.Duration
	MaxToolCalls           int
	MaxParallelTools       int
	ContextBudgetBytes     int
	ToolOutputBudgetBytes  int
	Progress               func(agent.ProgressEvent)
	ResumeRunning          bool
}

type diagnoseResult struct {
	Markdown               string
	State                  runstore.State
	RunDir                 string
	SessionDir             string
	Service                string
	MemoryDir              string
	Environment            string
	OverwriteSessionMemory bool
	ReportDir              string
}
