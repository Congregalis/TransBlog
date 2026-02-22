package chunk

import (
	"strings"
	"unicode/utf8"
)

func SplitMarkdown(markdownText string, maxChars int) []string {
	if maxChars <= 0 {
		maxChars = 2000
	}

	markdownText = strings.ReplaceAll(markdownText, "\r\n", "\n")
	markdownText = strings.TrimSpace(markdownText)
	if markdownText == "" {
		return nil
	}

	blocks := parseBlocks(markdownText)
	if len(blocks) == 0 {
		return nil
	}

	var chunks []string
	var current strings.Builder
	currentLen := 0

	flush := func() {
		if current.Len() == 0 {
			return
		}
		chunks = append(chunks, current.String())
		current.Reset()
		currentLen = 0
	}

	for _, block := range blocks {
		blockLen := runeLen(block)

		if blockLen > maxChars {
			flush()
			oversized := splitOversizedBlock(block, maxChars)
			chunks = append(chunks, oversized...)
			continue
		}

		if currentLen == 0 {
			current.WriteString(block)
			currentLen = blockLen
			continue
		}

		if currentLen+2+blockLen <= maxChars {
			current.WriteString("\n\n")
			current.WriteString(block)
			currentLen += 2 + blockLen
			continue
		}

		flush()
		current.WriteString(block)
		currentLen = blockLen
	}

	flush()
	return chunks
}

func parseBlocks(markdownText string) []string {
	lines := strings.Split(markdownText, "\n")
	blocks := make([]string, 0, len(lines))
	current := make([]string, 0, 16)

	inFence := false
	fenceDelimiter := ""

	flush := func() {
		if len(current) == 0 {
			return
		}
		blocks = append(blocks, strings.TrimSpace(strings.Join(current, "\n")))
		current = current[:0]
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if delimiter, ok := fenceStart(trimmed); ok {
			if !inFence {
				flush()
				inFence = true
				fenceDelimiter = delimiter
				current = append(current, line)
				continue
			}

			current = append(current, line)
			if strings.HasPrefix(trimmed, fenceDelimiter) {
				inFence = false
				fenceDelimiter = ""
				flush()
			}
			continue
		}

		if inFence {
			current = append(current, line)
			continue
		}

		if trimmed == "" {
			flush()
			continue
		}

		if strings.HasPrefix(trimmed, "#") {
			flush()
			blocks = append(blocks, line)
			continue
		}

		current = append(current, line)
	}

	flush()
	return blocks
}

func splitOversizedBlock(block string, maxChars int) []string {
	if maxChars <= 0 {
		return []string{block}
	}
	trimmed := strings.TrimSpace(block)
	if _, ok := fenceStart(trimmed); ok {
		return []string{block}
	}

	lines := strings.Split(block, "\n")
	var parts []string
	var current strings.Builder

	appendCurrent := func() {
		if current.Len() == 0 {
			return
		}
		parts = append(parts, current.String())
		current.Reset()
	}

	for _, line := range lines {
		if runeLen(line) > maxChars {
			appendCurrent()
			parts = append(parts, splitLongLine(line, maxChars)...)
			continue
		}

		if current.Len() == 0 {
			current.WriteString(line)
			continue
		}

		candidate := current.String() + "\n" + line
		if runeLen(candidate) <= maxChars {
			current.WriteString("\n")
			current.WriteString(line)
			continue
		}

		appendCurrent()
		current.WriteString(line)
	}

	appendCurrent()
	return parts
}

func splitLongLine(line string, maxChars int) []string {
	if maxChars <= 0 {
		return []string{line}
	}

	runes := []rune(line)
	if len(runes) <= maxChars {
		return []string{line}
	}

	parts := make([]string, 0, len(runes)/maxChars+1)
	for len(runes) > maxChars {
		parts = append(parts, string(runes[:maxChars]))
		runes = runes[maxChars:]
	}
	if len(runes) > 0 {
		parts = append(parts, string(runes))
	}
	return parts
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
	var builder strings.Builder
	for _, r := range s {
		if r != marker {
			break
		}
		builder.WriteRune(r)
	}
	return builder.String()
}

func runeLen(s string) int {
	return utf8.RuneCountInString(s)
}
