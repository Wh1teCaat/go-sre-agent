package main

import (
	"time"

	runstore "github.com/y2/go-sre-agent/internal/run"
)

type diagnoseOptions struct {
	Goal                   string
	ConfigPath             string
	BackendBaseURL         string
	AllowedPostURLs        []string
	LogFile                string
	AllowedLogDirs         []string
	AllowedHosts           []string
	AllowedContainers      []string
	RedisKeyPrefixes       []string
	PostgresDSN            string
	RedisAddr              string
	KafkaAddr              string
	KafkaTopic             string
	SmokeCommand           []string
	SmokeDir               string
	SmokeTimeout           time.Duration
	WebSocketURL           string
	MaxSteps               int
	LLMTimeout             time.Duration
	ToolTimeout            time.Duration
	SkillPath              string
	ToolAllowlist          []string
	RunDir                 string
	SessionID              string
	SessionDir             string
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
	Environment            string
	OverwriteSessionMemory bool
	ConfigPath             string
	MaxSteps               int
	LLMTimeout             time.Duration
	ToolTimeout            time.Duration
}

type diagnoseResult struct {
	Markdown               string
	State                  runstore.State
	RunDir                 string
	SessionDir             string
	Environment            string
	OverwriteSessionMemory bool
	ReportDir              string
}
