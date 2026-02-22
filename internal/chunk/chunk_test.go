package chunk

import "testing"

func TestSplitMarkdownRespectsLimit(t *testing.T) {
	input := `# Intro

This is a paragraph with enough content to force the splitter to create multiple chunks.

## Details

- item one
- item two
- item three
`

	chunks := SplitMarkdown(input, 80)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}

	for i, got := range chunks {
		if l := runeLen(got); l > 80 {
			t.Fatalf("chunk %d length=%d, want <=80; chunk=%q", i, l, got)
		}
	}
}

func TestSplitMarkdownKeepsCodeFenceWhole(t *testing.T) {
	input := "```mermaid\nflowchart LR\nA --> B\nB --> C\n```\n"
	chunks := SplitMarkdown(input, 10)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk for fenced code, got %d", len(chunks))
	}
	if chunks[0] != "```mermaid\nflowchart LR\nA --> B\nB --> C\n```" {
		t.Fatalf("unexpected chunk content: %q", chunks[0])
	}
}
