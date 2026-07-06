package policy

import (
	"time"

	"github.com/y2/go-sre-agent/internal/tools"
)

type Config struct {
	MaxSteps       int
	ToolAllowlist  []string
	ToolTimeout    time.Duration
	AllowedLogDirs []string
	AllowedHosts   []string
	ToolSchemas    map[string]tools.ToolSchema
}

type Validator struct {
	config  Config
	allowed map[string]struct{}
	schemas map[string]tools.ToolSchema
}

func NewValidator(config Config) *Validator {
	allowed := make(map[string]struct{}, len(config.ToolAllowlist))
	for _, tool := range config.ToolAllowlist {
		allowed[tool] = struct{}{}
	}
	schemas := make(map[string]tools.ToolSchema, len(config.ToolSchemas))
	for name, schema := range config.ToolSchemas {
		schemas[name] = schema
	}
	return &Validator{
		config:  config,
		allowed: allowed,
		schemas: schemas,
	}
}

func (v *Validator) MaxSteps() int {
	return v.config.MaxSteps
}

func (v *Validator) ToolTimeout() time.Duration {
	return v.config.ToolTimeout
}
