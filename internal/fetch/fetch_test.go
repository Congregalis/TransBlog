package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTMLExtractsReadableContentAndMetadata(t *testing.T) {
	t.Parallel()

	const noisyHTML = `<!doctype html>
<html>
<head>
  <title>Readable Title</title>
</head>
<body>
  <header>Top navigation should be removed from article content.</header>
  <nav>Top navigation should be removed from article content.</nav>
  <article>
    <h1>Readable Title</h1>
    <p>This is the first important paragraph with enough descriptive text to look like a real article for readability extraction.</p>
    <p>This is the second important paragraph with additional details so the extractor can identify the main body content.</p>
  </article>
  <footer>Footer links should not be part of the extracted article.</footer>
</body>
</html>`

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			http.Redirect(w, r, "/final", http.StatusFound)
		case "/final":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(noisyHTML))
		default:
			http.NotFound(w, r)
		}
	})

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	httpClient := server.Client()
	httpClient.Timeout = 5 * time.Second

	doc, err := HTML(context.Background(), httpClient, server.URL+"/start")
	if err != nil {
		t.Fatalf("HTML returned error: %v", err)
	}

	if got, want := doc.FinalURL, server.URL+"/final"; got != want {
		t.Fatalf("doc.FinalURL = %q, want %q", got, want)
	}
	if got := doc.Title; got != "Readable Title" {
		t.Fatalf("doc.Title = %q, want %q", got, "Readable Title")
	}
	if !strings.Contains(doc.HTML, "first important paragraph") {
		t.Fatalf("expected extracted content to include article body, got %q", doc.HTML)
	}
	if strings.Contains(doc.HTML, "Top navigation should be removed") {
		t.Fatalf("expected extracted content to drop navigation noise, got %q", doc.HTML)
	}
}

func TestHTMLFallsBackToOriginalBodyWhenExtractionFails(t *testing.T) {
	t.Parallel()

	const plainText = "not-html plain text response"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(plainText))
	}))
	t.Cleanup(server.Close)

	httpClient := server.Client()
	httpClient.Timeout = 5 * time.Second

	doc, err := HTML(context.Background(), httpClient, server.URL)
	if err != nil {
		t.Fatalf("HTML returned error: %v", err)
	}

	if doc.HTML != plainText {
		t.Fatalf("doc.HTML = %q, want original response body %q", doc.HTML, plainText)
	}
	if got, want := doc.FinalURL, server.URL; got != want {
		t.Fatalf("doc.FinalURL = %q, want %q", got, want)
	}
}

func TestHTMLReturnsStatusErrorWithBodySnippet(t *testing.T) {
	t.Parallel()

	const errBody = "missing page"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(errBody))
	}))
	t.Cleanup(server.Close)

	httpClient := server.Client()
	httpClient.Timeout = 5 * time.Second

	_, err := HTML(context.Background(), httpClient, server.URL)
	if err == nil {
		t.Fatalf("HTML() error = nil, want status error")
	}
	if !strings.Contains(err.Error(), "unexpected status 404") {
		t.Fatalf("HTML() error = %v, want status details", err)
	}
	if !strings.Contains(err.Error(), errBody) {
		t.Fatalf("HTML() error = %v, want body snippet %q", err, errBody)
	}
}

func TestHTMLReturnsTimeoutError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><head><title>slow</title></head><body>slow</body></html>"))
	}))
	t.Cleanup(server.Close)

	httpClient := server.Client()
	httpClient.Timeout = 50 * time.Millisecond

	_, err := HTML(context.Background(), httpClient, server.URL)
	if err == nil {
		t.Fatalf("HTML() error = nil, want timeout")
	}
	if !strings.Contains(err.Error(), "download URL") {
		t.Fatalf("HTML() error = %v, want wrapped download error", err)
	}
}

func TestExtractTitle(t *testing.T) {
	t.Parallel()

	got := extractTitle(`<html><head><title>  Hello World  </title></head><body></body></html>`)
	if got != "Hello World" {
		t.Fatalf("extractTitle() = %q, want %q", got, "Hello World")
	}
}
