package main

import (
	"time"

	runstore "github.com/y2/go-sre-agent/internal/run"
)

type diagnoseOptions struct {
	Goal              string
	ConfigPath        string
	BackendBaseURL    string
	LogFile           string
	AllowedLogDir     string
	AllowedLogDirs    []string
	AllowedHosts      []string
	AllowedContainers []string
	PostgresDSN       string
	RedisAddr         string
	WebSocketURL      string
	MaxSteps          int
	LLMTimeout        time.Duration
	ToolTimeout       time.Duration
	ToolAllowlist     []string
	RunDir            string
	ReportDir         string
}

type diagnosisConfig struct {
	Goal              string
	BackendBaseURL    string
	LogFile           string
	AllowedLogDir     string
	AllowedLogDirs    []string
	AllowedHosts      []string
	AllowedContainers []string
	PostgresDSN       string
	RedisAddr         string
	WebSocketURL      string
	MaxSteps          int
	LLMTimeout        time.Duration
	ToolTimeout       time.Duration
	ToolAllowlist     []string
	RunDir            string
	ReportDir         string
}

type llmChatOptions struct {
	Message string
}

type statusOptions struct {
	RunID      string
	ConfigPath string
	RunDir     string
}

type reportOptions struct {
	RunID      string
	ConfigPath string
	RunDir     string
}

type resumeOptions struct {
	RunID       string
	RunDir      string
	ConfigPath  string
	MaxSteps    int
	LLMTimeout  time.Duration
	ToolTimeout time.Duration
}

type diagnoseResult struct {
	Markdown  string
	State     runstore.State
	RunDir    string
	ReportDir string
}
