package fetch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const maxErrBody = 1024

func HTML(ctx context.Context, httpClient *http.Client, rawURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}

	req.Header.Set("User-Agent", "TransBlog/1.0 (+https://github.com/openai)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("download URL: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		errSnippet := strings.TrimSpace(string(body))
		if len(errSnippet) > maxErrBody {
			errSnippet = errSnippet[:maxErrBody] + "..."
		}
		return "", fmt.Errorf("unexpected status %d: %s", resp.StatusCode, errSnippet)
	}

	return string(body), nil
}
