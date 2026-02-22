package markdown

import (
	"strings"

	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2"
)

func FromHTML(html string) (string, error) {
	markdownText, err := htmltomarkdown.ConvertString(html)
	if err != nil {
		return "", err
	}

	markdownText = strings.ReplaceAll(markdownText, "\r\n", "\n")
	markdownText = strings.TrimSpace(markdownText)
	markdownText = WrapLooseMermaid(markdownText)

	return markdownText, nil
}

func WrapLooseMermaid(markdownText string) string {
	lines := strings.Split(markdownText, "\n")
	if len(lines) == 0 {
		return markdownText
	}

	var out []string
	inFence := false
	fenceDelimiter := ""
	inMermaidCapture := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		if delim, ok := fenceStart(trimmed); ok {
			if inMermaidCapture {
				out = append(out, "```")
				inMermaidCapture = false
			}

			out = append(out, line)
			if inFence && strings.HasPrefix(trimmed, fenceDelimiter) {
				inFence = false
				fenceDelimiter = ""
			} else if !inFence {
				inFence = true
				fenceDelimiter = delim
			}
			continue
		}

		if inFence {
			out = append(out, line)
			continue
		}

		if inMermaidCapture {
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				out = append(out, "```")
				inMermaidCapture = false
				out = append(out, line)
				continue
			}
			out = append(out, line)
			continue
		}

		if isLooseMermaidStart(trimmed) {
			out = append(out, "```mermaid")
			out = append(out, line)
			inMermaidCapture = true
			continue
		}

		out = append(out, line)
	}

	if inMermaidCapture {
		out = append(out, "```")
	}

	return strings.Join(out, "\n")
}

func isLooseMermaidStart(trimmed string) bool {
	if trimmed == "" {
		return false
	}

	lower := strings.ToLower(trimmed)
	prefixes := []string{
		"graph", "flowchart", "sequencediagram", "classdiagram", "statediagram",
		"erdiagram", "journey", "gantt", "mindmap", "timeline", "pie", "gitgraph",
		"quadrantchart", "requirementdiagram",
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func fenceStart(trimmed string) (string, bool) {
	if len(trimmed) < 3 {
		return "", false
	}

	if strings.HasPrefix(trimmed, "```") {
		return repeatedPrefix(trimmed, '`'), true
	}
	if strings.HasPrefix(trimmed, "~~~") {
		return repeatedPrefix(trimmed, '~'), true
	}
	return "", false
}

func repeatedPrefix(s string, marker rune) string {
	var out []rune
	for _, r := range s {
		if r != marker {
			break
		}
		out = append(out, r)
	}
	return string(out)
}
