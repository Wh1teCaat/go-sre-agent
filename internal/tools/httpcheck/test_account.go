package httpcheck

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
)

// TestAccount binds operator-owned credentials to one exact login URL.
// It is never included in tool arguments, observations or model context.
type TestAccount struct {
	URL, EmailEnv, PasswordEnv, CredentialsFile string
}

func (t *Tool) ConfigureTestAccount(account TestAccount) error {
	u, err := url.Parse(account.URL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("test_account requires an HTTP(S) URL without credentials, query or fragment")
	}
	if !t.allowedHosts.Allows(u.Hostname()) {
		return fmt.Errorf("test_account host is not allowed")
	}
	if err := t.validateMethod(http.MethodPost, u); err != nil {
		return err
	}
	envPair := account.EmailEnv != "" && account.PasswordEnv != ""
	if (account.CredentialsFile == "" && !envPair) || (account.CredentialsFile != "" && (account.EmailEnv != "" || account.PasswordEnv != "")) {
		return fmt.Errorf("test_account requires either credentials_file or email_env and password_env")
	}
	t.testAccount = &account
	return nil
}

func (t *Tool) injectTestAccount(args *Args) error {
	if args.Body != "" {
		return fmt.Errorf("configured login uses the test account; omit body")
	}
	if args.Repeat > 1 {
		return fmt.Errorf("configured login must be performed once; omit repeat")
	}
	email, password := os.Getenv(t.testAccount.EmailEnv), os.Getenv(t.testAccount.PasswordEnv)
	if t.testAccount.CredentialsFile != "" {
		f, err := os.Open(t.testAccount.CredentialsFile)
		if err != nil {
			return fmt.Errorf("read test account credentials file failed")
		}
		defer f.Close()
		var credentials struct {
			Email    string `json:"email"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(io.LimitReader(f, 16384)).Decode(&credentials); err != nil {
			return fmt.Errorf("invalid test account credentials file")
		}
		email, password = credentials.Email, credentials.Password
	}
	if email == "" || password == "" {
		return fmt.Errorf("test account credentials are missing")
	}
	body, _ := json.Marshal(map[string]string{"email": email, "password": password})
	args.Body = string(body)
	headers := http.Header{}
	for key, value := range args.Headers {
		headers.Set(key, value)
	}
	headers.Set("Content-Type", "application/json")
	if headers.Get("X-Request-ID") == "" {
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return fmt.Errorf("generate login request ID failed")
		}
		headers.Set("X-Request-ID", "sre-login-"+hex.EncodeToString(id[:]))
	}
	args.Headers = map[string]string{}
	for key := range headers {
		args.Headers[key] = headers.Get(key)
	}
	// Header canonicalization uses X-Request-Id, while evidence accesses X-Request-ID.
	args.Headers["X-Request-ID"] = headers.Get("X-Request-ID")
	delete(args.Headers, "X-Request-Id")
	return nil
}

// Whitelist factual login results, discarding tokens and arbitrary echoed credentials.
func testAccountResponseFacts(body []byte) map[string]any {
	facts := map[string]any{"response_json_valid": false}
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil || payload == nil {
		return facts
	}
	facts["response_json_valid"] = true
	var code int
	if value, ok := payload["code"]; ok && string(value) != "null" && json.Unmarshal(value, &code) == nil {
		facts["business_code"] = code
	}
	facts["session_issued"] = hasToken(payload)
	return facts
}

func hasToken(payload map[string]json.RawMessage) bool {
	for _, key := range []string{"token", "access_token", "accessToken"} {
		var token string
		if json.Unmarshal(payload[key], &token) == nil && token != "" {
			return true
		}
	}
	var nested map[string]json.RawMessage
	if json.Unmarshal(payload["data"], &nested) == nil && nested != nil {
		for _, key := range []string{"token", "access_token", "accessToken"} {
			var token string
			if json.Unmarshal(nested[key], &token) == nil && token != "" {
				return true
			}
		}
	}
	return false
}
