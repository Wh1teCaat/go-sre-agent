package httpcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
)

const Name = "http_check"

type Args struct {
	URL     string            `json:"url"`
	Method  string            `json:"method,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
}

type Tool struct {
	client       *http.Client
	maxBodyBytes int
	allowedHosts tools.AllowedHosts
	allowedPOST  map[string]struct{}
}

func New(client *http.Client, maxBodyBytes int) *Tool {
	return NewWithAllowedHosts(client, maxBodyBytes, nil)
}

func NewWithAllowedHosts(client *http.Client, maxBodyBytes int, allowedHosts []string) *Tool {
	return NewWithPolicy(client, maxBodyBytes, allowedHosts, nil)
}

// NewWithPolicy 额外声明允许 POST 的精确 URL；其他 URL 只能使用 GET/HEAD。
func NewWithPolicy(client *http.Client, maxBodyBytes int, allowedHosts []string, allowedPOSTURLs []string) *Tool {
	if client == nil {
		client = http.DefaultClient
	}
	if maxBodyBytes <= 0 {
		maxBodyBytes = 512
	}
	allowedPOST := make(map[string]struct{}, len(allowedPOSTURLs))
	for _, rawURL := range allowedPOSTURLs {
		if normalized, err := normalizeURL(rawURL); err == nil && normalized != "" {
			allowedPOST[normalized] = struct{}{}
		}
	}
	return &Tool{
		client:       client,
		maxBodyBytes: maxBodyBytes,
		allowedHosts: tools.NewAllowedHosts(allowedHosts),
		allowedPOST:  allowedPOST,
	}
}

func (t *Tool) Name() string {
	return Name
}

func (t *Tool) Description() string {
	return Spec().Description
}

func (t *Tool) Schema() tools.ToolSchema {
	return Spec().Schema
}

func (t *Tool) Run(ctx context.Context, rawArgs json.RawMessage) (schema.Observation, error) {
	var args Args
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return schema.Observation{}, fmt.Errorf("decode http_check args: %w", err)
	}
	if strings.TrimSpace(args.URL) == "" {
		return schema.Observation{}, fmt.Errorf("http_check requires url")
	}
	parsed, err := url.Parse(args.URL)
	if err != nil {
		return schema.Observation{}, fmt.Errorf("parse url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return schema.Observation{}, fmt.Errorf("url scheme %q is not supported", parsed.Scheme)
	}
	if parsed.Hostname() == "" {
		return schema.Observation{}, fmt.Errorf("url requires host")
	}
	if !t.allowedHosts.Allows(parsed.Hostname()) {
		// HTTP 工具会发真实网络请求，必须在请求构造前做 host allowlist 检查。
		return schema.Observation{}, tools.DisallowedHostError(parsed.Hostname())
	}

	method := strings.ToUpper(strings.TrimSpace(args.Method))
	if method == "" {
		method = http.MethodGet
	}
	if err := t.validateMethod(method, parsed); err != nil {
		return schema.Observation{}, err
	}

	request, err := http.NewRequestWithContext(ctx, method, args.URL, bytes.NewBufferString(args.Body))
	if err != nil {
		return schema.Observation{}, fmt.Errorf("build request: %w", err)
	}
	for key, value := range args.Headers {
		request.Header.Set(key, value)
	}

	startedAt := time.Now()
	response, err := t.requestClient().Do(request)
	latency := time.Since(startedAt)
	if err != nil {
		return schema.Observation{}, fmt.Errorf("execute request: %w", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, int64(t.maxBodyBytes)))
	if err != nil {
		return schema.Observation{}, fmt.Errorf("read response body: %w", err)
	}
	// body 只保留有限片段，并在进入 observation 前脱敏；
	// 该 observation 后续会进入 LLM 上下文和 Markdown 报告。
	bodySnippet := tools.RedactSensitive(string(body))

	latencyMS := latency.Milliseconds()
	if latencyMS < 0 {
		latencyMS = 0
	}
	summary := fmt.Sprintf("%s %s returned %d in %dms", method, args.URL, response.StatusCode, latencyMS)

	return schema.Observation{
		Tool:    Name,
		Summary: summary,
		Data: map[string]any{
			"url":          args.URL,
			"method":       method,
			"status":       response.StatusCode,
			"latency_ms":   latencyMS,
			"body_snippet": bodySnippet,
		},
	}, nil
}

func (t *Tool) validateMethod(method string, target *url.URL) error {
	switch method {
	case http.MethodGet, http.MethodHead:
		return nil
	case http.MethodPost:
		normalized, err := normalizeURL(target.String())
		if err == nil {
			if _, ok := t.allowedPOST[normalized]; ok {
				return nil
			}
		}
		return fmt.Errorf("POST url %q is not allowed", target.String())
	default:
		return fmt.Errorf("http method %q is not allowed", method)
	}
}

func normalizeURL(rawURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "" || parsed.Hostname() == "" {
		return "", fmt.Errorf("url requires scheme and host")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Fragment = ""
	return parsed.String(), nil
}

// requestClient 为本次请求复制 client，并对每个重定向目标重复执行 scheme/host 策略。
// 直接修改共享 client.CheckRedirect 会让并发诊断相互影响。
func (t *Tool) requestClient() *http.Client {
	client := *t.client
	previous := client.CheckRedirect
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if request.URL.Scheme != "http" && request.URL.Scheme != "https" {
			return fmt.Errorf("redirect url scheme %q is not supported", request.URL.Scheme)
		}
		if !t.allowedHosts.Allows(request.URL.Hostname()) {
			return tools.DisallowedHostError(request.URL.Hostname())
		}
		if err := t.validateMethod(request.Method, request.URL); err != nil {
			return err
		}
		if previous != nil {
			return previous(request, via)
		}
		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}
		return nil
	}
	return &client
}

func Spec() tools.ToolSpec {
	return tools.ToolSpec{
		Name:        Name,
		Description: "Request an HTTP endpoint and return status, latency, and a body snippet. GET/HEAD are read-only; POST is limited to configured diagnostic URLs.",
		Schema: tools.ToolSchema{
			Properties: map[string]tools.ArgSpec{
				"url":     {Type: "string", Required: true, Description: "HTTP URL to request."},
				"method":  {Type: "string", Description: "HTTP method, defaults to GET."},
				"headers": {Type: "object", Description: "Optional HTTP headers."},
				"body":    {Type: "string", Description: "Optional request body."},
			},
		},
	}
}
