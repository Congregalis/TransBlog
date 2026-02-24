package cli

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunMultiURLWritesPerURLOutputsAndSummary(t *testing.T) {
	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/post-a":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(sampleArticle("Post A", "content A")))
		case "/post-b":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(sampleArticle("Post B", "content B")))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(contentServer.Close)

	openAIServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output_text":"译文内容"}`)
	}))
	t.Cleanup(openAIServer.Close)

	tmpDir := useTempWorkingDir(t)
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", openAIServer.URL)

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	err := Run([]string{contentServer.URL + "/post-a", contentServer.URL + "/post-b"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("Run() error = %v; stderr=%s", err, stderr.String())
	}

	summaryPath := filepath.Join(tmpDir, "out", "_summary.json")
	rawSummary, err := os.ReadFile(summaryPath)
	if err != nil {
		t.Fatalf("read summary file: %v", err)
	}

	var summary taskSummary
	if err := json.Unmarshal(rawSummary, &summary); err != nil {
		t.Fatalf("unmarshal summary JSON: %v", err)
	}

	if summary.TotalURLs != 2 || summary.SuccessCount != 2 || summary.FailureCount != 0 {
		t.Fatalf("unexpected summary counters: %+v", summary)
	}
	if len(summary.Results) != 2 {
		t.Fatalf("summary results len = %d, want 2", len(summary.Results))
	}
	for i, item := range summary.Results {
		if !item.Success {
			t.Fatalf("result %d success=false, error=%q", i, item.ErrorMessage)
		}
		if item.OutputPath == "" {
			t.Fatalf("result %d missing output path", i)
		}
		if _, err := os.Stat(item.OutputPath); err != nil {
			t.Fatalf("result %d output file %q not found: %v", i, item.OutputPath, err)
		}
	}

	entries, err := os.ReadDir(filepath.Join(tmpDir, "out"))
	if err != nil {
		t.Fatalf("ReadDir(out): %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("out file count = %d, want 3", len(entries))
	}

	if !strings.Contains(stdout.String(), "Done: 2 succeeded, 0 failed") {
		t.Fatalf("stdout missing final summary: %s", stdout.String())
	}
}

func TestRunMultiURLContinuesAfterSingleURLFailure(t *testing.T) {
	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(sampleArticle("OK", "good content")))
		case "/bad":
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(contentServer.Close)

	openAIServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output_text":"译文内容"}`)
	}))
	t.Cleanup(openAIServer.Close)

	tmpDir := useTempWorkingDir(t)
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", openAIServer.URL)

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	err := Run([]string{contentServer.URL + "/ok", contentServer.URL + "/bad"}, &stdout, &stderr)
	if err == nil {
		t.Fatalf("Run() error = nil, want partial-failure error")
	}
	if !strings.Contains(err.Error(), "1 URL(s) failed") {
		t.Fatalf("Run() error = %q, want partial-failure message", err.Error())
	}

	summaryPath := filepath.Join(tmpDir, "out", "_summary.json")
	rawSummary, err := os.ReadFile(summaryPath)
	if err != nil {
		t.Fatalf("read summary file: %v", err)
	}

	var summary taskSummary
	if err := json.Unmarshal(rawSummary, &summary); err != nil {
		t.Fatalf("unmarshal summary JSON: %v", err)
	}

	if summary.SuccessCount != 1 || summary.FailureCount != 1 {
		t.Fatalf("unexpected summary counters: %+v", summary)
	}

	successFound := false
	failureFound := false
	for _, item := range summary.Results {
		if item.Success {
			successFound = true
			if item.OutputPath == "" {
				t.Fatalf("success item missing output path")
			}
			if _, err := os.Stat(item.OutputPath); err != nil {
				t.Fatalf("success output %q not found: %v", item.OutputPath, err)
			}
			continue
		}
		failureFound = true
		if item.ErrorType != errorTypeFetch {
			t.Fatalf("failure item error_type=%q, want %q", item.ErrorType, errorTypeFetch)
		}
		if item.ErrorMessage == "" {
			t.Fatalf("failure item missing error message")
		}
	}

	if !successFound || !failureFound {
		t.Fatalf("expected one success and one failure, got %+v", summary.Results)
	}

	if !strings.Contains(stdout.String(), "Done: 1 succeeded, 1 failed") {
		t.Fatalf("stdout missing final summary: %s", stdout.String())
	}
}

