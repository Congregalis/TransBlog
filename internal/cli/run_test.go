package cli

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
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

	"github.com/joho/godotenv"
	versionpkg "transblog/internal/version"
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

	useTempWorkingDir(t)
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", openAIServer.URL)

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	err := Run([]string{contentServer.URL + "/post-a", contentServer.URL + "/post-b"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("Run() error = %v; stderr=%s", err, stderr.String())
	}

	summaryPath := summaryPathFromStdout(t, stdout.String())
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
	if summary.BatchID == "" || summary.BatchDate == "" {
		t.Fatalf("summary missing batch metadata: %+v", summary)
	}
	if len(summary.Labels) == 0 {
		t.Fatalf("summary labels empty")
	}
	if len(summary.RetryableFailedURLs) != 0 {
		t.Fatalf("retry_failed_urls=%v, want empty", summary.RetryableFailedURLs)
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

	entries, err := os.ReadDir(filepath.Dir(summaryPath))
	if err != nil {
		t.Fatalf("ReadDir(batch dir): %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("batch dir file count = %d, want 3", len(entries))
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

	useTempWorkingDir(t)
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

	summaryPath := summaryPathFromStdout(t, stdout.String())
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
	if len(summary.RetryableFailedURLs) != 1 || summary.RetryableFailedURLs[0] != contentServer.URL+"/bad" {
		t.Fatalf("retry_failed_urls=%v, want [%s]", summary.RetryableFailedURLs, contentServer.URL+"/bad")
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

	useTempWorkingDir(t)
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

	summaryPath := summaryPathFromStdout(t, stdout.String())
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
	errType, errMessage, _, _, _, _, _ := errorDetails(
		newProcessError(errorTypeOutput, errors.New("write output file /tmp/out.md: permission denied")),
	)
	if errType != errorTypeOutput {
		t.Fatalf("error_type = %q, want %q", errType, errorTypeOutput)
	}
	if !strings.Contains(errMessage, "write output file") {
		t.Fatalf("error_message=%q, want write output context", errMessage)
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

	useTempWorkingDir(t)
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

	summaryPath := summaryPathFromStdout(t, stdout.String())
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

func TestParseFlagsAllowsVersionWithoutURL(t *testing.T) {
	opts, err := parseFlags([]string{"--version"}, io.Discard)
	if err != nil {
		t.Fatalf("parseFlags() error = %v, want nil", err)
	}
	if !opts.ShowVersion {
		t.Fatalf("ShowVersion=false, want true")
	}
	if len(opts.SourceURLs) != 0 {
		t.Fatalf("SourceURLs len=%d, want 0", len(opts.SourceURLs))
	}
}

func TestParseFlagsAllowsNoURLForOnboarding(t *testing.T) {
	opts, err := parseFlags([]string{}, io.Discard)
	if err != nil {
		t.Fatalf("parseFlags() error = %v, want nil", err)
	}
	if len(opts.SourceURLs) != 0 {
		t.Fatalf("SourceURLs len=%d, want 0", len(opts.SourceURLs))
	}
}

func TestParseFlagsAcceptsRefresh(t *testing.T) {
	opts, err := parseFlags([]string{"--refresh", "https://example.com/post"}, io.Discard)
	if err != nil {
		t.Fatalf("parseFlags() error = %v, want nil", err)
	}
	if !opts.Refresh {
		t.Fatalf("Refresh=false, want true")
	}
}

func TestParseFlagsRejectsInitFlag(t *testing.T) {
	_, err := parseFlags([]string{"--init"}, io.Discard)
	if err == nil {
		t.Fatalf("parseFlags() error = nil, want unknown flag error")
	}
	if !strings.Contains(err.Error(), "flag provided but not defined: -init") {
		t.Fatalf("parseFlags() error = %v, want unknown init flag message", err)
	}
}

func TestRunVersionSkipsAPIKeyRequirement(t *testing.T) {
	oldVersion := versionpkg.Version
	oldCommit := versionpkg.Commit
	oldBuildTime := versionpkg.BuildTime
	versionpkg.Version = "v1.2.3"
	versionpkg.Commit = "abc1234"
	versionpkg.BuildTime = "2026-02-24T20:30:00Z"
	t.Cleanup(func() {
		versionpkg.Version = oldVersion
		versionpkg.Commit = oldCommit
		versionpkg.BuildTime = oldBuildTime
	})

	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENAI_BASE_URL", "")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := Run([]string{"--version"}, &stdout, &stderr); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	output := strings.TrimSpace(stdout.String())
	want := "transblog version=v1.2.3 commit=abc1234 build_time=2026-02-24T20:30:00Z"
	if output != want {
		t.Fatalf("stdout = %q, want %q", output, want)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestRunOnboardingCreatesEnvAndCollectsURL(t *testing.T) {
	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/post" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(sampleArticle("Onboard", "onboard content")))
	}))
	t.Cleanup(contentServer.Close)

	openAIServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output_text":"# title\n\ntranslated"}`)
	}))
	t.Cleanup(openAIServer.Close)

	tmpDir := useTempWorkingDir(t)
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", openAIServer.URL)

	originalInput := cliInput
	cliInput = strings.NewReader("\n" + contentServer.URL + "/post\n")
	t.Cleanup(func() {
		cliInput = originalInput
	})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := Run([]string{}, &stdout, &stderr); err != nil {
		t.Fatalf("Run() error = %v; stderr=%s", err, stderr.String())
	}

	envPath := filepath.Join(tmpDir, defaultEnvFileName)
	if _, err := os.Stat(envPath); err != nil {
		t.Fatalf("expected env file at %s: %v", envPath, err)
	}

	outputPaths := outputPathsFromStdout(stdout.String())
	if len(outputPaths) != 2 {
		t.Fatalf("output paths len=%d, want 2; stdout=%s", len(outputPaths), stdout.String())
	}
	for _, outputPath := range outputPaths {
		if _, err := os.Stat(outputPath); err != nil {
			t.Fatalf("expected output at %s: %v", outputPath, err)
		}
	}
	if !strings.Contains(stdout.String(), "Output mode: Markdown + HTML") {
		t.Fatalf("stdout missing onboarding output mode message: %s", stdout.String())
	}
}

func TestRunOnboardingPromptsForAPIKeyAndURL(t *testing.T) {
	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/post" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(sampleArticle("Prompt", "prompt content")))
	}))
	t.Cleanup(contentServer.Close)

	var authHeader atomic.Value
	openAIServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		authHeader.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output_text":"# title\n\ntranslated"}`)
	}))
	t.Cleanup(openAIServer.Close)

	useTempWorkingDir(t)
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENAI_BASE_URL", openAIServer.URL)

	originalInput := cliInput
	cliInput = strings.NewReader("sk-onboard\n\n" + contentServer.URL + "/post\n")
	t.Cleanup(func() {
		cliInput = originalInput
	})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := Run([]string{}, &stdout, &stderr); err != nil {
		t.Fatalf("Run() error = %v; stderr=%s", err, stderr.String())
	}

	if got := authHeader.Load(); got != "Bearer sk-onboard" {
		t.Fatalf("authorization header = %v, want Bearer sk-onboard", got)
	}
	if !strings.Contains(stderr.String(), "OPENAI_API_KEY is not set.") {
		t.Fatalf("stderr missing onboarding key prompt: %s", stderr.String())
	}

	envPath := filepath.Join(".", defaultEnvFileName)
	envValues, err := godotenv.Read(envPath)
	if err != nil {
		t.Fatalf("read env file: %v", err)
	}
	if got := envValues["OPENAI_API_KEY"]; got != "sk-onboard" {
		t.Fatalf("OPENAI_API_KEY in .env = %q, want sk-onboard", got)
	}
}

