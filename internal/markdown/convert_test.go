package markdown

import (
	"strings"
	"testing"
)

func TestWrapLooseMermaidWrapsLooseBlock(t *testing.T) {
	t.Parallel()

	input := "# Diagram\n\ngraph TD\nA-->B\n\n## Next\nBody"
	output := WrapLooseMermaid(input)

	want := "# Diagram\n\n```mermaid\ngraph TD\nA-->B\n```\n\n## Next\nBody"
	if output != want {
		t.Fatalf("WrapLooseMermaid() output mismatch\nwant:\n%s\n\ngot:\n%s", want, output)
	}
}

func TestWrapLooseMermaidRespectsFenceBoundaries(t *testing.T) {
	t.Parallel()

	input := "```\ngraph TD\nA-->B\n```\n\nflowchart LR\nA-->B"
	output := WrapLooseMermaid(input)

	if !strings.Contains(output, "```\ngraph TD\nA-->B\n```") {
		t.Fatalf("expected existing fenced block unchanged, got:\n%s", output)
	}
	if !strings.Contains(output, "```mermaid\nflowchart LR\nA-->B\n```") {
		t.Fatalf("expected loose mermaid to be wrapped, got:\n%s", output)
	}
}

func TestWrapLooseMermaidClosesFenceAtEOF(t *testing.T) {
	t.Parallel()

	input := "graph TD\nA-->B"
	output := WrapLooseMermaid(input)

	want := "```mermaid\ngraph TD\nA-->B\n```"
	if output != want {
		t.Fatalf("WrapLooseMermaid() = %q, want %q", output, want)
	}
}

func TestFromHTMLEmptyContentReturnsEmptyMarkdown(t *testing.T) {
	t.Parallel()

	output, err := FromHTML("<!doctype html><html><head><title>x</title></head><body></body></html>")
	if err != nil {
		t.Fatalf("FromHTML() error = %v, want nil", err)
	}
	if output != "" {
		t.Fatalf("FromHTML() output = %q, want empty", output)
	}
}
