package agent

import (
	"github.com/y2/go-sre-agent/internal/schema"
	"testing"
)

func TestConfiguredLoginHealthRequiresBusinessEvidence(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status, code int
		issued       bool
		want         string
	}{
		{"login succeeded", 200, 0, true, schema.TargetHealthHealthy},
		{"HTTP alone", 200, 0, false, schema.TargetHealthUnknown},
		{"business failure", 200, 401, false, schema.TargetHealthUnhealthy},
		{"invalid credentials", 401, 401, false, schema.TargetHealthUnhealthy},
		{"redirect", 302, 0, false, schema.TargetHealthUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obs := schema.Observation{CheckStatus: schema.CheckExecutionCompleted, Data: map[string]any{"test_account_used": true, "status": tc.status, "business_code": tc.code, "session_issued": tc.issued}}
			if got := inferTargetHealth("http_check", obs); got != tc.want {
				t.Fatalf("health=%s want %s", got, tc.want)
			}
		})
	}
}
