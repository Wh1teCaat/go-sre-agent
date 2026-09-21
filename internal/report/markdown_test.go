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

func TestMarkdownRedactsSensitiveContent(t *testing.T) {
	markdown := Markdown(Input{
		Goal:      "诊断 password=secret",
		Diagnosis: schema.Diagnosis{Summary: "token=secret"},
	})

	if strings.Contains(markdown, "secret") || !strings.Contains(markdown, "[REDACTED]") {
		t.Fatalf("markdown was not redacted:\n%s", markdown)
	}
}

func TestMarkdownShowsStructuredEvidenceAndPendingVerification(t *testing.T) {
	observedAt := time.Date(2026, 9, 21, 8, 30, 0, 0, time.UTC)
	markdown := Markdown(Input{
		Diagnosis: schema.Diagnosis{
			Summary: "接口异常可能与认证分支有关。",
			RootCause: &schema.RootCause{
				Status:    "suspected",
				Statement: "当前日志不足以确定认证分支。",
			},
			Evidence: []schema.Evidence{
				{Step: 1, Tool: "http_check"},
				{Step: 2, Tool: "log_read"},
			},
			SupportingEvidence: []schema.Evidence{{Step: 1, Tool: "http_check"}},
			CounterEvidence:    []schema.Evidence{{Step: 2, Tool: "log_read"}},
			PendingVerifications: []schema.PendingVerification{{
				Question:      "关联同一请求的认证日志",
				Reason:        "确认是否命中相同分支",
				Target:        schema.TargetIdentity{Kind: "endpoint", ID: "http://api.local/login"},
				SuggestedTool: "log_read",
			}},
		},
		Trace: []trace.Entry{
			{
				Step:     1,
				ToolName: "http_check",
				Result: schema.Observation{
					Summary: "POST /login returned 500",
					Facts: []schema.Fact{{
						Key:        "status",
						Value:      500,
						ObservedAt: observedAt,
						Target:     schema.TargetIdentity{Kind: "endpoint", ID: "http://api.local/login"},
					}},
				},
			},
			{
				Step:     2,
				ToolName: "log_read",
				Result:   schema.Observation{Summary: "read 0 log lines"},
			},
		},
	})

	for _, want := range []string{
		"## Supporting Evidence",
		"## Counter Evidence",
		"## Pending Verifications",
		"## Structured Facts",
		"`status` = `500` [target=endpoint:http://api.local/login, observed_at=2026-09-21T08:30:00Z]",
		"关联同一请求的认证日志: 确认是否命中相同分支 [target=endpoint:http://api.local/login] [tool=log_read]",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("markdown missing %q:\n%s", want, markdown)
		}
	}
}