func TestRunSummaryIncludesTranslateErrorType(t *testing.T) {
	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(sampleArticle("OK", "good content")))
		case "/translate-bad":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(sampleArticle("Bad", "translate_fail_marker")))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(contentServer.Close)

	var requestCount int32
	openAIServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}

		if atomic.AddInt32(&requestCount, 1) == 2 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"forced translate failure"}}`)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output_text":"译文内容"}`)
	}))
	t.Cleanup(openAIServer.Close)

	tmpDir := useTempWorkingDir(t)
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", openAIServer.URL)

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	err := Run([]string{contentServer.URL + "/ok", contentServer.URL + "/translate-bad"}, &stdout, &stderr)
	if err == nil {
		t.Fatalf("Run() error = nil, want partial-failure error")
	}
	if !strings.Contains(err.Error(), "1 URL(s) failed") {
		t.Fatalf("Run() error = %q, want partial-failure message", err.Error())
	}

	summaryPath := filepath.Join(tmpDir, "out", "_summary.json")
	rawSummary, err := os.ReadFile(summaryPath)
	if err != nil {
		t.Fatalf("read summary file: %v", err)
	}

	var summary taskSummary
	if err := json.Unmarshal(rawSummary, &summary); err != nil {
		t.Fatalf("unmarshal summary JSON: %v", err)
	}

	if summary.SuccessCount != 1 || summary.FailureCount != 1 {
		t.Fatalf("unexpected summary counters: %+v", summary)
	}

	var translateFailure *summaryItem
	for i := range summary.Results {
		item := &summary.Results[i]
		if item.Success {
			continue
		}
		translateFailure = item
	}
	if translateFailure == nil {
		t.Fatalf("expected one translate failure in summary, got %+v", summary.Results)
	}
	if translateFailure.ErrorType != errorTypeTranslate {
		t.Fatalf("error_type = %q, want %q", translateFailure.ErrorType, errorTypeTranslate)
	}
	if !strings.Contains(translateFailure.ErrorMessage, "OpenAI Responses API status 400") {
		t.Fatalf("error_message=%q, want OpenAI 400 context", translateFailure.ErrorMessage)
	}
}

func TestRunSummaryIncludesOutputErrorType(t *testing.T) {
	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/blocked":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(sampleArticle("Blocked", "blocked content")))
		case "/ok":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(sampleArticle("OK", "good content")))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(contentServer.Close)

	openAIServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output_text":"译文内容"}`)
	}))
	t.Cleanup(openAIServer.Close)

	tmpDir := useTempWorkingDir(t)
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", openAIServer.URL)

	blockedURL := contentServer.URL + "/blocked"
	blockedFilename, err := filenameFromURL(blockedURL, false)
	if err != nil {
		t.Fatalf("filenameFromURL(%q): %v", blockedURL, err)
	}
	blockedPath := filepath.Join(tmpDir, "out", blockedFilename)
	if err := os.MkdirAll(blockedPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", blockedPath, err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	err = Run([]string{blockedURL, contentServer.URL + "/ok"}, &stdout, &stderr)
	if err == nil {
		t.Fatalf("Run() error = nil, want partial-failure error")
	}
	if !strings.Contains(err.Error(), "1 URL(s) failed") {
		t.Fatalf("Run() error = %q, want partial-failure message", err.Error())
	}

	summaryPath := filepath.Join(tmpDir, "out", "_summary.json")
	rawSummary, err := os.ReadFile(summaryPath)
	if err != nil {
		t.Fatalf("read summary file: %v", err)
	}

	var summary taskSummary
	if err := json.Unmarshal(rawSummary, &summary); err != nil {
		t.Fatalf("unmarshal summary JSON: %v", err)
	}

	if summary.SuccessCount != 1 || summary.FailureCount != 1 {
		t.Fatalf("unexpected summary counters: %+v", summary)
	}

	var outputFailure *summaryItem
	for i := range summary.Results {
		item := &summary.Results[i]
		if item.Success {
			continue
		}
		outputFailure = item
	}
	if outputFailure == nil {
		t.Fatalf("expected one output failure in summary, got %+v", summary.Results)
	}
	if outputFailure.ErrorType != errorTypeOutput {
		t.Fatalf("error_type = %q, want %q", outputFailure.ErrorType, errorTypeOutput)
	}
	if !strings.Contains(outputFailure.ErrorMessage, "write output file") {
		t.Fatalf("error_message=%q, want write output context", outputFailure.ErrorMessage)
	}
}

