package docker

import "github.com/y2/go-sre-agent/internal/tools"

const Name = "docker_inspect"

func Spec() tools.ToolSpec {
	return tools.ToolSpec{
		Name:        Name,
		Description: "Reserved read-only Docker inspection tool for a later milestone.",
		Schema: tools.ToolSchema{
			Properties: map[string]tools.ArgSpec{
				"container": {Type: "string", Required: true, Description: "Container name or ID."},
			},
		},
	}
}