func TestRunOnboardingAllowsEditingConfig(t *testing.T) {
	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/post" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(sampleArticle("Edit", "edit content")))
	}))
	t.Cleanup(contentServer.Close)

	var requestedModel atomic.Value
	openAIServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}

		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &payload)
		requestedModel.Store(payload.Model)

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output_text":"# title\n\ntranslated"}`)
	}))
	t.Cleanup(openAIServer.Close)

	useTempWorkingDir(t)
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", openAIServer.URL)

	originalInput := cliInput
	cliInput = strings.NewReader(strings.Join([]string{
		"y",
		"3",
		"gpt-edited",
		"4",
		"2",
		"5",
		"edited-out",
		"",
		contentServer.URL + "/post",
	}, "\n"))
	t.Cleanup(func() {
		cliInput = originalInput
	})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := Run([]string{}, &stdout, &stderr); err != nil {
		t.Fatalf("Run() error = %v; stderr=%s", err, stderr.String())
	}

	if got := requestedModel.Load(); got != "gpt-edited" {
		t.Fatalf("requested model = %v, want gpt-edited", got)
	}

	outputPath := firstOutputPathFromStdout(t, stdout.String())
	if !strings.Contains(outputPath, filepath.Join("edited-out")) {
		t.Fatalf("output path = %s, want under edited-out", outputPath)
	}
	if _, err := os.Stat(outputPath); err != nil {
		t.Fatalf("expected markdown output in edited-out: %v", err)
	}
}