func TestRunFailFastStopsAfterFirstFailure(t *testing.T) {
	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bad":
			http.Error(w, "broken", http.StatusInternalServerError)
		case "/ok":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(sampleArticle("OK", "good content")))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(contentServer.Close)

	openAIServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output_text":"译文内容"}`)
	}))
	t.Cleanup(openAIServer.Close)

	tmpDir := useTempWorkingDir(t)
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", openAIServer.URL)

	badURL := contentServer.URL + "/bad"
	okURL := contentServer.URL + "/ok"

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := Run([]string{"--fail-fast", badURL, okURL}, &stdout, &stderr)
	if err == nil {
		t.Fatalf("Run() error = nil, want fail-fast error")
	}

	summaryPath := filepath.Join(tmpDir, "out", "_summary.json")
	rawSummary, err := os.ReadFile(summaryPath)
	if err != nil {
		t.Fatalf("read summary file: %v", err)
	}

	var summary taskSummary
	if err := json.Unmarshal(rawSummary, &summary); err != nil {
		t.Fatalf("unmarshal summary JSON: %v", err)
	}

	if len(summary.Results) != 1 {
		t.Fatalf("summary result len=%d, want 1 due to fail-fast stop", len(summary.Results))
	}
	if summary.Results[0].SourceURL != badURL {
		t.Fatalf("summary first source_url=%q, want %q", summary.Results[0].SourceURL, badURL)
	}
	if !strings.Contains(stderr.String(), "Fail-fast enabled: stop after first failure.") {
		t.Fatalf("stderr missing fail-fast message: %s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "Done: 0 succeeded, 1 failed") {
		t.Fatalf("stdout missing final summary: %s", stdout.String())
	}
}

func TestRunMaxRetriesFlagControlsOpenAIRetry(t *testing.T) {
	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/retry" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(sampleArticle("Retry", "retry content")))
	}))
	t.Cleanup(contentServer.Close)

	var callCount int32
	openAIServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}

		n := atomic.AddInt32(&callCount, 1)
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"temporary upstream failure"}}`)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output_text":"译文内容"}`)
	}))
	t.Cleanup(openAIServer.Close)

	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", openAIServer.URL)
	sourceURL := contentServer.URL + "/retry"

	dirNoRetry := t.TempDir()
	runInWorkingDir(t, dirNoRetry, func() string {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		atomic.StoreInt32(&callCount, 0)

		err := Run([]string{"--chunk-size", "10000", "--max-retries", "0", sourceURL}, &stdout, &stderr)
		if err == nil {
			t.Fatalf("Run() error = nil, want failure when retries are disabled")
		}
		if got := atomic.LoadInt32(&callCount); got != 1 {
			t.Fatalf("OpenAI call count=%d, want 1 with --max-retries=0", got)
		}
		return ""
	})

	dirWithRetry := t.TempDir()
	runInWorkingDir(t, dirWithRetry, func() string {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		atomic.StoreInt32(&callCount, 0)

		if err := Run([]string{"--chunk-size", "10000", "--max-retries", "1", sourceURL}, &stdout, &stderr); err != nil {
			t.Fatalf("Run() error = %v; stderr=%s", err, stderr.String())
		}
		if got := atomic.LoadInt32(&callCount); got != 2 {
			t.Fatalf("OpenAI call count=%d, want 2 with --max-retries=1", got)
		}
		return ""
	})
}

