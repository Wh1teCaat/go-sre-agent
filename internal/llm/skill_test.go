package llm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadSkillStripsFrontmatter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "SKILL.md")
	source := "---\nname: example\ndescription: test\n---\n\nUse only observed evidence.\n"
	if err := os.WriteFile(path, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}

	skill, err := LoadSkill(path)
	if err != nil {
		t.Fatalf("load skill: %v", err)
	}
	if skill != "Use only observed evidence." {
		t.Fatalf("content = %q", skill)
	}
	if strings.Contains(skill, "description:") {
		t.Fatalf("frontmatter leaked into model content: %q", skill)
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
