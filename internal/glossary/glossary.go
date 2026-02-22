package glossary

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

func Load(path string) (map[string]string, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read glossary file %s: %w", path, err)
	}

	var data map[string]string
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("parse glossary JSON %s: %w", path, err)
	}

	cleaned := make(map[string]string, len(data))
	for k, v := range data {
		key := strings.TrimSpace(k)
		val := strings.TrimSpace(v)
		if key == "" || val == "" {
			continue
		}
		cleaned[key] = val
	}

	return cleaned, nil
}

func Prompt(glossary map[string]string) string {
	if len(glossary) == 0 {
		return ""
	}

	keys := make([]string, 0, len(glossary))
	for key := range glossary {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var builder strings.Builder
	builder.WriteString("Glossary (enforce exact preferred translations):\n")
	for _, key := range keys {
		builder.WriteString("- ")
		builder.WriteString(key)
		builder.WriteString(" => ")
		builder.WriteString(glossary[key])
		builder.WriteString("\n")
	}
	return strings.TrimSuffix(builder.String(), "\n")
}

func Apply(markdownText string, glossary map[string]string) string {
	if len(glossary) == 0 || markdownText == "" {
		return markdownText
	}

	replacer := buildReplacer(glossary)
	lines := strings.Split(markdownText, "\n")

	var out []string
	inFence := false
	fenceDelimiter := ""

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if delimiter, ok := fenceStart(trimmed); ok {
			out = append(out, line)
			if inFence && strings.HasPrefix(trimmed, fenceDelimiter) {
				inFence = false
				fenceDelimiter = ""
			} else if !inFence {
				inFence = true
				fenceDelimiter = delimiter
			}
			continue
		}

		if inFence {
			out = append(out, line)
			continue
		}

		out = append(out, applyOutsideInlineCode(line, replacer))
	}

	return strings.Join(out, "\n")
}

func buildReplacer(glossary map[string]string) *strings.Replacer {
	type pair struct {
		key   string
		value string
	}

	pairs := make([]pair, 0, len(glossary))
	for key, value := range glossary {
		pairs = append(pairs, pair{key: key, value: value})
	}

	sort.SliceStable(pairs, func(i, j int) bool {
		return len([]rune(pairs[i].key)) > len([]rune(pairs[j].key))
	})

	replacements := make([]string, 0, len(pairs)*2)
	for _, p := range pairs {
		replacements = append(replacements, p.key, p.value)
	}
	return strings.NewReplacer(replacements...)
}

func applyOutsideInlineCode(line string, replacer *strings.Replacer) string {
	var builder strings.Builder
	var segment strings.Builder
	inInlineCode := false

	for _, r := range line {
		if r == '`' {
			if inInlineCode {
				builder.WriteString(segment.String())
				segment.Reset()
				builder.WriteRune('`')
				inInlineCode = false
			} else {
				builder.WriteString(replacer.Replace(segment.String()))
				segment.Reset()
				builder.WriteRune('`')
				inInlineCode = true
			}
			continue
		}
		segment.WriteRune(r)
	}

	if inInlineCode {
		builder.WriteString(segment.String())
	} else {
		builder.WriteString(replacer.Replace(segment.String()))
	}

	return builder.String()
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
