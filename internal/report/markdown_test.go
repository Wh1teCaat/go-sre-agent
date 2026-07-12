package report

import (
	"strings"
	"testing"
	"time"

	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/trace"
)

func TestMarkdownUsesOnlyTraceBackedEvidence(t *testing.T) {
	markdown := Markdown(Input{
		Goal: "诊断登录 500",
		Diagnosis: schema.Diagnosis{
			Summary: "登录失败",
			Evidence: []schema.Evidence{
				{Step: 1, Tool: "http_check", Summary: "模型声称数据库损坏"},
				{Step: 99, Tool: "redis_ping", Summary: "模型声称 Redis 不可达"},
			},
		},
		Trace: []trace.Entry{
			{
				Step:     1,
				ToolName: "http_check",
				Result: schema.Observation{
					Tool:    "http_check",
					Summary: "POST /v1/user/login returned 500",
				},
				Duration: time.Millisecond,
			},
		},
	})

	if !strings.Contains(markdown, "- Step 1 `http_check`: POST /v1/user/login returned 500") {
		t.Fatalf("markdown missing trace-backed evidence:\n%s", markdown)
	}
	for _, notWant := range []string{"模型声称数据库损坏", "模型声称 Redis 不可达"} {
		if strings.Contains(markdown, notWant) {
			t.Fatalf("markdown contains unverified model evidence %q:\n%s", notWant, markdown)
		}
	}
	if !strings.Contains(markdown, "## Filtered Evidence Claims") {
		t.Fatalf("markdown missing filtered evidence section:\n%s", markdown)
	}
	if !strings.Contains(markdown, "- Step 99 `redis_ping`: no matching trace entry") {
		t.Fatalf("markdown missing filtered evidence reason:\n%s", markdown)
	}
}

func TestMarkdownCanUseTraceErrorAsEvidence(t *testing.T) {
	markdown := Markdown(Input{
		Goal: "检查后端",
		Diagnosis: schema.Diagnosis{
			Summary: "后端检查失败",
			Evidence: []schema.Evidence{
				{Step: 1, Tool: "http_check", Summary: "模型原始错误描述"},
			},
		},
		Trace: []trace.Entry{
			{
				Step:     1,
				ToolName: "http_check",
				Result: schema.Observation{
					Tool:  "http_check",
					Error: "connection refused",
				},
				Error:    "connection refused",
				Duration: time.Millisecond,
			},
		},
	})

	if !strings.Contains(markdown, "- Step 1 `http_check`: connection refused") {
		t.Fatalf("markdown missing trace error evidence:\n%s", markdown)
	}
	if strings.Contains(markdown, "模型原始错误描述") {
		t.Fatalf("markdown contains model evidence instead of trace error:\n%s", markdown)
	}
}

func TestMarkdownShowsPlanCoverage(t *testing.T) {
	markdown := Markdown(Input{
		Goal: "检查后端",
		Plan: schema.Plan{
			Items: []schema.PlanItem{
				{ID: "backend", Goal: "检查后端服务是否存活"},
			},
		},
		Diagnosis: schema.Diagnosis{
			Summary: "后端有响应",
			Coverage: []schema.CoverageItem{
				{PlanItemID: "backend", Status: "done", Note: "HTTP 检查已完成"},
			},
		},
	})

	if !strings.Contains(markdown, "## Plan Coverage") {
		t.Fatalf("markdown missing plan coverage:\n%s", markdown)
	}
	if !strings.Contains(markdown, "- `done` backend: 检查后端服务是否存活 - HTTP 检查已完成") {
		t.Fatalf("markdown missing coverage item:\n%s", markdown)
	}
}