func TestRunWorkersFlagChangesConcurrency(t *testing.T) {
	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/parallel" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(sampleLongArticle("Parallel")))
	}))
	t.Cleanup(contentServer.Close)

	var inFlight int32
	var maxInFlight int32
	openAIServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}

		current := atomic.AddInt32(&inFlight, 1)
		for {
			prev := atomic.LoadInt32(&maxInFlight)
			if current <= prev {
				break
			}
			if atomic.CompareAndSwapInt32(&maxInFlight, prev, current) {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
		atomic.AddInt32(&inFlight, -1)

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output_text":"译文内容"}`)
	}))
	t.Cleanup(openAIServer.Close)

	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", openAIServer.URL)
	sourceURL := contentServer.URL + "/parallel"

	singleWorkerDir := t.TempDir()
	runInWorkingDir(t, singleWorkerDir, func() string {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		atomic.StoreInt32(&inFlight, 0)
		atomic.StoreInt32(&maxInFlight, 0)

		if err := Run([]string{"--chunk-size", "80", "--workers", "1", sourceURL}, &stdout, &stderr); err != nil {
			t.Fatalf("Run() with --workers=1 error = %v; stderr=%s", err, stderr.String())
		}
		if got := atomic.LoadInt32(&maxInFlight); got != 1 {
			t.Fatalf("max in-flight=%d, want 1 when workers=1", got)
		}
		return ""
	})

	multiWorkerDir := t.TempDir()
	runInWorkingDir(t, multiWorkerDir, func() string {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		atomic.StoreInt32(&inFlight, 0)
		atomic.StoreInt32(&maxInFlight, 0)

		if err := Run([]string{"--chunk-size", "80", "--workers", "4", sourceURL}, &stdout, &stderr); err != nil {
			t.Fatalf("Run() with --workers=4 error = %v; stderr=%s", err, stderr.String())
		}
		if got := atomic.LoadInt32(&maxInFlight); got <= 1 {
			t.Fatalf("max in-flight=%d, want >1 when workers=4", got)
		}
		return ""
	})
}

