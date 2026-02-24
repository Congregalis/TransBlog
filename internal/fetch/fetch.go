package fetch

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	readability "github.com/go-shiori/go-readability"
	"golang.org/x/net/html"
)

const maxErrBody = 1024

type Document struct {
	HTML     string
	Title    string
	FinalURL string
}

func HTML(ctx context.Context, httpClient *http.Client, rawURL string) (Document, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return Document{}, fmt.Errorf("build request: %w", err)
	}

	req.Header.Set("User-Agent", "TransBlog/1.0 (+https://github.com/openai)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	resp, err := httpClient.Do(req)
	if err != nil {
		return Document{}, fmt.Errorf("download URL: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Document{}, fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		errSnippet := strings.TrimSpace(string(body))
		if len(errSnippet) > maxErrBody {
			errSnippet = errSnippet[:maxErrBody] + "..."
		}
		return Document{}, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, errSnippet)
	}

	finalURL := rawURL
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}

	rawHTML := string(body)
	doc := Document{
		HTML:     rawHTML,
		Title:    normalizeTitle(extractTitle(rawHTML)),
		FinalURL: finalURL,
	}

	if !isHTMLContentType(resp.Header.Get("Content-Type")) {
		return doc, nil
	}

	parsedURL, err := url.Parse(finalURL)
	if err != nil {
		return doc, nil
	}

	article, err := readability.FromReader(bytes.NewReader(body), parsedURL)
	if err != nil {
		return doc, nil
	}

	if content := strings.TrimSpace(article.Content); content != "" {
		doc.HTML = content
	}
	if title := strings.TrimSpace(article.Title); title != "" {
		doc.Title = normalizeTitle(title)
	}

	return doc, nil
}

func isHTMLContentType(contentType string) bool {
	if strings.TrimSpace(contentType) == "" {
		return true
	}

	lower := strings.ToLower(contentType)
	return strings.Contains(lower, "text/html") || strings.Contains(lower, "application/xhtml+xml")
}

func extractTitle(rawHTML string) string {
	doc, err := html.Parse(strings.NewReader(rawHTML))
	if err != nil {
		return ""
	}

	var walk func(*html.Node) string
	walk = func(node *html.Node) string {
		if node.Type == html.ElementNode && node.Data == "title" {
			return strings.TrimSpace(extractNodeText(node))
		}

		for child := node.FirstChild; child != nil; child = child.NextSibling {
			if title := walk(child); title != "" {
				return title
			}
		}
		return ""
	}

	return walk(doc)
}

func extractNodeText(node *html.Node) string {
	var b strings.Builder

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}

	walk(node)
	return b.String()
}

func normalizeTitle(title string) string {
	return strings.Join(strings.Fields(title), " ")
}
