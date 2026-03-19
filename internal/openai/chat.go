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
)

type ChatClient struct {
	apiKey     string
	endpoint   string
	httpClient *http.Client
	maxRetries int
}

func NewChatClient(apiKey string, baseURL string, httpClient *http.Client, maxRetries int) *ChatClient {
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

	return &ChatClient{
		apiKey:     apiKey,
		endpoint:   baseURL + "/v1/chat/completions",
		httpClient: httpClient,
		maxRetries: maxRetries,
	}
}

func (c *ChatClient) TranslateMarkdownChunk(ctx context.Context, model string, mdChunk string, glossaryMap map[string]string) (string, error) {
	translated, _, err := c.TranslateMarkdownChunkWithUsage(ctx, model, mdChunk, glossaryMap)
	return translated, err
}

func (c *ChatClient) TranslateMarkdownChunkWithUsage(
	ctx context.Context,
	model string,
	mdChunk string,
	glossaryMap map[string]string,
) (string, Usage, error) {
	return c.translateMarkdownChunk(ctx, model, mdChunk, glossaryMap, false, "")
}

func (c *ChatClient) TranslateMarkdownChunkStrict(
	ctx context.Context,
	model string,
	mdChunk string,
	glossaryMap map[string]string,
	failureReason string,
) (string, error) {
	translated, _, err := c.TranslateMarkdownChunkStrictWithUsage(ctx, model, mdChunk, glossaryMap, failureReason)
	return translated, err
}

func (c *ChatClient) TranslateMarkdownChunkStrictWithUsage(
	ctx context.Context,
	model string,
	mdChunk string,
	glossaryMap map[string]string,
	failureReason string,
) (string, Usage, error) {
	return c.translateMarkdownChunk(ctx, model, mdChunk, glossaryMap, true, failureReason)
}

func (c *ChatClient) translateMarkdownChunk(
	ctx context.Context,
	model string,
	mdChunk string,
	glossaryMap map[string]string,
	strict bool,
	failureReason string,
) (string, Usage, error) {
	systemPrompt := buildSystemPrompt(strict)
	userPrompt := buildUserPrompt(mdChunk, glossaryMap, strict, failureReason)

	// Build messages array for Chat Completions API
	payload := map[string]any{
		"model": model,
		"messages": []map[string]any{
			{
				"role":    "system",
				"content": systemPrompt,
			},
			{
				"role":    "user",
				"content": userPrompt,
			},
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", Usage{}, fmt.Errorf("marshal OpenAI request: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		translated, usage, retry, err := c.callChatCompletions(ctx, body)
		if err == nil {
			return translated, usage, nil
		}

		lastErr = err
		if !retry || attempt == c.maxRetries {
			break
		}

		delay := backoffDelay(attempt)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return "", Usage{}, ctx.Err()
		}
	}

	if lastErr == nil {
		lastErr = errors.New("unknown translation error")
	}
	return "", Usage{}, lastErr
}

func (c *ChatClient) callChatCompletions(ctx context.Context, body []byte) (translated string, usage Usage, retry bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return "", Usage{}, false, fmt.Errorf("build OpenAI request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", Usage{}, true, fmt.Errorf("request OpenAI Chat Completions API: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", Usage{}, true, fmt.Errorf("read OpenAI response body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		message := parseAPIError(respBody)
		err := fmt.Errorf("OpenAI Chat Completions API status %d: %s", resp.StatusCode, message)
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			if retryAfter := parseRetryAfter(resp.Header.Get("Retry-After")); retryAfter > 0 {
				select {
				case <-time.After(retryAfter):
				case <-ctx.Done():
					return "", Usage{}, false, ctx.Err()
				}
			}
			return "", Usage{}, true, err
		}
		return "", Usage{}, false, err
	}

	output, err := extractChatOutputText(respBody)
	if err != nil {
		return "", Usage{}, false, err
	}
	usage = extractChatUsage(respBody)
	return output, usage, false, nil
}

func extractChatOutputText(body []byte) (string, error) {
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}

	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("parse OpenAI Chat response JSON: %w", err)
	}

	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("OpenAI Chat response has no choices")
	}

	content := strings.TrimSpace(parsed.Choices[0].Message.Content)
	if content == "" {
		return "", fmt.Errorf("OpenAI Chat response missing message content")
	}

	return content, nil
}

func extractChatUsage(body []byte) Usage {
	var parsed struct {
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}

	if err := json.Unmarshal(body, &parsed); err != nil {
		return Usage{}
	}

	if parsed.Usage.PromptTokens == 0 && parsed.Usage.CompletionTokens == 0 && parsed.Usage.TotalTokens == 0 {
		return Usage{}
	}

	return Usage{
		InputTokens:  parsed.Usage.PromptTokens,
		OutputTokens: parsed.Usage.CompletionTokens,
		TotalTokens:  parsed.Usage.TotalTokens,
		Available:    true,
	}
}

func parseRetryAfterChat(value string) time.Duration {
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

func backoffDelayChat(attempt int) time.Duration {
	base := time.Second
	delay := base * time.Duration(1<<attempt)
	jitter := time.Duration(rand.Intn(250)) * time.Millisecond
	max := 30 * time.Second
	if delay+jitter > max {
		return max
	}
	return delay + jitter
}