func TestParseFlagsRejectsInvalidWorkers(t *testing.T) {
	_, err := parseFlags([]string{"--workers", "0", "https://example.com"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "--workers must be greater than 0") {
		t.Fatalf("parseFlags error=%v, want workers validation error", err)
	}
}

func TestParseFlagsRejectsInvalidMaxRetries(t *testing.T) {
	_, err := parseFlags([]string{"--max-retries", "-1", "https://example.com"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "--max-retries must be 0 or greater") {
		t.Fatalf("parseFlags error=%v, want max-retries validation error", err)
	}
}

func TestValidateTranslatedChunk(t *testing.T) {
	cases := []struct {
		name       string
		source     string
		translated string
		wantErr    string
	}{
		{
			name:       "empty",
			source:     "# Title\n\ncontent",
			translated: "   ",
			wantErr:    "empty translated markdown",
		},
		{
			name:       "unbalanced fence",
			source:     "```go\nfmt.Println(1)\n```",
			translated: "```go\nfmt.Println(1)\n",
			wantErr:    "unbalanced fenced code block",
		},
		{
			name:       "missing heading",
			source:     "# Heading\n\ntext",
			translated: "普通段落",
			wantErr:    "missing headings",
		},
		{
			name:       "valid",
			source:     "# Heading\n\n- one\n- two\n\n```go\nfmt.Println(1)\n```",
			translated: "# 标题\n\n- 一\n- 二\n\n```go\nfmt.Println(1)\n```",
			wantErr:    "",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := validateTranslatedChunk(tc.source, tc.translated)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateTranslatedChunk() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validateTranslatedChunk() error = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestRunSummaryTracksQualityFallbackCount(t *testing.T) {
	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/needs-retry":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(sampleArticle("Retry", "retry content")))
		case "/normal":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(sampleArticle("Normal", "normal content")))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(contentServer.Close)

	var callCount int32
	openAIServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}

		n := atomic.AddInt32(&callCount, 1)
		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1:
			// Invalid markdown to trigger quality retry.
			_, _ = io.WriteString(w, "{\"output_text\":\"```go\\nfmt.Println(1)\\n\"}")
		default:
			_, _ = io.WriteString(w, "{\"output_text\":\"# 标题\\n\\n正常内容\"}")
		}
	}))
	t.Cleanup(openAIServer.Close)

	tmpDir := useTempWorkingDir(t)
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", openAIServer.URL)

	url1 := contentServer.URL + "/needs-retry"
	url2 := contentServer.URL + "/normal"

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := Run([]string{"--chunk-size", "10000", url1, url2}, &stdout, &stderr); err != nil {
		t.Fatalf("Run() error = %v; stderr=%s", err, stderr.String())
	}

	summaryPath := filepath.Join(tmpDir, "out", "_summary.json")
	rawSummary, err := os.ReadFile(summaryPath)
	if err != nil {
		t.Fatalf("read summary file: %v", err)
	}

	var summary taskSummary
	if err := json.Unmarshal(rawSummary, &summary); err != nil {
		t.Fatalf("unmarshal summary JSON: %v", err)
	}

	if summary.QualityFallbackTotalCount != 1 {
		t.Fatalf("quality_fallback_total_count=%d, want 1", summary.QualityFallbackTotalCount)
	}

	if len(summary.Results) != 2 {
		t.Fatalf("summary results len=%d, want 2", len(summary.Results))
	}
	if summary.Results[0].QualityFallbackCount != 1 {
		t.Fatalf("result[0] quality_fallback_count=%d, want 1", summary.Results[0].QualityFallbackCount)
	}
	if summary.Results[1].QualityFallbackCount != 0 {
		t.Fatalf("result[1] quality_fallback_count=%d, want 0", summary.Results[1].QualityFallbackCount)
	}
}

func TestRunSummaryTracksQualityFallbackCountOnFailure(t *testing.T) {
	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/always-bad":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(sampleArticle("Bad", "bad content")))
		case "/ok":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(sampleArticle("OK", "ok content")))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(contentServer.Close)

	var callCount int32
	openAIServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}

		n := atomic.AddInt32(&callCount, 1)
		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1, 2:
			// Invalid on initial + strict retry, forcing a quality validation failure.
			_, _ = io.WriteString(w, "{\"output_text\":\"```go\\nfmt.Println(1)\\n\"}")
		default:
			_, _ = io.WriteString(w, "{\"output_text\":\"# 标题\\n\\n正常内容\"}")
		}
	}))
	t.Cleanup(openAIServer.Close)

	tmpDir := useTempWorkingDir(t)
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", openAIServer.URL)

	badURL := contentServer.URL + "/always-bad"
	okURL := contentServer.URL + "/ok"

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := Run([]string{"--chunk-size", "10000", badURL, okURL}, &stdout, &stderr)
	if err == nil {
		t.Fatalf("Run() error = nil, want partial-failure error")
	}

	summaryPath := filepath.Join(tmpDir, "out", "_summary.json")
	rawSummary, err := os.ReadFile(summaryPath)
	if err != nil {
		t.Fatalf("read summary file: %v", err)
	}

	var summary taskSummary
	if err := json.Unmarshal(rawSummary, &summary); err != nil {
		t.Fatalf("unmarshal summary JSON: %v", err)
	}

	if summary.QualityFallbackTotalCount != 1 {
		t.Fatalf("quality_fallback_total_count=%d, want 1", summary.QualityFallbackTotalCount)
	}
	if len(summary.Results) != 2 {
		t.Fatalf("summary results len=%d, want 2", len(summary.Results))
	}
	if summary.Results[0].Success {
		t.Fatalf("result[0] should fail")
	}
	if summary.Results[0].QualityFallbackCount != 1 {
		t.Fatalf("result[0] quality_fallback_count=%d, want 1", summary.Results[0].QualityFallbackCount)
	}
	if summary.Results[0].ErrorType != errorTypeTranslate {
		t.Fatalf("result[0] error_type=%q, want %q", summary.Results[0].ErrorType, errorTypeTranslate)
	}
}

