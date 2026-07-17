package policy

import "github.com/y2/go-sre-agent/internal/tools"

type Config struct {
	ToolAllowlist []string
	ToolSchemas   map[string]tools.ToolSchema
}

type Validator struct {
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
		allowed: allowed,
		schemas: schemas,
	}
}
