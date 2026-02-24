package openai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTranslateMarkdownChunkWithUsageRetriesThenSucceeds(t *testing.T) {
	t.Parallel()

	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}

		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"temporary failure"}}`)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output_text":"translated","usage":{"input_tokens":12,"output_tokens":3,"total_tokens":15}}`)
	}))
	t.Cleanup(server.Close)

	client := NewClient("test-key", server.URL, &http.Client{Timeout: 3 * time.Second}, 1)

	got, usage, err := client.TranslateMarkdownChunkWithUsage(context.Background(), "gpt-5.2", "# title", nil)
	if err != nil {
		t.Fatalf("TranslateMarkdownChunkWithUsage() error = %v", err)
	}
	if got != "translated" {
		t.Fatalf("translated = %q, want %q", got, "translated")
	}
	if !usage.Available || usage.InputTokens != 12 || usage.OutputTokens != 3 || usage.TotalTokens != 15 {
		t.Fatalf("usage = %+v, want available usage values", usage)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("request calls = %d, want 2", calls)
	}
}

func TestTranslateMarkdownChunkDoesNotRetryOnBadRequest(t *testing.T) {
	t.Parallel()

	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"bad request"}}`)
	}))
	t.Cleanup(server.Close)

	client := NewClient("test-key", server.URL, &http.Client{Timeout: 2 * time.Second}, 5)

	_, err := client.TranslateMarkdownChunk(context.Background(), "gpt-5.2", "# title", nil)
	if err == nil {
		t.Fatalf("TranslateMarkdownChunk() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "status 400") {
		t.Fatalf("TranslateMarkdownChunk() error = %v, want status 400", err)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("request calls = %d, want 1", calls)
	}
}

func TestExtractOutputTextFallsBackToOutputArray(t *testing.T) {
	t.Parallel()

	body := []byte(`{"output":[{"content":[{"type":"output_text","text":"first"},{"type":"other","text":"ignored"}]},{"content":[{"type":"output_text","text":"second"}]}]}`)
	got, err := extractOutputText(body)
	if err != nil {
		t.Fatalf("extractOutputText() error = %v", err)
	}
	if got != "first\nsecond" {
		t.Fatalf("extractOutputText() = %q, want %q", got, "first\nsecond")
	}
}

func TestExtractOutputTextReturnsErrorWhenMissingText(t *testing.T) {
	t.Parallel()

	_, err := extractOutputText([]byte(`{"output":[{"content":[{"type":"reasoning","text":"r"}]}]}`))
	if err == nil {
		t.Fatalf("extractOutputText() error = nil, want missing output error")
	}
	if !strings.Contains(err.Error(), "missing output_text") {
		t.Fatalf("extractOutputText() error = %v, want missing output_text message", err)
	}
}

func TestParseAPIErrorPrefersJSONMessage(t *testing.T) {
	t.Parallel()

	got := parseAPIError([]byte(`{"error":{"message":"quota exceeded"}}`))
	if got != "quota exceeded" {
		t.Fatalf("parseAPIError() = %q, want %q", got, "quota exceeded")
	}
}

func TestExtractUsageMissingReturnsUnavailable(t *testing.T) {
	t.Parallel()

	usage := extractUsage([]byte(`{"output_text":"ok"}`))
	if usage.Available {
		t.Fatalf("usage.Available = true, want false")
	}
	if usage.InputTokens != 0 || usage.OutputTokens != 0 || usage.TotalTokens != 0 {
		t.Fatalf("usage values = %+v, want zero values", usage)
	}
}