func TestRunSummaryIncludesUsageAndCostEstimate(t *testing.T) {
	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/a":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(sampleArticle("A", "content a")))
		case "/b":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(sampleArticle("B", "content b")))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(contentServer.Close)

	var callCount int32
	openAIServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}

		n := atomic.AddInt32(&callCount, 1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_, _ = io.WriteString(w, `{"output_text":"# 标题\n\n内容 A","usage":{"input_tokens":1000,"output_tokens":200,"total_tokens":1200}}`)
			return
		}
		_, _ = io.WriteString(w, `{"output_text":"# 标题\n\n内容 B","usage":{"input_tokens":3000,"output_tokens":400,"total_tokens":3400}}`)
	}))
	t.Cleanup(openAIServer.Close)

	tmpDir := useTempWorkingDir(t)
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", openAIServer.URL)

	priceConfigPath := filepath.Join(tmpDir, "price.json")
	if err := os.WriteFile(priceConfigPath, []byte(`{"gpt-5.2":{"input_per_million":1.0,"output_per_million":2.0}}`), 0o644); err != nil {
		t.Fatalf("write price config: %v", err)
	}

	url1 := contentServer.URL + "/a"
	url2 := contentServer.URL + "/b"

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := Run([]string{"--chunk-size", "10000", "--price-config", priceConfigPath, url1, url2}, &stdout, &stderr); err != nil {
		t.Fatalf("Run() error = %v; stderr=%s", err, stderr.String())
	}

	summaryPath := filepath.Join(tmpDir, "out", "_summary.json")
	rawSummary, err := os.ReadFile(summaryPath)
	if err != nil {
		t.Fatalf("read summary file: %v", err)
	}

	var summary taskSummary
	if err := json.Unmarshal(rawSummary, &summary); err != nil {
		t.Fatalf("unmarshal summary JSON: %v", err)
	}

	if summary.InputTokens != 4000 || summary.OutputTokens != 600 || summary.TotalTokens != 4600 {
		t.Fatalf("unexpected token totals: input=%d output=%d total=%d", summary.InputTokens, summary.OutputTokens, summary.TotalTokens)
	}
	if summary.MissingUsageCount != 0 {
		t.Fatalf("missing_usage_count=%d, want 0", summary.MissingUsageCount)
	}
	if summary.CostEstimateModel != "gpt-5.2" {
		t.Fatalf("cost_estimate_model=%q, want gpt-5.2", summary.CostEstimateModel)
	}

	wantCost := 0.0052
	if math.Abs(summary.CostEstimate-wantCost) > 0.000001 {
		t.Fatalf("cost_estimate=%f, want %f", summary.CostEstimate, wantCost)
	}
	if summary.CostEstimatePartial {
		t.Fatalf("cost_estimate_partial=true, want false")
	}

	if len(summary.Results) != 2 {
		t.Fatalf("summary results len=%d, want 2", len(summary.Results))
	}
	if summary.Results[0].InputTokens != 1000 || summary.Results[0].OutputTokens != 200 || summary.Results[0].TotalTokens != 1200 {
		t.Fatalf("result[0] unexpected usage: %+v", summary.Results[0])
	}
	if summary.Results[1].InputTokens != 3000 || summary.Results[1].OutputTokens != 400 || summary.Results[1].TotalTokens != 3400 {
		t.Fatalf("result[1] unexpected usage: %+v", summary.Results[1])
	}

	if !strings.Contains(stdout.String(), "Usage: input=4000 output=600 total=4600 tokens") {
		t.Fatalf("stdout missing usage summary: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "Estimated cost (gpt-5.2): $0.005200") {
		t.Fatalf("stdout missing cost estimate: %s", stdout.String())
	}
}

