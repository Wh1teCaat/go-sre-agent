package httpcheck

import (
	"context"
	"encoding/json"
	"github.com/y2/go-sre-agent/internal/tools"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCredentialedLoginAndNoSecretOutput(t *testing.T) {
	for _, fileMode := range []bool{false, true} {
		t.Run(map[bool]string{false: "env", true: "file"}[fileMode], func(t *testing.T) {
			const password = "secret-password-for-test"
			const email = "test-account@example.invalid"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if r.Method != "POST" || body["email"] != email || body["password"] != password || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("X-Request-ID") == "" {
					t.Error("invalid login request")
				}
				w.Header().Set("Set-Cookie", "token=issued-token")
				w.Write([]byte(`{"code":0,"data":{"access_token":"issued-token"},"echo":"` + password + `"}`))
			}))
			defer server.Close()
			t.Setenv("LOGIN_TEST_EMAIL", email)
			t.Setenv("LOGIN_TEST_PASSWORD", password)
			file := ""
			if fileMode {
				file = filepath.Join(t.TempDir(), "account.secret")
				raw, _ := json.Marshal(map[string]string{"email": email, "password": password})
				if err := os.WriteFile(file, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			tool := NewWithPolicy(2048, []string{"127.0.0.1"}, []string{server.URL})
			account := TestAccount{URL: server.URL, EmailEnv: "LOGIN_TEST_EMAIL", PasswordEnv: "LOGIN_TEST_PASSWORD", CredentialsFile: file}
			if fileMode {
				account.EmailEnv = ""
				account.PasswordEnv = ""
			}
			err := tool.ConfigureTestAccount(account)
			if err != nil {
				t.Fatal(err)
			}
			obs, err := tool.Run(context.Background(), json.RawMessage(`{"url":"`+server.URL+`","method":"POST"}`))
			if err != nil {
				t.Fatal(err)
			}
			if obs.Data["status"] != 200 || obs.Data["session_issued"] != true || obs.Data["business_code"] != 0 || !tool.Spec().SideEffect {
				t.Fatalf("unexpected result: %#v", obs)
			}
			raw, _ := json.Marshal(obs)
			for _, secret := range []string{password, email, "issued-token"} {
				if strings.Contains(string(raw), secret) {
					t.Fatal("credential leaked")
				}
			}
		})
	}
}

func TestNoRedirectAndNoModelCredentialOverride(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Redirect(w, r, "/other", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	t.Setenv("LOGIN_TEST_EMAIL", "test@example.invalid")
	t.Setenv("LOGIN_TEST_PASSWORD", "password")
	tool := NewWithPolicy(2048, []string{"127.0.0.1"}, []string{server.URL})
	err := tool.ConfigureTestAccount(TestAccount{URL: server.URL, EmailEnv: "LOGIN_TEST_EMAIL", PasswordEnv: "LOGIN_TEST_PASSWORD"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tool.Run(context.Background(), json.RawMessage(`{"url":"http://evil.invalid"}`)); err == nil {
		t.Fatal("accepted override")
	}
	obs, err := tool.Run(context.Background(), json.RawMessage(`{"url":"`+server.URL+`","method":"POST"}`))
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || obs.Data["status"] != 307 {
		t.Fatal("followed redirect")
	}
	t.Setenv("LOGIN_TEST_PASSWORD", "")
	if _, err = tool.Run(context.Background(), json.RawMessage(`{"url":"`+server.URL+`","method":"POST"}`)); err == nil || calls != 1 {
		t.Fatal("sent request without credentials")
	}
	blocked := NewWithPolicy(2048, []string{"other.invalid"}, []string{server.URL})
	if err := blocked.ConfigureTestAccount(TestAccount{URL: server.URL, EmailEnv: "E", PasswordEnv: "P"}); err == nil {
		t.Fatal("accepted disallowed host")
	}
	blocked = NewWithPolicy(2048, []string{"127.0.0.1"}, nil)
	if err := blocked.ConfigureTestAccount(TestAccount{URL: server.URL, EmailEnv: "E", PasswordEnv: "P"}); err == nil {
		t.Fatal("accepted disallowed POST")
	}
}

func TestAccountInjectionIsScopedAndPreservesInputs(t *testing.T) {
	t.Setenv("LOGIN_TEST_EMAIL", "test@example.invalid")
	t.Setenv("LOGIN_TEST_PASSWORD", "private-password")
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path != "/login" || r.Method != "POST" {
			if body["email"] != "" || body["password"] != "" {
				t.Error("credentials injected outside configured login")
			}
		}
		w.Write([]byte(`{"code":0,"data":{"token":"issued-token"}}`))
	}))
	defer server.Close()
	tool := NewWithPolicy(2048, []string{"127.0.0.1"}, []string{server.URL + "/login", server.URL + "/other"})
	if err := tool.ConfigureTestAccount(TestAccount{URL: server.URL + "/login", EmailEnv: "LOGIN_TEST_EMAIL", PasswordEnv: "LOGIN_TEST_PASSWORD"}); err != nil {
		t.Fatal(err)
	}
	for _, args := range []Args{{URL: server.URL + "/other", Method: "POST"}, {URL: server.URL + "/login", Method: "GET"}} {
		raw, _ := json.Marshal(args)
		if _, err := tool.Run(context.Background(), raw); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range []Args{{URL: server.URL + "/login", Method: "POST", Body: `{"password":"override"}`}, {URL: server.URL + "/login", Method: "POST", Repeat: 2}} {
		raw, _ := json.Marshal(args)
		if _, err := tool.Run(context.Background(), raw); err == nil {
			t.Fatal("accepted login override or repeat")
		}
	}
	if calls != 2 {
		t.Fatal("invalid login request reached server")
	}
	args := Args{URL: server.URL + "/login", Method: "POST", Headers: map[string]string{"x-request-id": "operator-request-id"}}
	raw, _ := json.Marshal(args)
	obs, err := tool.Run(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Data["request_id"] != "operator-request-id" {
		t.Fatal("request ID not preserved")
	}
	if args.Body != "" || strings.Contains(string(raw), "private-password") {
		t.Fatal("mutated model arguments")
	}
	clean := tools.RedactSensitiveValue(obs.Data).(map[string]any)
	if clean["session_issued"] != true {
		t.Fatal("login outcome lost during runtime redaction")
	}
}
