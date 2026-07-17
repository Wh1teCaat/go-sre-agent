package llm

import (
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
)

const maxSkillBytes = 128 * 1024

// Skill 是一次运行使用的、不可变的模型行为契约。
type Skill struct {
	Path    string
	Content string
	SHA256  string
}

// LoadSkill 只在进程启动时读取 skill；后续每个 step 复用同一份内容。
func LoadSkill(path string) (Skill, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return Skill{}, fmt.Errorf("diagnostic skill path is empty")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Skill{}, fmt.Errorf("read diagnostic skill %q: %w", path, err)
	}
	if len(data) == 0 {
		return Skill{}, fmt.Errorf("diagnostic skill %q is empty", path)
	}
	if len(data) > maxSkillBytes {
		return Skill{}, fmt.Errorf("diagnostic skill %q exceeds %d bytes", path, maxSkillBytes)
	}

	content := stripSkillFrontmatter(strings.TrimSpace(string(data)))
	if content == "" {
		return Skill{}, fmt.Errorf("diagnostic skill %q has no instructions", path)
	}
	hash := sha256.Sum256(data)
	return Skill{Path: path, Content: content, SHA256: fmt.Sprintf("%x", hash)}, nil
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