func TestMaskSecret(t *testing.T) {
	if got := maskSecret(""); got != "(empty)" {
		t.Fatalf("maskSecret(empty) = %q, want (empty)", got)
	}
	if got := maskSecret("abcd"); got != "****" {
		t.Fatalf("maskSecret(4 chars) = %q, want ****", got)
	}
	if got := maskSecret("sk-123456"); got != "*****3456" {
		t.Fatalf("maskSecret(long) = %q, want *****3456", got)
	}
}

func TestBuildBatchIDUsesReadableTimeAndSlug(t *testing.T) {
	now := time.Date(2026, 2, 26, 0, 29, 9, 0, time.UTC)
	id := buildBatchID(now, []string{"https://example.com/blog/writing-about-agentic-engineering-patterns"})
	if id != "00-29-09_writing-about-agentic-engineering-patterns" {
		t.Fatalf("batch id = %q, want readable time+slug", id)
	}
}

func TestBuildBatchIDForMultipleURLsAddsCountHint(t *testing.T) {
	now := time.Date(2026, 2, 26, 9, 8, 7, 0, time.UTC)
	id := buildBatchID(now, []string{
		"https://example.com/a/first-post",
		"https://example.com/b/second-post",
		"https://example.com/c/third-post",
	})
	if id != "09-08-07_first-post-and-2-more" {
		t.Fatalf("batch id = %q, want first-post-and-2-more style", id)
	}
}