func TestRunHandlesMissingUsageGracefully(t *testing.T) {
	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/missing-usage" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(sampleArticle("MissingUsage", "content")))
	}))
	t.Cleanup(contentServer.Close)

	openAIServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output_text":"# 标题\n\n内容"}`)
	}))
	t.Cleanup(openAIServer.Close)

	tmpDir := useTempWorkingDir(t)
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", openAIServer.URL)

	priceConfigPath := filepath.Join(tmpDir, "price.json")
	if err := os.WriteFile(priceConfigPath, []byte(`{"gpt-5.2":{"input_per_million":1.0,"output_per_million":2.0}}`), 0o644); err != nil {
		t.Fatalf("write price config: %v", err)
	}

	sourceURL := contentServer.URL + "/missing-usage"

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := Run([]string{"--chunk-size", "10000", "--price-config", priceConfigPath, sourceURL, sourceURL}, &stdout, &stderr); err != nil {
		t.Fatalf("Run() error = %v; stderr=%s", err, stderr.String())
	}

	summaryPath := filepath.Join(tmpDir, "out", "_summary.json")
	rawSummary, err := os.ReadFile(summaryPath)
	if err != nil {
		t.Fatalf("read summary file: %v", err)
	}

	var summary taskSummary
	if err := json.Unmarshal(rawSummary, &summary); err != nil {
		t.Fatalf("unmarshal summary JSON: %v", err)
	}

	if summary.MissingUsageCount != 2 {
		t.Fatalf("missing_usage_count=%d, want 2", summary.MissingUsageCount)
	}
	if summary.CostEstimateModel != "gpt-5.2" {
		t.Fatalf("cost_estimate_model=%q, want gpt-5.2", summary.CostEstimateModel)
	}
	if !summary.CostEstimatePartial {
		t.Fatalf("cost_estimate_partial=false, want true")
	}
	if !strings.Contains(stderr.String(), "Usage info missing for 2 chunk(s); totals may be partial.") {
		t.Fatalf("stderr missing usage warning: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "Cost estimate is partial due to missing usage data.") {
		t.Fatalf("stderr missing partial cost warning: %s", stderr.String())
	}
}

