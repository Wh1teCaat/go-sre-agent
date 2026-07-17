package llm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadSkillStripsFrontmatterAndHashesSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "SKILL.md")
	source := "---\nname: example\ndescription: test\n---\n\nUse only observed evidence.\n"
	if err := os.WriteFile(path, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}

	skill, err := LoadSkill(path)
	if err != nil {
		t.Fatalf("load skill: %v", err)
	}
	if skill.Content != "Use only observed evidence." {
		t.Fatalf("content = %q", skill.Content)
	}
	if skill.SHA256 == "" || len(skill.SHA256) != 64 {
		t.Fatalf("sha256 = %q", skill.SHA256)
	}
	if strings.Contains(skill.Content, "description:") {
		t.Fatalf("frontmatter leaked into model content: %q", skill.Content)
	}
}

func TestLoadSkillRejectsEmptyInstructions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "SKILL.md")
	if err := os.WriteFile(path, []byte("---\nname: empty\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSkill(path); err == nil {
		t.Fatal("empty skill succeeded")
	}
}
