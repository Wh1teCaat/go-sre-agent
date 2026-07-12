package llm

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

const (
	DefaultLLMProvider             = "mock"
	DefaultOpenAICompatibleBaseURL = "https://api.openai.com/v1"
	DefaultOpenAICompatibleModel   = "gpt-4o-mini"
	DefaultOllamaBaseURL           = "http://localhost:11434/v1"
	DefaultAnthropicBaseURL        = "https://api.anthropic.com/v1"
)

// Config 是 CLI 选择模型 provider 时使用的通用配置。
// 各 ChatClient 只读取自己协议需要的字段。
type Config struct {
	Provider string
	APIKey   string
	BaseURL  string
	Model    string
}

// LoadConfig 先读取可选 .env，再用真实进程环境覆盖同名配置。
func LoadConfig(path string) (Config, error) {
	values := map[string]string{}
	if path != "" {
		fileValues, err := readDotEnv(path)
		if err != nil {
			return Config{}, err
		}
		for key, value := range fileValues {
			values[key] = value
		}
	}

	for _, key := range []string{
		"SRE_AGENT_LLM_PROVIDER",
		"MIMO_API_KEY", "MIMO_BASE_URL", "MIMO_MODEL",
		"OPENAI_API_KEY", "OPENAI_BASE_URL", "OPENAI_MODEL",
		"OLLAMA_BASE_URL", "OLLAMA_MODEL",
		"ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL",
	} {
		if value, ok := os.LookupEnv(key); ok {
			values[key] = value
		}
	}
	return ConfigFromValues(values), nil
}

func ConfigFromValues(values map[string]string) Config {
	provider := valueOr(values, "SRE_AGENT_LLM_PROVIDER")
	if provider == "" {
		provider = DefaultLLMProvider
	}

	switch provider {
	case "ollama":
		return Config{
			Provider: provider,
			APIKey:   "ollama",
			BaseURL:  valueOrDefault(values, DefaultOllamaBaseURL, "OLLAMA_BASE_URL"),
			Model:    valueOr(values, "OLLAMA_MODEL"),
		}
	case "anthropic":
		return Config{
			Provider: provider,
			APIKey:   valueOr(values, "ANTHROPIC_API_KEY"),
			BaseURL:  valueOrDefault(values, DefaultAnthropicBaseURL, "ANTHROPIC_BASE_URL"),
			Model:    valueOr(values, "ANTHROPIC_MODEL"),
		}
	default:
		return Config{
			Provider: provider,
			APIKey:   valueOr(values, "MIMO_API_KEY", "OPENAI_API_KEY"),
			BaseURL:  valueOrDefault(values, DefaultOpenAICompatibleBaseURL, "MIMO_BASE_URL", "OPENAI_BASE_URL"),
			Model:    valueOrDefault(values, DefaultOpenAICompatibleModel, "MIMO_MODEL", "OPENAI_MODEL"),
		}
	}
}

func valueOr(values map[string]string, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(values[key]); value != "" {
			return value
		}
	}
	return ""
}

func valueOrDefault(values map[string]string, fallback string, keys ...string) string {
	if value := valueOr(values, keys...); value != "" {
		return value
	}
	return fallback
}

func readDotEnv(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open env file: %w", err)
	}
	defer file.Close()

	values := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("invalid env line %q", line)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("invalid empty env key in line %q", line)
		}
		values[key] = strings.Trim(strings.TrimSpace(value), `"'`)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan env file: %w", err)
	}
	return values, nil
}
