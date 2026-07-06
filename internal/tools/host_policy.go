package tools

import (
	"fmt"
	"net"
	"strings"
)

// AllowedHosts 表示允许访问的主机集合。空集合表示不限制，通常只在本地 mock/测试中使用。
type AllowedHosts map[string]struct{}

// NewAllowedHosts 规范化配置中的主机名，便于工具执行前做一致的 allowlist 判断。
func NewAllowedHosts(hosts []string) AllowedHosts {
	allowed := make(AllowedHosts, len(hosts))
	for _, host := range hosts {
		normalized := NormalizeHost(host)
		if normalized == "" {
			continue
		}
		allowed[normalized] = struct{}{}
	}
	return allowed
}

// Allows 判断目标主机是否在 allowlist 中。
// 这里按 host 匹配，不匹配 URL path，避免策略和业务路由耦合。
func (h AllowedHosts) Allows(host string) bool {
	if len(h) == 0 {
		return true
	}
	_, ok := h[NormalizeHost(host)]
	return ok
}

// NormalizeHost 统一大小写、去掉端口和 IPv6 方括号，减少等价主机的匹配差异。
func NormalizeHost(host string) string {
	normalized := strings.TrimSpace(strings.ToLower(host))
	if normalized == "" {
		return ""
	}
	if splitHost, _, err := net.SplitHostPort(normalized); err == nil {
		normalized = splitHost
	}
	return strings.Trim(normalized, "[]")
}

// DisallowedHostError 返回统一的主机禁止访问错误，方便测试和上层报告识别。
func DisallowedHostError(host string) error {
	return fmt.Errorf("host %q is not allowed", NormalizeHost(host))
}
