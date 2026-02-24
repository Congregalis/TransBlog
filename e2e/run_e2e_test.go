package e2e

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"transblog/internal/cli"
)

func TestE2ESingleURLSuccess(t *testing.T) {
	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/post" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(sampleArticle("Single", "single post body")))
	}))
	t.Cleanup(contentServer.Close)

	openAIServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output_text":"# Single\n\ntranslated content"}`)
	}))
	t.Cleanup(openAIServer.Close)

	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", openAIServer.URL)

	tmpDir := t.TempDir()
	runInWorkingDir(t, tmpDir, func() {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		sourceURL := contentServer.URL + "/post"
		if err := cli.Run([]string{"--chunk-size", "10000", sourceURL}, &stdout, &stderr); err != nil {
			t.Fatalf("Run() error = %v; stderr=%s", err, stderr.String())
		}

		matches, err := filepath.Glob(filepath.Join(tmpDir, "out", "*.md"))
		if err != nil {
			t.Fatalf("glob output file: %v", err)
		}
		if len(matches) != 1 {
			t.Fatalf("output files len=%d, want 1", len(matches))
		}

		content, err := os.ReadFile(matches[0])
		if err != nil {
			t.Fatalf("read output: %v", err)
		}
		text := string(content)
		if !strings.Contains(text, "## Source: Single ("+sourceURL+")") {
			t.Fatalf("output missing source metadata: %s", text)
		}
		if !strings.Contains(text, "translated content") {
			t.Fatalf("output missing translated text: %s", text)
		}
	})
}

func TestE2EMultiURLPartialFailure(t *testing.T) {
	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(sampleArticle("OK", "content ok")))
		case "/bad":
			http.NotFound(w, r)
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
		_, _ = io.WriteString(w, `{"output_text":"# Success\n\nnormal translated text"}`)
	}))
	t.Cleanup(openAIServer.Close)

	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", openAIServer.URL)

	tmpDir := t.TempDir()
	runInWorkingDir(t, tmpDir, func() {
		okURL := contentServer.URL + "/ok"
		badURL := contentServer.URL + "/bad"

		var stdout bytes.Buffer
		var stderr bytes.Buffer
		err := cli.Run([]string{"--chunk-size", "10000", okURL, badURL}, &stdout, &stderr)
		if err == nil {
			t.Fatalf("Run() error = nil, want partial-failure error")
		}

		summaryPath := filepath.Join(tmpDir, "out", "_summary.json")
		summaryData, readErr := os.ReadFile(summaryPath)
		if readErr != nil {
			t.Fatalf("read summary: %v", readErr)
		}

		var summary struct {
			SuccessCount int `json:"success_count"`
			FailureCount int `json:"failure_count"`
			Results      []struct {
				SourceURL string `json:"source_url"`
				Success   bool   `json:"success"`
				ErrorType string `json:"error_type"`
			} `json:"results"`
		}
		if err := json.Unmarshal(summaryData, &summary); err != nil {
			t.Fatalf("unmarshal summary: %v", err)
		}

		if summary.SuccessCount != 1 || summary.FailureCount != 1 {
			t.Fatalf("summary counts = (%d,%d), want (1,1)", summary.SuccessCount, summary.FailureCount)
		}
		if len(summary.Results) != 2 {
			t.Fatalf("summary results len=%d, want 2", len(summary.Results))
		}

		var sawFetchFailure bool
		for _, result := range summary.Results {
			if result.SourceURL == badURL {
				sawFetchFailure = true
				if result.Success {
					t.Fatalf("bad URL result marked success")
				}
				if result.ErrorType != "fetch_failed" {
					t.Fatalf("bad URL error_type=%q, want fetch_failed", result.ErrorType)
				}
			}
		}
		if !sawFetchFailure {
			t.Fatalf("summary missing bad URL result")
		}
	})
}

func TestE2EResumeAfterInterruptedRun(t *testing.T) {
	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/long" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(sampleLongArticle("Resume")))
	}))
	t.Cleanup(contentServer.Close)

	var phase int32 = 1
	var phase1Calls int32
	var phase2Calls int32
	openAIServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}

		body, _ := io.ReadAll(r.Body)
		if atomic.LoadInt32(&phase) == 1 {
			if atomic.AddInt32(&phase1Calls, 1) == 3 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"message":"forced interruption"}}`)
				return
			}
		} else {
			atomic.AddInt32(&phase2Calls, 1)
		}

		sum := sha1.Sum(body)
		translated := "# title\n\ntranslated-" + hex.EncodeToString(sum[:8])
		encoded, _ := json.Marshal(translated)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output_text":`+string(encoded)+`}`)
	}))
	t.Cleanup(openAIServer.Close)

	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", openAIServer.URL)

	tmpDir := t.TempDir()
	runInWorkingDir(t, tmpDir, func() {
		sourceURL := contentServer.URL + "/long"
		var stdout bytes.Buffer
		var stderr bytes.Buffer

		firstErr := cli.Run([]string{"--chunk-size", "60", sourceURL}, &stdout, &stderr)
		if firstErr == nil {
			t.Fatalf("first Run() error = nil, want interruption error")
		}

		statePath := filepath.Join(tmpDir, "out", ".transblog.state.json")
		stateData, err := os.ReadFile(statePath)
		if err != nil {
			t.Fatalf("read state file: %v", err)
		}

		var state struct {
			URLs map[string]struct {
				ChunkCount int `json:"chunk_count"`
				Chunks     map[string]struct {
					Source     string `json:"source"`
					Translated string `json:"translated"`
				} `json:"chunks"`
			} `json:"urls"`
		}
		if err := json.Unmarshal(stateData, &state); err != nil {
			t.Fatalf("unmarshal state file: %v", err)
		}
		entry, ok := state.URLs[sourceURL]
		if !ok {
			t.Fatalf("state missing URL entry for %s", sourceURL)
		}
		savedChunks := len(entry.Chunks)
		if savedChunks == 0 || savedChunks >= entry.ChunkCount {
			t.Fatalf("saved chunks=%d, chunk_count=%d; want partial save", savedChunks, entry.ChunkCount)
		}

		atomic.StoreInt32(&phase, 2)
		stdout.Reset()
		stderr.Reset()

		if err := cli.Run([]string{"--chunk-size", "60", sourceURL}, &stdout, &stderr); err != nil {
			t.Fatalf("second Run() error = %v; stderr=%s", err, stderr.String())
		}

		if got, want := int(atomic.LoadInt32(&phase2Calls)), entry.ChunkCount-savedChunks; got != want {
			t.Fatalf("resume calls=%d, want %d", got, want)
		}
		if _, err := os.Stat(statePath); !os.IsNotExist(err) {
			t.Fatalf("state file should be removed after completion, stat err=%v", err)
		}
	})
}

func runInWorkingDir(t *testing.T, dir string, fn func()) {
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

	fn()
}

func sampleArticle(title string, text string) string {
	return "<!doctype html><html><head><title>" + title + "</title></head><body><article><h1>" + title + "</h1><p>" + text + " paragraph with enough length for readability extraction.</p></article></body></html>"
}

func sampleLongArticle(title string) string {
	paragraphs := []string{
		"This is the first long paragraph to force markdown chunking and resume behavior verification for the integration test suite.",
		"This is the second long paragraph with extra descriptive words so the chunk splitter has enough material to cut into multiple segments.",
		"This is the third long paragraph that forces a controlled interruption in the first run.",
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