func TestRunAppliesEnvDefaults(t *testing.T) {
	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/post" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(sampleArticle("Config Post", "config content")))
	}))
	t.Cleanup(contentServer.Close)

	var requestedModel atomic.Value
	openAIServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}

		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &payload)
		requestedModel.Store(payload.Model)

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output_text":"# title\n\ntranslated"}`)
	}))
	t.Cleanup(openAIServer.Close)

	tmpDir := useTempWorkingDir(t)
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", openAIServer.URL)

	envPath := filepath.Join(tmpDir, defaultEnvFileName)
	envContent := "TRANSBLOG_MODEL=gpt-config\nTRANSBLOG_WORKERS=1\nTRANSBLOG_OUT_DIR=custom-out\n"
	if err := os.WriteFile(envPath, []byte(envContent), 0o644); err != nil {
		t.Fatalf("write env file: %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	sourceURL := contentServer.URL + "/post"
	if err := Run([]string{sourceURL}, &stdout, &stderr); err != nil {
		t.Fatalf("Run() error = %v; stderr=%s", err, stderr.String())
	}

	if got := requestedModel.Load(); got != "gpt-config" {
		t.Fatalf("requested model = %v, want gpt-config", got)
	}

	outputPath := firstOutputPathFromStdout(t, stdout.String())
	if !strings.Contains(outputPath, filepath.Join("custom-out")) {
		t.Fatalf("output path = %s, want under custom-out", outputPath)
	}
	if _, err := os.Stat(outputPath); err != nil {
		t.Fatalf("expected output at %s: %v", outputPath, err)
	}
}

func TestRunFlagOverridesEnvDefaults(t *testing.T) {
	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/post" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(sampleArticle("Override Post", "override content")))
	}))
	t.Cleanup(contentServer.Close)

	var requestedModel atomic.Value
	openAIServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}

		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &payload)
		requestedModel.Store(payload.Model)

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output_text":"# title\n\ntranslated"}`)
	}))
	t.Cleanup(openAIServer.Close)

	tmpDir := useTempWorkingDir(t)
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", openAIServer.URL)

	envPath := filepath.Join(tmpDir, defaultEnvFileName)
	envContent := "TRANSBLOG_MODEL=gpt-config\nTRANSBLOG_WORKERS=1\nTRANSBLOG_OUT_DIR=custom-out\n"
	if err := os.WriteFile(envPath, []byte(envContent), 0o644); err != nil {
		t.Fatalf("write env file: %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	sourceURL := contentServer.URL + "/post"
	if err := Run([]string{"--model", "gpt-flag", sourceURL}, &stdout, &stderr); err != nil {
		t.Fatalf("Run() error = %v; stderr=%s", err, stderr.String())
	}

	if got := requestedModel.Load(); got != "gpt-flag" {
		t.Fatalf("requested model = %v, want gpt-flag", got)
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

	useTempWorkingDir(t)
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", openAIServer.URL)

	url1 := contentServer.URL + "/needs-retry"
	url2 := contentServer.URL + "/normal"

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := Run([]string{"--chunk-size", "10000", url1, url2}, &stdout, &stderr); err != nil {
		t.Fatalf("Run() error = %v; stderr=%s", err, stderr.String())
	}

	summaryPath := summaryPathFromStdout(t, stdout.String())
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

	useTempWorkingDir(t)
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

	summaryPath := summaryPathFromStdout(t, stdout.String())
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

	summaryPath := summaryPathFromStdout(t, stdout.String())
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

	summaryPath := summaryPathFromStdout(t, stdout.String())
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

func TestRunCacheSkipsFetchAndTranslateWithoutRefresh(t *testing.T) {
	var fetchCalls int32
	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fetchCalls, 1)
		if r.URL.Path != "/cache-me" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(sampleArticle("Cache Me", "cache content body")))
	}))
	t.Cleanup(contentServer.Close)

	var openAICalls int32
	openAIServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&openAICalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output_text":"# 标题\n\n缓存译文","usage":{"input_tokens":100,"output_tokens":50,"total_tokens":150}}`)
	}))
	t.Cleanup(openAIServer.Close)

	tmpDir := useTempWorkingDir(t)
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", openAIServer.URL)

	sourceURL := contentServer.URL + "/cache-me"

	var stdout1 bytes.Buffer
	var stderr1 bytes.Buffer
	if err := Run([]string{"--chunk-size", "10000", sourceURL}, &stdout1, &stderr1); err != nil {
		t.Fatalf("first Run() error = %v; stderr=%s", err, stderr1.String())
	}

	firstFetchCalls := atomic.LoadInt32(&fetchCalls)
	firstOpenAICalls := atomic.LoadInt32(&openAICalls)
	if firstFetchCalls == 0 || firstOpenAICalls == 0 {
		t.Fatalf("expected first run to call fetch/openai, got fetch=%d openai=%d", firstFetchCalls, firstOpenAICalls)
	}

	var stdout2 bytes.Buffer
	var stderr2 bytes.Buffer
	if err := Run([]string{"--chunk-size", "10000", sourceURL}, &stdout2, &stderr2); err != nil {
		t.Fatalf("second Run() error = %v; stderr=%s", err, stderr2.String())
	}

	if got := atomic.LoadInt32(&fetchCalls); got != firstFetchCalls {
		t.Fatalf("fetch calls after cache run = %d, want %d", got, firstFetchCalls)
	}
	if got := atomic.LoadInt32(&openAICalls); got != firstOpenAICalls {
		t.Fatalf("openai calls after cache run = %d, want %d", got, firstOpenAICalls)
	}
	if !strings.Contains(stderr2.String(), "Cache hit: reused") {
		t.Fatalf("stderr missing cache hit message: %s", stderr2.String())
	}

	cachePath := filepath.Join(tmpDir, "out", cacheFileName)
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("expected cache file at %s: %v", cachePath, err)
	}
}

