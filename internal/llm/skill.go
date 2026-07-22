package llm

import (
	"fmt"
	"os"
	"strings"
)

const maxSkillBytes = 128 * 1024

// LoadSkill 只在进程启动时读取 skill；后续每个 step 复用同一份内容。
func LoadSkill(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("diagnostic skill path is empty")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read diagnostic skill %q: %w", path, err)
	}
	if len(data) == 0 {
		return "", fmt.Errorf("diagnostic skill %q is empty", path)
	}
	if len(data) > maxSkillBytes {
		return "", fmt.Errorf("diagnostic skill %q exceeds %d bytes", path, maxSkillBytes)
	}

	content := stripSkillFrontmatter(strings.TrimSpace(string(data)))
	if content == "" {
		return "", fmt.Errorf("diagnostic skill %q has no instructions", path)
	}
	return content, nil
}

func stripSkillFrontmatter(content string) string {
	if !strings.HasPrefix(content, "---\n") {
		return content
	}
	rest := content[len("---\n"):]
	if end := strings.Index(rest, "\n---"); end >= 0 {
		return strings.TrimSpace(rest[end+len("\n---"):])
	}
	return content
}
