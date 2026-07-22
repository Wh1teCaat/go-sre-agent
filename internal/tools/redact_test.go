package tools

import "testing"

func TestRedactSensitiveValueRedactsNestedCredentials(t *testing.T) {
	value := map[string]any{
		"headers": map[string]any{
			"Authorization": "Bearer secret",
		},
		"items": []any{map[string]any{"password": "secret"}},
	}

	redacted := RedactSensitiveValue(value).(map[string]any)
	if got := redacted["headers"].(map[string]any)["Authorization"]; got != "[REDACTED]" {
		t.Fatalf("authorization = %#v, want redacted", got)
	}
	if got := redacted["items"].([]any)[0].(map[string]any)["password"]; got != "[REDACTED]" {
		t.Fatalf("password = %#v, want redacted", got)
	}
}