func TestRunRefreshBypassesCache(t *testing.T) {
	var fetchCalls int32
	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fetchCalls, 1)
		if r.URL.Path != "/refresh-me" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(sampleArticle("Refresh Me", "refresh content body")))
	}))
	t.Cleanup(contentServer.Close)

	var openAICalls int32
	openAIServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&openAICalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output_text":"# 标题\n\n刷新译文","usage":{"input_tokens":120,"output_tokens":60,"total_tokens":180}}`)
	}))
	t.Cleanup(openAIServer.Close)

	useTempWorkingDir(t)
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", openAIServer.URL)

	sourceURL := contentServer.URL + "/refresh-me"

	var stdout1 bytes.Buffer
	var stderr1 bytes.Buffer
	if err := Run([]string{"--chunk-size", "10000", sourceURL}, &stdout1, &stderr1); err != nil {
		t.Fatalf("first Run() error = %v; stderr=%s", err, stderr1.String())
	}

	firstFetchCalls := atomic.LoadInt32(&fetchCalls)
	firstOpenAICalls := atomic.LoadInt32(&openAICalls)

	var stdout2 bytes.Buffer
	var stderr2 bytes.Buffer
	if err := Run([]string{"--refresh", "--chunk-size", "10000", sourceURL}, &stdout2, &stderr2); err != nil {
		t.Fatalf("refresh Run() error = %v; stderr=%s", err, stderr2.String())
	}

	if got := atomic.LoadInt32(&fetchCalls); got <= firstFetchCalls {
		t.Fatalf("fetch calls with --refresh = %d, want > %d", got, firstFetchCalls)
	}
	if got := atomic.LoadInt32(&openAICalls); got <= firstOpenAICalls {
		t.Fatalf("openai calls with --refresh = %d, want > %d", got, firstOpenAICalls)
	}
}

func TestRunMarkdownOutputIncludesEnhancedFrontMatter(t *testing.T) {
	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/meta" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(sampleArticle("Meta Post", "front matter content")))
	}))
	t.Cleanup(contentServer.Close)

	openAIServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output_text":"# 标题\n\n前置信息","usage":{"input_tokens":321,"output_tokens":123,"total_tokens":444}}`)
	}))
	t.Cleanup(openAIServer.Close)

	useTempWorkingDir(t)
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", openAIServer.URL)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	sourceURL := contentServer.URL + "/meta"
	if err := Run([]string{"--chunk-size", "10000", sourceURL}, &stdout, &stderr); err != nil {
		t.Fatalf("Run() error = %v; stderr=%s", err, stderr.String())
	}

	outputPath := firstOutputPathFromStdout(t, stdout.String())
	raw, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("read output file: %v", err)
	}
	text := string(raw)

	if !strings.Contains(text, "title: \"Meta Post\"") {
		t.Fatalf("front matter missing title: %s", text)
	}
	if !strings.Contains(text, "lang: \"zh-CN\"") {
		t.Fatalf("front matter missing lang: %s", text)
	}
	if !strings.Contains(text, "chunk_count: 1") {
		t.Fatalf("front matter missing chunk_count: %s", text)
	}
	if !strings.Contains(text, "duration: \"") {
		t.Fatalf("front matter missing duration: %s", text)
	}
	if !strings.Contains(text, "token_usage:") ||
		!strings.Contains(text, "input: 321") ||
		!strings.Contains(text, "output: 123") ||
		!strings.Contains(text, "total: 444") {
		t.Fatalf("front matter missing token_usage block: %s", text)
	}
}

