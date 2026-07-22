package tools

import (
	"fmt"
	"sort"
)

type Registry struct {
	tools map[string]Tool
}

func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

func (r *Registry) Register(tool Tool) error {
	if tool == nil {
		return fmt.Errorf("tool is nil")
	}
	name := tool.Spec().Name
	if name == "" {
		return fmt.Errorf("tool name is empty")
	}
	if _, exists := r.tools[name]; exists {
		// 工具名是 LLM action 里的唯一标识，重复注册会让审计和执行结果不可预测。
		return fmt.Errorf("tool %q already registered", name)
	}
	r.tools[name] = tool
	return nil
}

func (r *Registry) Get(name string) (Tool, bool) {
	tool, ok := r.tools[name]
	return tool, ok
}

func (r *Registry) List() []ToolSpec {
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	// 稳定排序让 prompt 中的工具列表可复现，也减少测试和日志 diff 噪声。
	sort.Strings(names)

	specs := make([]ToolSpec, 0, len(names))
	for _, name := range names {
		specs = append(specs, r.tools[name].Spec())
	}
	return specs
}