func TestRunResumeReusesSavedChunksAndMatchesSingleRun(t *testing.T) {
	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/long" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(sampleLongArticle("Long Post")))
	}))
	t.Cleanup(contentServer.Close)

	var phase int32 = 1
	var phase1Calls int32
	var phase2Calls int32
	var phase3Calls int32

	openAIServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}

		body, _ := io.ReadAll(r.Body)
		switch atomic.LoadInt32(&phase) {
		case 1:
			if atomic.AddInt32(&phase1Calls, 1) == 3 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"message":"forced interruption"}}`)
				return
			}
		case 2:
			atomic.AddInt32(&phase2Calls, 1)
		case 3:
			atomic.AddInt32(&phase3Calls, 1)
		}

		sum := sha1.Sum(body)
		translated := "译文-" + hex.EncodeToString(sum[:8])
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output_text":"`+translated+`"}`)
	}))
	t.Cleanup(openAIServer.Close)

	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", openAIServer.URL)

	sourceURL := contentServer.URL + "/long"

	resumeDir := t.TempDir()
	resumedContent := runInWorkingDir(t, resumeDir, func() string {
		var stdout bytes.Buffer
		var stderr bytes.Buffer

		firstErr := Run([]string{"--chunk-size", "120", sourceURL}, &stdout, &stderr)
		if firstErr == nil {
			t.Fatalf("first Run() error = nil, want interruption failure")
		}

		statePath := filepath.Join(resumeDir, "out", stateFileName)
		stateData, err := os.ReadFile(statePath)
		if err != nil {
			t.Fatalf("read state file: %v", err)
		}

		var state resumeState
		if err := json.Unmarshal(stateData, &state); err != nil {
			t.Fatalf("unmarshal state file: %v", err)
		}

		entry := state.URLs[sourceURL]
		if entry == nil {
			t.Fatalf("state missing URL entry for %s", sourceURL)
		}
		if entry.ChunkCount < 3 {
			t.Fatalf("chunk_count=%d, want at least 3 for resume test", entry.ChunkCount)
		}
		savedChunks := len(entry.Chunks)
		if savedChunks == 0 || savedChunks >= entry.ChunkCount {
			t.Fatalf("saved chunk count=%d, want in (0,%d)", savedChunks, entry.ChunkCount)
		}

		atomic.StoreInt32(&phase, 2)
		stdout.Reset()
		stderr.Reset()

		if err := Run([]string{"--chunk-size", "120", sourceURL}, &stdout, &stderr); err != nil {
			t.Fatalf("second Run() error = %v; stderr=%s", err, stderr.String())
		}

		if got, want := int(atomic.LoadInt32(&phase2Calls)), entry.ChunkCount-savedChunks; got != want {
			t.Fatalf("resume API calls=%d, want %d (chunk_count=%d saved=%d)", got, want, entry.ChunkCount, savedChunks)
		}

		filename, err := filenameFromURL(sourceURL, false)
		if err != nil {
			t.Fatalf("filenameFromURL: %v", err)
		}
		outputPath := filepath.Join(resumeDir, "out", filename)
		outputData, err := os.ReadFile(outputPath)
		if err != nil {
			t.Fatalf("read resumed output: %v", err)
		}

		if _, err := os.Stat(statePath); !os.IsNotExist(err) {
			t.Fatalf("state file should be removed after completion, stat err=%v", err)
		}
		return string(outputData)
	})

	atomic.StoreInt32(&phase, 3)

	baselineDir := t.TempDir()
	baselineContent := runInWorkingDir(t, baselineDir, func() string {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		if err := Run([]string{"--chunk-size", "120", sourceURL}, &stdout, &stderr); err != nil {
			t.Fatalf("baseline Run() error = %v; stderr=%s", err, stderr.String())
		}

		filename, err := filenameFromURL(sourceURL, false)
		if err != nil {
			t.Fatalf("filenameFromURL: %v", err)
		}
		outputPath := filepath.Join(baselineDir, "out", filename)
		outputData, err := os.ReadFile(outputPath)
		if err != nil {
			t.Fatalf("read baseline output: %v", err)
		}
		return string(outputData)
	})

	if resumedContent != baselineContent {
		t.Fatalf("resumed output differs from one-shot output")
	}
	if atomic.LoadInt32(&phase3Calls) == 0 {
		t.Fatalf("expected baseline run to call OpenAI at least once")
	}
}

func useTempWorkingDir(t *testing.T) string {
	t.Helper()

	tmpDir := t.TempDir()
	originalDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("Chdir(%q): %v", tmpDir, err)
	}

	t.Cleanup(func() {
		_ = os.Chdir(originalDir)
	})

	return tmpDir
}

func runInWorkingDir(t *testing.T, dir string, fn func() string) string {
	t.Helper()

	originalDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir(%q): %v", dir, err)
	}
	defer func() {
		_ = os.Chdir(originalDir)
	}()

	return fn()
}

func sampleArticle(title string, text string) string {
	return "<!doctype html><html><head><title>" + title + "</title></head><body><article><h1>" + title + "</h1><p>" + text + " paragraph with enough length for readability extraction.</p></article></body></html>"
}

func sampleLongArticle(title string) string {
	paragraphs := []string{
		"This is the first long paragraph to force markdown chunking and resume behavior verification for the CLI test suite.",
		"This is the second long paragraph with extra descriptive words so the chunk splitter has enough material to cut into multiple segments.",
		"This is the third long paragraph that continues the sequence and helps us validate deterministic output across resumed and single-pass runs.",
		"This is the fourth long paragraph to ensure there are more chunks than one worker request, making partial completion observable.",
	}

	var b strings.Builder
	b.WriteString("<!doctype html><html><head><title>")
	b.WriteString(title)
	b.WriteString("</title></head><body><article><h1>")
	b.WriteString(title)
	b.WriteString("</h1>")
	for _, p := range paragraphs {
		b.WriteString("<p>")
		b.WriteString(p)
		b.WriteString("</p>")
	}
	b.WriteString("</article></body></html>")
	return b.String()
}
