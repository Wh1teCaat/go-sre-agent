package httpcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
)

const Name = "http_check"

// maxRepeat 限制单次 action 的采样上限，避免模型对目标服务放大请求量。
const maxRepeat = 10

type Args struct {
	URL     string            `json:"url"`
	Method  string            `json:"method,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
	Repeat  int               `json:"repeat,omitempty"`
}

type Tool struct {
	maxBodyBytes           int
	allowedHosts           tools.AllowedHosts
	allowedPOST            map[string]struct{}
	allowedResponseHeaders map[string]string
}

// NewWithPolicy 额外声明允许 POST 的精确 URL；其他 URL 只能使用 GET/HEAD。
// responseHeaders 是可选的响应头白名单，避免把未授权响应头送入 trace 或模型上下文。
func NewWithPolicy(maxBodyBytes int, allowedHosts []string, allowedPOSTURLs []string, responseHeaders ...[]string) *Tool {
	if maxBodyBytes <= 0 {
		maxBodyBytes = 512
	}
	allowedPOST := make(map[string]struct{}, len(allowedPOSTURLs))
	for _, rawURL := range allowedPOSTURLs {
		if normalized, err := normalizeURL(rawURL); err == nil && normalized != "" {
			allowedPOST[normalized] = struct{}{}
		}
	}
	allowedResponseHeaders := make(map[string]string)
	if len(responseHeaders) > 0 {
		for _, header := range responseHeaders[0] {
			header = strings.TrimSpace(header)
			if header == "" || len([]rune(header)) > 128 || strings.ContainsAny(header, "\r\n:") {
				continue
			}
			allowedResponseHeaders[strings.ToLower(header)] = header
		}
	}
	return &Tool{
		maxBodyBytes:           maxBodyBytes,
		allowedHosts:           tools.NewAllowedHosts(allowedHosts),
		allowedPOST:            allowedPOST,
		allowedResponseHeaders: allowedResponseHeaders,
	}
}

func (t *Tool) Spec() tools.ToolSpec {
	return tools.ToolSpec{
		Name:        Name,
		Description: "Request an HTTP endpoint and return status, latency, response request_id, a bounded body snippet, and operator-allowed response headers. Put a user-specified reproduction request_id in the X-Request-ID header; use the returned request_id for later log correlation. GET/HEAD are read-only; POST is limited to configured diagnostic URLs.",
		Schema: tools.ToolSchema{
			Properties: map[string]tools.ArgSpec{
				"url":     {Type: "string", Required: true, Description: "HTTP URL to request."},
				"method":  {Type: "string", Description: "HTTP method, defaults to GET."},
				"headers": {Type: "object", Description: "Optional HTTP headers."},
				"body":    {Type: "string", Description: "Optional request body."},
				"repeat":  {Type: "number", Description: "Optional attempt count (2-10) to sample an intermittently failing endpoint; reports per-status counts and the latency range."},
			},
		},
	}
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

	repeat := args.Repeat
	if repeat <= 0 {
		repeat = 1
	}
	if repeat > maxRepeat {
		repeat = maxRepeat
	}

	client := t.requestClient()
	statusCounts := map[string]int{}
	var latencies []int64
	transportErrors := 0
	var lastErr error
	lastStatus := 0
	var lastLatencyMS int64
	bodySnippet := ""
	requestID := ""
	lastResponseHeaders := map[string]string{}
	responseHeaderValues := map[string]map[string]int{}
	for attempt := 0; attempt < repeat; attempt++ {
		request, err := http.NewRequestWithContext(ctx, method, args.URL, bytes.NewBufferString(args.Body))
		if err != nil {
			return schema.Observation{}, fmt.Errorf("build request: %w", err)
		}
		for key, value := range args.Headers {
			request.Header.Set(key, value)
		}

		startedAt := time.Now()
		response, err := client.Do(request)
		latencyMS := time.Since(startedAt).Milliseconds()
		if latencyMS < 0 {
			latencyMS = 0
		}
		if err != nil {
			// 偶发故障的失败尝试也是采样数据；只有全部失败才让整个工具报错。
			transportErrors++
			lastErr = fmt.Errorf("execute request: %w", err)
			statusCounts["error"]++
			continue
		}
		body, err := io.ReadAll(io.LimitReader(response.Body, int64(t.maxBodyBytes)))
		response.Body.Close()
		if err != nil {
			transportErrors++
			lastErr = fmt.Errorf("read response body: %w", err)
			statusCounts["error"]++
			continue
		}
		// body 只保留有限片段，并在进入 observation 前脱敏；
		// 该 observation 后续会进入 LLM 上下文和 Markdown 报告。
		bodySnippet = tools.RedactSensitive(string(body))
		requestID = strings.TrimSpace(response.Header.Get("X-Request-ID"))
		lastResponseHeaders = t.collectResponseHeaders(response.Header)
		for header, value := range lastResponseHeaders {
			if responseHeaderValues[header] == nil {
				responseHeaderValues[header] = make(map[string]int)
			}
			responseHeaderValues[header][value]++
		}
		lastStatus = response.StatusCode
		lastLatencyMS = latencyMS
		statusCounts[strconv.Itoa(response.StatusCode)]++
		latencies = append(latencies, latencyMS)
	}
	if len(latencies) == 0 {
		return schema.Observation{}, lastErr
	}

	minLatency, maxLatency := latencies[0], latencies[0]
	for _, latency := range latencies[1:] {
		minLatency = min(minLatency, latency)
		maxLatency = max(maxLatency, latency)
	}

	summary := fmt.Sprintf("%s %s returned %d in %dms", method, args.URL, lastStatus, lastLatencyMS)
	data := map[string]any{
		"url":              args.URL,
		"method":           method,
		"status":           lastStatus,
		"latency_ms":       lastLatencyMS,
		"body_snippet":     bodySnippet,
		"request_id":       requestID,
		"response_headers": lastResponseHeaders,
	}
	if repeat > 1 {
		summary = fmt.Sprintf("%s %s sampled %d times: %s; latency %d-%dms", method, args.URL, repeat, formatStatusCounts(statusCounts), minLatency, maxLatency)
		data["attempts"] = repeat
		data["status_counts"] = statusCounts
		data["transport_errors"] = transportErrors
		data["latency_ms_min"] = minLatency
		data["latency_ms_max"] = maxLatency
		data["response_header_values"] = responseHeaderValues
	}

	return schema.Observation{
		Tool:    Name,
		Summary: summary,
		Data:    data,
	}, nil
}

// collectResponseHeaders 仅提取运营者白名单中的响应头，并限制单个值长度与控制字符。
func (t *Tool) collectResponseHeaders(headers http.Header) map[string]string {
	collected := make(map[string]string, len(t.allowedResponseHeaders))
	for normalized, configuredName := range t.allowedResponseHeaders {
		values := headers.Values(configuredName)
		if len(values) == 0 {
			continue
		}
		value := strings.Join(values, ", ")
		value = strings.Map(func(r rune) rune {
			if r < 0x20 || r == 0x7f {
				return -1
			}
			return r
		}, value)
		if runes := []rune(value); len(runes) > 256 {
			value = string(runes[:256]) + "..."
		}
		collected[normalized] = tools.RedactSensitive(value)
	}
	return collected
}

// formatStatusCounts 输出确定性排序的状态分布，例如 "200x4, 500x1, errorx1"。
func formatStatusCounts(counts map[string]int) string {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%sx%d", key, counts[key]))
	}
	return strings.Join(parts, ", ")
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
	client := *http.DefaultClient
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
		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}
		return nil
	}
	return &client
}
