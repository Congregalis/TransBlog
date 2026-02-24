package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"

	"transblog/internal/glossary"
)

const (
	defaultBaseURL    = "https://api.openai.com/v1"
	maxErrBody        = 2048
	defaultMaxRetries = 5
)

type Client struct {
	apiKey     string
	endpoint   string
	httpClient *http.Client
	maxRetries int
}

func NewClient(apiKey string, baseURL string, httpClient *http.Client, maxRetries int) *Client {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = defaultBaseURL
	}
	baseURL = strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	if strings.HasSuffix(baseURL, "/v1") {
		baseURL = strings.TrimSuffix(baseURL, "/v1")
	}
	if maxRetries < 0 {
		maxRetries = defaultMaxRetries
	}

	return &Client{
		apiKey:     apiKey,
		endpoint:   baseURL + "/v1/responses",
		httpClient: httpClient,
		maxRetries: maxRetries,
	}
}

func (c *Client) TranslateMarkdownChunk(ctx context.Context, model string, mdChunk string, glossaryMap map[string]string) (string, error) {
	systemPrompt := strings.Join([]string{
		"Translate English Markdown to Simplified Chinese.",
		"Preserve Markdown layout and syntax exactly.",
		"Do not translate code fences, inline code, or URLs.",
		"Keep link targets unchanged.",
		"Return only translated Markdown with no commentary.",
	}, " ")

	userPrompt := buildUserPrompt(mdChunk, glossaryMap)

	// Build input array for the new API format
	payload := map[string]any{
		"model": model,
		"input": []map[string]any{
			{
				"type": "message",
				"role": "developer",
				"content": []map[string]any{
					{
						"type": "input_text",
						"text": systemPrompt,
					},
				},
			},
			{
				"type": "message",
				"role": "user",
				"content": []map[string]any{
					{
						"type": "input_text",
						"text": userPrompt,
					},
				},
			},
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal OpenAI request: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		translated, retry, err := c.callResponses(ctx, body)
		if err == nil {
			return translated, nil
		}

		lastErr = err
		if !retry || attempt == c.maxRetries {
			break
		}

		delay := backoffDelay(attempt)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	if lastErr == nil {
		lastErr = errors.New("unknown translation error")
	}
	return "", lastErr
}

func (c *Client) callResponses(ctx context.Context, body []byte) (translated string, retry bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return "", false, fmt.Errorf("build OpenAI request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", true, fmt.Errorf("request OpenAI Responses API: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", true, fmt.Errorf("read OpenAI response body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		message := parseAPIError(respBody)
		err := fmt.Errorf("OpenAI Responses API status %d: %s", resp.StatusCode, message)
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			if retryAfter := parseRetryAfter(resp.Header.Get("Retry-After")); retryAfter > 0 {
				select {
				case <-time.After(retryAfter):
				case <-ctx.Done():
					return "", false, ctx.Err()
				}
			}
			return "", true, err
		}
		return "", false, err
	}

	output, err := extractOutputText(respBody)
	if err != nil {
		return "", false, err
	}
	return output, false, nil
}

func buildUserPrompt(mdChunk string, glossaryMap map[string]string) string {
	var builder strings.Builder
	builder.WriteString("Translate the following Markdown into Simplified Chinese.\n")
	builder.WriteString("Keep Markdown syntax, headings, list markers, and links intact.\n")
	builder.WriteString("Do not translate inline code or fenced code blocks.\n")
	if len(glossaryMap) > 0 {
		builder.WriteString(glossary.Prompt(glossaryMap))
		builder.WriteString("\n")
	}
	builder.WriteString("\nMarkdown chunk:\n")
	builder.WriteString(mdChunk)
	return builder.String()
}

func parseAPIError(body []byte) string {
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.Unmarshal(body, &parsed); err == nil && strings.TrimSpace(parsed.Error.Message) != "" {
		return parsed.Error.Message
	}

	snippet := strings.TrimSpace(string(body))
	if len(snippet) > maxErrBody {
		snippet = snippet[:maxErrBody] + "..."
	}
	if snippet == "" {
		return "empty error response"
	}
	return snippet
}

func extractOutputText(body []byte) (string, error) {
	var parsed struct {
		OutputText string `json:"output_text"`
		Output     []struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}

	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("parse OpenAI response JSON: %w", err)
	}

	if text := strings.TrimSpace(parsed.OutputText); text != "" {
		return text, nil
	}

	var builder strings.Builder
	for _, item := range parsed.Output {
		for _, content := range item.Content {
			if content.Type == "output_text" && content.Text != "" {
				if builder.Len() > 0 {
					builder.WriteString("\n")
				}
				builder.WriteString(content.Text)
			}
		}
	}

	if builder.Len() == 0 {
		return "", fmt.Errorf("OpenAI response missing output_text")
	}

	return strings.TrimSpace(builder.String()), nil
}

func parseRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}

	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}

	if ts, err := http.ParseTime(value); err == nil {
		delta := time.Until(ts)
		if delta > 0 {
			return delta
		}
	}

	return 0
}

func backoffDelay(attempt int) time.Duration {
	base := time.Second
	delay := base * time.Duration(1<<attempt)
	jitter := time.Duration(rand.Intn(250)) * time.Millisecond
	max := 30 * time.Second
	if delay+jitter > max {
		return max
	}
	return delay + jitter
}
