package main

import (
	"context"
	"encoding/json"
	"time"

	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
)

// demoTool is registered only for the explicit tui-demo mock scenario. It never
// opens a socket, reads a service log, or invokes an external process.
type demoTool struct {
	name, summary string
	delay         time.Duration
}

func (t demoTool) Spec() tools.ToolSpec {
	return tools.ToolSpec{Name: t.name, Description: "Offline TUI demonstration", Schema: tools.ToolSchema{Properties: map[string]tools.ArgSpec{}}}
}
func (t demoTool) Run(ctx context.Context, _ json.RawMessage) (schema.Observation, error) {
	timer := time.NewTimer(t.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return schema.Observation{}, ctx.Err()
	case <-timer.C:
	}
	return schema.Observation{Tool: t.name, Summary: t.summary, CheckStatus: schema.CheckExecutionCompleted, TargetHealth: schema.TargetHealthUnhealthy, ObservedAt: time.Now().UTC()}, nil
}
func registerTUIDemoTools(registry *tools.Registry) error {
	for _, tool := range []demoTool{{"demo_http", "登录接口返回 500", 180 * time.Millisecond}, {"demo_logs", "应用日志出现 connection refused", 250 * time.Millisecond}, {"demo_postgres", "PostgreSQL 连接检查失败", 550 * time.Millisecond}} {
		if err := registry.Register(tool); err != nil {
			return err
		}
	}
	return nil
}
