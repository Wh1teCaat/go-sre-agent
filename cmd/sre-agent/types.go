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
	AllowedLogDirs    []string
	AllowedHosts      []string
	AllowedContainers []string
	PostgresDSN       string
	RedisAddr         string
	WebSocketURL      string
	MaxSteps          int
	LLMTimeout        time.Duration
	ToolTimeout       time.Duration
	SkillPath         string
	ToolAllowlist     []string
	RunDir            string
	ReportDir         string
}

type runOptions struct {
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
