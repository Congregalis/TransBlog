package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
			t.Fatalf("result %d success=false, error=%q", i, item.Error)
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
		if item.Error == "" {
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

func sampleArticle(title string, text string) string {
	return "<!doctype html><html><head><title>" + title + "</title></head><body><article><h1>" + title + "</h1><p>" + text + " paragraph with enough length for readability extraction.</p></article></body></html>"
}
