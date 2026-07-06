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
}

func New(client *http.Client, maxBodyBytes int) *Tool {
	return NewWithAllowedHosts(client, maxBodyBytes, nil)
}

func NewWithAllowedHosts(client *http.Client, maxBodyBytes int, allowedHosts []string) *Tool {
	if client == nil {
		client = http.DefaultClient
	}
	if maxBodyBytes <= 0 {
		maxBodyBytes = 512
	}
	return &Tool{
		client:       client,
		maxBodyBytes: maxBodyBytes,
		allowedHosts: tools.NewAllowedHosts(allowedHosts),
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

	request, err := http.NewRequestWithContext(ctx, method, args.URL, bytes.NewBufferString(args.Body))
	if err != nil {
		return schema.Observation{}, fmt.Errorf("build request: %w", err)
	}
	for key, value := range args.Headers {
		request.Header.Set(key, value)
	}

	startedAt := time.Now()
	response, err := t.client.Do(request)
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

func Spec() tools.ToolSpec {
	return tools.ToolSpec{
		Name:        Name,
		Description: "Request an HTTP endpoint and return status, latency, and a body snippet.",
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
