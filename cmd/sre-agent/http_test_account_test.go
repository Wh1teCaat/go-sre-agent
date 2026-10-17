package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfiguredLoginToolWiring(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["email"] != "test@example.invalid" || body["password"] != "private-test-password" {
			t.Error("missing configured credentials")
		}
		w.Write([]byte(`{"code":0,"data":{"token":"private-issued-token"}}`))
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "account.secret")
	if err := os.WriteFile(path, []byte(`{"email":"test@example.invalid","password":"private-test-password"}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfgPath := writeTestConfig(t, fmt.Sprintf("targets:\n  backend_base_url: %q\n  test_account:\n    url: %q\n    credentials_file: %q\n", server.URL, server.URL+"/v1/user/login", path))
	cfg, err := resolveDiagnosisConfig(diagnoseOptions{ConfigPath: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := buildToolRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	tool, ok := registry.Get("http_check")
	if !ok {
		t.Fatal("login tool missing")
	}
	obs, err := tool.Run(context.Background(), json.RawMessage(fmt.Sprintf(`{"url":%q,"method":"POST"}`, server.URL+"/v1/user/login")))
	if err != nil {
		t.Fatal(err)
	}
	if obs.Data["session_issued"] != true {
		t.Fatal("login did not return token")
	}
	prompt, _ := json.Marshal(targetContextForDiagnose(cfg))
	if !strings.Contains(string(prompt), `"test_account_configured":true`) {
		t.Fatal("login capability missing")
	}
	for _, value := range []string{path, "private-test-password", "test@example.invalid"} {
		if strings.Contains(string(prompt), value) {
			t.Fatal("credential context leaked")
		}
	}
	cfg.TestAccount.URL = ""
	registry, err = buildToolRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.Get("login_check"); ok {
		t.Fatal("separate login tool registered")
	}
}