func TestRunHTMLViewIncludesReaderEnhancements(t *testing.T) {
	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/html-tools" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(sampleArticle("HTML Tools", "html enhancements content")))
	}))
	t.Cleanup(contentServer.Close)

	openAIServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{\"output_text\":\"# 标题\\n\\n```go\\nfmt.Println(1)\\n```\"}")
	}))
	t.Cleanup(openAIServer.Close)

	useTempWorkingDir(t)
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", openAIServer.URL)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	sourceURL := contentServer.URL + "/html-tools"
	if err := Run([]string{"--view", "--chunk-size", "10000", sourceURL}, &stdout, &stderr); err != nil {
		t.Fatalf("Run() error = %v; stderr=%s", err, stderr.String())
	}

	outputPath := firstOutputPathFromStdout(t, stdout.String())
	htmlRaw, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("read html output: %v", err)
	}
	htmlText := string(htmlRaw)

	if strings.Contains(htmlText, `id="search-input"`) {
		t.Fatalf("html should not include search input in lightweight mode: %s", htmlText)
	}
	if !strings.Contains(htmlText, `id="font-size-select"`) {
		t.Fatalf("html missing font-size control: %s", htmlText)
	}
	if !strings.Contains(htmlText, "copy-code-btn") {
		t.Fatalf("html missing copy-code handler: %s", htmlText)
	}
	if !strings.Contains(htmlText, "heading-anchor") {
		t.Fatalf("html missing heading anchor support: %s", htmlText)
	}
	if !strings.Contains(htmlText, "syncFromLocationHash") {
		t.Fatalf("html missing hash deep-link sync: %s", htmlText)
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

		outputPath := firstOutputPathFromStdout(t, stdout.String())
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

		outputPath := firstOutputPathFromStdout(t, stdout.String())
		outputData, err := os.ReadFile(outputPath)
		if err != nil {
			t.Fatalf("read baseline output: %v", err)
		}
		return string(outputData)
	})

	if stripFrontMatter(resumedContent) != stripFrontMatter(baselineContent) {
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

func stripFrontMatter(markdownText string) string {
	if !strings.HasPrefix(markdownText, "---\n") {
		return markdownText
	}

	endMarker := "\n---\n\n"
	idx := strings.Index(markdownText, endMarker)
	if idx < 0 {
		return markdownText
	}
	return markdownText[idx+len(endMarker):]
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

func summaryPathFromStdout(t *testing.T, stdout string) string {
	t.Helper()

	for _, line := range strings.Split(stdout, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Summary: ") {
			path := strings.TrimSpace(strings.TrimPrefix(trimmed, "Summary: "))
			if path != "" {
				return path
			}
		}
	}
	t.Fatalf("stdout missing summary path: %s", stdout)
	return ""
}

func outputPathsFromStdout(stdout string) []string {
	paths := make([]string, 0, 2)
	for _, line := range strings.Split(stdout, "\n") {
		trimmed := strings.TrimSpace(line)
		if idx := strings.Index(trimmed, "Output: "); idx >= 0 {
			path := strings.TrimSpace(trimmed[idx+len("Output: "):])
			if path != "" {
				paths = append(paths, path)
			}
		}
		if idx := strings.Index(trimmed, "Output (HTML): "); idx >= 0 {
			path := strings.TrimSpace(trimmed[idx+len("Output (HTML): "):])
			if path != "" {
				paths = append(paths, path)
			}
		}
	}
	return paths
}

func firstOutputPathFromStdout(t *testing.T, stdout string) string {
	t.Helper()

	paths := outputPathsFromStdout(stdout)
	if len(paths) == 0 {
		t.Fatalf("stdout missing output path: %s", stdout)
	}
	return paths[0]
}
