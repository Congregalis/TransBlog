package glossary

import (
	"strings"
	"testing"
)

func TestApplyRespectsCodeBoundaries(t *testing.T) {
	input := strings.Join([]string{
		"Use agent in sentence.",
		"`agent` should stay as code.",
		"```go",
		"// agent remains unchanged in fenced code",
		"const role = \"agent\"",
		"```",
	}, "\n")

	glossaryMap := map[string]string{"agent": "代理"}
	got := Apply(input, glossaryMap)

	want := strings.Join([]string{
		"Use 代理 in sentence.",
		"`agent` should stay as code.",
		"```go",
		"// agent remains unchanged in fenced code",
		"const role = \"agent\"",
		"```",
	}, "\n")

	if got != want {
		t.Fatalf("Apply() mismatch\nwant:\n%s\n\ngot:\n%s", want, got)
	}
}

func TestPromptSorted(t *testing.T) {
	glossaryMap := map[string]string{
		"chunk": "分块",
		"agent": "代理",
	}

	got := Prompt(glossaryMap)
	if !strings.Contains(got, "- agent => 代理") {
		t.Fatalf("missing agent line: %q", got)
	}
	if !strings.Contains(got, "- chunk => 分块") {
		t.Fatalf("missing chunk line: %q", got)
	}
	if strings.Index(got, "- agent => 代理") > strings.Index(got, "- chunk => 分块") {
		t.Fatalf("expected keys sorted lexicographically: %q", got)
	}
}
