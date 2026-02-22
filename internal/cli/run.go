package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"transblog/internal/chunk"
	"transblog/internal/fetch"
	"transblog/internal/glossary"
	"transblog/internal/markdown"
	"transblog/internal/openai"
)

const defaultChunkSize = 2000

type options struct {
	Model      string
	OutPath    string
	View       bool
	Glossary   string
	Timeout    time.Duration
	ChunkSize  int
	ShowHelp   bool
	SourceURLs []string
}

func Run(args []string, stdout io.Writer, stderr io.Writer) error {
	opts, err := parseFlags(args, stderr)
	if err != nil {
		return err
	}
	if opts.ShowHelp {
		return nil
	}

	apiKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	if apiKey == "" {
		return errors.New("OPENAI_API_KEY is required")
	}

	baseURL := strings.TrimSpace(os.Getenv("OPENAI_BASE_URL"))
	httpClient := &http.Client{Timeout: opts.Timeout}

	glossaryMap, err := glossary.Load(opts.Glossary)
	if err != nil {
		return err
	}

	openAIClient := openai.NewClient(apiKey, baseURL, httpClient)

	// Collect source/translation pairs for view mode
	var pairs []pair

	ctx := context.Background()
	for _, rawURL := range opts.SourceURLs {
		sourceURL := strings.TrimSpace(rawURL)
		if sourceURL == "" {
			continue
		}

		markdownDoc, err := fetchAndConvert(ctx, httpClient, sourceURL)
		if err != nil {
			return err
		}

		chunks := chunk.SplitMarkdown(markdownDoc, opts.ChunkSize)
		if len(chunks) == 0 {
			continue
		}

		for idx, mdChunk := range chunks {
			translated, err := openAIClient.TranslateMarkdownChunk(ctx, opts.Model, mdChunk, glossaryMap)
			if err != nil {
				return fmt.Errorf("translate chunk %d for %s: %w", idx+1, sourceURL, err)
			}
			translated = glossary.Apply(translated, glossaryMap)

			pairs = append(pairs, pair{sourceURL: sourceURL, source: mdChunk, translated: translated})
		}
	}

	var output strings.Builder
	if opts.View {
		writeHTMLView(&output, pairs, opts)
	} else {
		writeMarkdown(&output, pairs, opts)
	}

	result := output.String()
	if opts.OutPath == "" {
		// Default: save to ./out/ directory
		outDir := "out"
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			return fmt.Errorf("create output directory: %w", err)
		}

		// Generate filename from first URL
		u := opts.SourceURLs[0]
		parsed, err := url.Parse(u)
		if err != nil || parsed.Host == "" {
			return fmt.Errorf("invalid URL: %s", u)
		}
		filename := strings.ReplaceAll(parsed.Host+parsed.Path, "/", "_")
		ext := ".md"
		if opts.View {
			ext = ".html"
		}
		filename = strings.TrimSuffix(filename, "_") + ext
		if filename == ext {
			filename = "output" + ext
		}

		outPath := filepath.Join(outDir, filename)
		if err := os.WriteFile(outPath, []byte(result), 0o644); err != nil {
			return fmt.Errorf("write output file %s: %w", outPath, err)
		}
		_, err = fmt.Fprintf(stdout, "Output: %s\n", outPath)
		return err
	}

	if err := os.MkdirAll(filepath.Dir(opts.OutPath), 0o755); err != nil && filepath.Dir(opts.OutPath) != "." {
		return fmt.Errorf("create output directory: %w", err)
	}
	if err := os.WriteFile(opts.OutPath, []byte(result), 0o644); err != nil {
		return fmt.Errorf("write output file %s: %w", opts.OutPath, err)
	}
	return nil
}

func parseFlags(args []string, stderr io.Writer) (options, error) {
	fs := flag.NewFlagSet("transblog", flag.ContinueOnError)
	fs.SetOutput(stderr)

	opts := options{}
	fs.StringVar(&opts.Model, "model", "gpt-5.2", "OpenAI model name")
	fs.StringVar(&opts.OutPath, "out", "", "Output markdown file (default: ./out/)")
	fs.BoolVar(&opts.View, "view", false, "Generate HTML split-view with Markdown rendering and synchronized scrolling")
	fs.StringVar(&opts.Glossary, "glossary", "", "Path to glossary JSON map, e.g. {\"term\":\"translation\"}")
	fs.DurationVar(&opts.Timeout, "timeout", 90*time.Second, "HTTP timeout, e.g. 120s")
	fs.IntVar(&opts.ChunkSize, "chunk-size", defaultChunkSize, "Target chunk size in characters")

	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: transblog [flags] <url> [url...]")
		fmt.Fprintln(stderr, "")
		fmt.Fprintln(stderr, "Example:")
		fmt.Fprintln(stderr, "  transblog https://boristane.com/blog/how-i-use-claude-code/")
		fmt.Fprintln(stderr, "")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			opts.ShowHelp = true
			return opts, nil
		}
		return options{}, err
	}
	if opts.Timeout <= 0 {
		return options{}, errors.New("--timeout must be positive")
	}
	if opts.ChunkSize <= 0 {
		opts.ChunkSize = defaultChunkSize
	}

	opts.SourceURLs = fs.Args()
	if len(opts.SourceURLs) == 0 {
		fs.Usage()
		return options{}, errors.New("at least one URL is required")
	}

	return opts, nil
}

func fetchAndConvert(ctx context.Context, httpClient *http.Client, sourceURL string) (string, error) {
	html, err := fetch.HTML(ctx, httpClient, sourceURL)
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", sourceURL, err)
	}

	md, err := markdown.FromHTML(html)
	if err != nil {
		return "", fmt.Errorf("convert %s HTML to markdown: %w", sourceURL, err)
	}

	return md, nil
}

func writeFrontMatter(w io.StringWriter, opts options) {
	now := time.Now().UTC().Format(time.RFC3339)
	_, _ = w.WriteString("---\n")
	_, _ = w.WriteString("source_urls:\n")

	for _, sourceURL := range opts.SourceURLs {
		_, _ = w.WriteString("  - ")
		_, _ = w.WriteString(sourceURL)
		_, _ = w.WriteString("\n")
	}

	_, _ = w.WriteString("generated_at: ")
	_, _ = w.WriteString(now)
	_, _ = w.WriteString("\n")
	_, _ = w.WriteString("model: ")
	_, _ = w.WriteString(opts.Model)
	_, _ = w.WriteString("\n")
	_, _ = w.WriteString("---\n\n")
}

type pair struct {
	sourceURL  string
	source     string
	translated string
}

func writeMarkdown(w io.StringWriter, pairs []pair, opts options) {
	writeFrontMatter(w, opts)

	for i, p := range pairs {
		if i > 0 {
			_, _ = w.WriteString("\n\n")
		}
		_, _ = w.WriteString("## Source: ")
		_, _ = w.WriteString(p.sourceURL)
		_, _ = w.WriteString("\n\n")
		_, _ = w.WriteString(p.source)
		_, _ = w.WriteString("\n\n---\n\n")
		_, _ = w.WriteString(p.translated)
	}
}

func writeHTMLView(w io.StringWriter, pairs []pair, opts options) {
	type viewPair struct {
		SourceURL  string `json:"sourceURL"`
		Source     string `json:"source"`
		Translated string `json:"translated"`
	}

	viewPairs := make([]viewPair, 0, len(pairs))
	for _, p := range pairs {
		viewPairs = append(viewPairs, viewPair{
			SourceURL:  p.sourceURL,
			Source:     p.source,
			Translated: p.translated,
		})
	}

	payload, err := json.Marshal(viewPairs)
	if err != nil {
		payload = []byte("[]")
	}
	payloadText := strings.ReplaceAll(string(payload), "</", "<\\/")

	now := time.Now().UTC().Format(time.RFC3339)
	_, _ = w.WriteString(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Bilingual View</title>
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/highlight.js@11.11.1/styles/github-dark.min.css">
<style>
* { box-sizing: border-box; margin: 0; padding: 0; }
:root { color-scheme: light; }
body {
  font-family: "Iowan Old Style", "Palatino Linotype", "Book Antiqua", Palatino, serif;
  line-height: 1.6;
  color: #1f2937;
  background: radial-gradient(circle at 20% 0%, #f8fafc 0%, #eef2ff 44%, #e2e8f0 100%);
}
.topbar {
  background: #0f172a;
  color: #f8fafc;
  padding: 14px 18px 12px;
  border-bottom: 1px solid rgba(255, 255, 255, 0.08);
}
.topbar h1 {
  font-size: 18px;
  font-weight: 700;
  letter-spacing: 0.2px;
  font-family: "Avenir Next", "Segoe UI", sans-serif;
}
.topbar .meta {
  margin-top: 4px;
  font-size: 12px;
  opacity: 0.78;
  font-family: "Avenir Next", "Segoe UI", sans-serif;
}
.toolbar {
  margin-top: 10px;
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
}
.toolbar-item {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  font-size: 12px;
  padding: 6px 10px;
  border-radius: 999px;
  border: 1px solid rgba(255, 255, 255, 0.15);
  background: rgba(255, 255, 255, 0.06);
  font-family: "Avenir Next", "Segoe UI", sans-serif;
}
.toolbar-item select {
  border: 1px solid rgba(255, 255, 255, 0.28);
  background: #1e293b;
  color: #f8fafc;
  border-radius: 999px;
  padding: 2px 8px;
  font-size: 12px;
  font-family: "Avenir Next", "Segoe UI", sans-serif;
}
.toolbar-item input[type="checkbox"] {
  accent-color: #38bdf8;
}
.progress-chip {
  margin-left: auto;
}
.layout {
  height: calc(100vh - 108px);
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 12px;
  padding: 12px;
}
.pane {
  display: flex;
  flex-direction: column;
  min-height: 0;
  border: 1px solid #dbe3ef;
  border-radius: 12px;
  overflow: hidden;
  background: #ffffff;
  box-shadow: 0 12px 36px rgba(15, 23, 42, 0.1);
}
.pane-head {
  padding: 10px 12px;
  border-bottom: 1px solid #e5e7eb;
  background: linear-gradient(180deg, #ffffff 0%, #f8fafc 100%);
  font-size: 12px;
  font-weight: 700;
  letter-spacing: 0.5px;
  text-transform: uppercase;
  color: #475569;
  font-family: "Avenir Next", "Segoe UI", sans-serif;
}
.pane-scroll {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  padding: 18px;
  overscroll-behavior: contain;
}
.chunk {
  margin-bottom: 28px;
  padding-bottom: 22px;
  border-bottom: 1px dashed #dbe3ef;
}
.chunk:last-child { margin-bottom: 0; }
.chunk-meta {
  margin-bottom: 8px;
  font-size: 11px;
  color: #64748b;
  word-break: break-all;
  font-family: "Avenir Next", "Segoe UI", sans-serif;
}
.markdown-body h1, .markdown-body h2, .markdown-body h3,
.markdown-body h4, .markdown-body h5, .markdown-body h6 {
  margin: 22px 0 10px;
  line-height: 1.3;
}
.markdown-body h1:first-child, .markdown-body h2:first-child, .markdown-body h3:first-child,
.markdown-body h4:first-child, .markdown-body h5:first-child, .markdown-body h6:first-child {
  margin-top: 0;
}
.markdown-body p, .markdown-body ul, .markdown-body ol, .markdown-body pre, .markdown-body blockquote {
  margin: 0 0 13px;
}
.markdown-body ul, .markdown-body ol { padding-left: 20px; }
.markdown-body li + li { margin-top: 4px; }
.markdown-body code {
  background: #eff6ff;
  padding: 2px 5px;
  border-radius: 4px;
  font-size: 0.9em;
}
.markdown-body pre {
  background: #0b1120;
  color: #dbeafe;
  padding: 12px;
  border-radius: 8px;
  overflow: auto;
}
.markdown-body pre code {
  background: transparent;
  padding: 0;
}
.markdown-body blockquote {
  border-left: 3px solid #93c5fd;
  padding-left: 12px;
  color: #475569;
}
.markdown-body a { color: #0f4ea8; text-underline-offset: 2px; }
.markdown-body img { max-width: 100%; height: auto; }
.markdown-body table {
  border-collapse: collapse;
  margin: 0 0 13px;
  width: 100%;
}
.markdown-body th, .markdown-body td {
  border: 1px solid #dbe3ef;
  padding: 6px 8px;
}
.empty-state {
  color: #64748b;
  font-family: "Avenir Next", "Segoe UI", sans-serif;
  font-size: 14px;
}
@media (max-width: 980px) {
  .topbar {
    padding: 12px 14px 10px;
  }
  .toolbar {
    gap: 8px;
  }
  .progress-chip {
    margin-left: 0;
  }
  .layout {
    grid-template-columns: 1fr;
    height: auto;
    min-height: calc(100vh - 96px);
    padding: 10px;
    gap: 10px;
  }
  .pane { min-height: 44vh; }
}
</style>
</head>
<body>
`)

	_, _ = w.WriteString("<header class=\"topbar\">\n")
	_, _ = w.WriteString("<h1>Bilingual View</h1>\n")
	_, _ = w.WriteString("<div class=\"meta\">Generated: ")
	_, _ = w.WriteString(now)
	_, _ = w.WriteString(" | Model: ")
	_, _ = w.WriteString(opts.Model)
	_, _ = w.WriteString(" | Chunks: ")
	_, _ = w.WriteString(fmt.Sprintf("%d", len(pairs)))
	_, _ = w.WriteString("</div>\n")
	_, _ = w.WriteString("<div class=\"toolbar\">\n")
	_, _ = w.WriteString("  <label class=\"toolbar-item\">")
	_, _ = w.WriteString("<input id=\"sync-lock\" type=\"checkbox\" checked> 同步滚动</label>\n")
	_, _ = w.WriteString("  <label class=\"toolbar-item\">Chunk")
	_, _ = w.WriteString("<select id=\"chunk-jump\"></select></label>\n")
	_, _ = w.WriteString("  <div class=\"toolbar-item progress-chip\" id=\"reading-progress\">Chunk 0 / 0</div>\n")
	_, _ = w.WriteString("</div>\n")
	_, _ = w.WriteString("</header>\n")

	_, _ = w.WriteString("<main class=\"layout\">\n")
	_, _ = w.WriteString("  <section class=\"pane\">\n")
	_, _ = w.WriteString("    <div class=\"pane-head\">English</div>\n")
	_, _ = w.WriteString("    <div id=\"pane-en\" class=\"pane-scroll\"></div>\n")
	_, _ = w.WriteString("  </section>\n")
	_, _ = w.WriteString("  <section class=\"pane\">\n")
	_, _ = w.WriteString("    <div class=\"pane-head\">中文</div>\n")
	_, _ = w.WriteString("    <div id=\"pane-zh\" class=\"pane-scroll\"></div>\n")
	_, _ = w.WriteString("  </section>\n")
	_, _ = w.WriteString("</main>\n")

	_, _ = w.WriteString("<script id=\"pairs-data\" type=\"application/json\">")
	_, _ = w.WriteString(payloadText)
	_, _ = w.WriteString("</script>\n")
	_, _ = w.WriteString("<script src=\"https://cdn.jsdelivr.net/npm/marked/marked.min.js\"></script>\n")
	_, _ = w.WriteString("<script src=\"https://cdn.jsdelivr.net/npm/dompurify@3.2.6/dist/purify.min.js\"></script>\n")
	_, _ = w.WriteString("<script src=\"https://cdn.jsdelivr.net/npm/highlight.js@11.11.1/lib/highlight.min.js\"></script>\n")
	_, _ = w.WriteString(`<script>
(function () {
  const pairs = JSON.parse(document.getElementById("pairs-data").textContent || "[]");

  const paneEN = document.getElementById("pane-en");
  const paneZH = document.getElementById("pane-zh");
  const syncLock = document.getElementById("sync-lock");
  const chunkJump = document.getElementById("chunk-jump");
  const progressEl = document.getElementById("reading-progress");

  const panes = {
    en: { el: paneEN },
    zh: { el: paneZH },
  };
  const chunkNodes = {
    en: [],
    zh: [],
  };
  let metrics = {
    en: { tops: [], spans: [] },
    zh: { tops: [], spans: [] },
  };
  const suppressUntilByPane = {
    en: 0,
    zh: 0,
  };

  let activePaneId = "en";
  let syncScheduled = false;
  let resizeRAF = 0;

  if (window.marked && typeof window.marked.setOptions === "function") {
    window.marked.setOptions({
      gfm: true,
      breaks: false,
      headerIds: false,
      mangle: false,
    });
  }

  function nowMS() {
    return window.performance && typeof window.performance.now === "function"
      ? window.performance.now()
      : Date.now();
  }

  function markSuppressed(paneId, durationMS) {
    suppressUntilByPane[paneId] = nowMS() + durationMS;
  }

  function isSuppressed(paneId) {
    return nowMS() < suppressUntilByPane[paneId];
  }

  function escapeHTML(text) {
    return String(text)
      .replaceAll("&", "&amp;")
      .replaceAll("<", "&lt;")
      .replaceAll(">", "&gt;")
      .replaceAll('"', "&quot;")
      .replaceAll("'", "&#39;");
  }

  function renderMarkdown(md) {
    const raw = md || "";
    const hasMarked = window.marked && typeof window.marked.parse === "function";
    const hasPurify = window.DOMPurify && typeof window.DOMPurify.sanitize === "function";

    if (!hasMarked || !hasPurify) {
      return "<pre>" + escapeHTML(raw) + "</pre>";
    }

    const html = window.marked.parse(raw);
    return window.DOMPurify.sanitize(html, {
      USE_PROFILES: { html: true },
    });
  }

  function applyHighlighting(root) {
    if (!window.hljs || !root) {
      return;
    }
    root.querySelectorAll("pre code").forEach(function (block) {
      window.hljs.highlightElement(block);
    });
  }

  function compactURL(urlText) {
    try {
      const url = new URL(urlText);
      const path = url.pathname === "/" ? "" : url.pathname;
      return url.hostname + path;
    } catch (_) {
      return urlText;
    }
  }

  function renderPane(paneId, key) {
    const pane = panes[paneId].el;
    pane.innerHTML = "";

    if (pairs.length === 0) {
      const empty = document.createElement("p");
      empty.className = "empty-state";
      empty.textContent = "No translated chunks.";
      pane.appendChild(empty);
      chunkNodes[paneId] = [];
      return;
    }

    pairs.forEach(function (item, index) {
      const chunk = document.createElement("article");
      chunk.className = "chunk";
      chunk.setAttribute("data-index", String(index));

      const meta = document.createElement("div");
      meta.className = "chunk-meta";
      meta.textContent = compactURL(item.sourceURL) + " | chunk " + (index + 1);

      const body = document.createElement("div");
      body.className = "markdown-body";
      body.innerHTML = renderMarkdown(item[key]);
      applyHighlighting(body);

      chunk.appendChild(meta);
      chunk.appendChild(body);
      pane.appendChild(chunk);
    });

    chunkNodes[paneId] = Array.from(pane.querySelectorAll(".chunk"));
  }

  renderPane("en", "source");
  renderPane("zh", "translated");

  function updateChunkJumpOptions() {
    chunkJump.innerHTML = "";
    const total = pairs.length;
    if (total === 0) {
      const option = document.createElement("option");
      option.value = "0";
      option.textContent = "No chunks";
      chunkJump.appendChild(option);
      chunkJump.disabled = true;
      return;
    }

    chunkJump.disabled = false;
    pairs.forEach(function (item, index) {
      const option = document.createElement("option");
      option.value = String(index);
      option.textContent = String(index + 1) + " - " + compactURL(item.sourceURL);
      chunkJump.appendChild(option);
    });
  }

  updateChunkJumpOptions();

  function buildMetrics(paneId) {
    const pane = panes[paneId].el;
    const chunks = chunkNodes[paneId];
    const tops = chunks.map(function (chunk) {
      return chunk.offsetTop;
    });
    const spans = chunks.map(function (_, index) {
      const start = tops[index];
      const end = index + 1 < tops.length ? tops[index + 1] : pane.scrollHeight;
      return Math.max(end - start, 1);
    });
    return { tops: tops, spans: spans };
  }

  function refreshMetrics() {
    metrics.en = buildMetrics("en");
    metrics.zh = buildMetrics("zh");
  }

  refreshMetrics();

  function findChunkIndex(tops, scrollTop) {
    if (tops.length === 0) {
      return 0;
    }

    let lo = 0;
    let hi = tops.length - 1;
    let ans = 0;

    while (lo <= hi) {
      const mid = (lo + hi) >> 1;
      if (tops[mid] <= scrollTop + 1) {
        ans = mid;
        lo = mid + 1;
      } else {
        hi = mid - 1;
      }
    }

    return ans;
  }

  function getChunkState(paneId) {
    const pane = panes[paneId].el;
    const metric = metrics[paneId];
    if (!metric || metric.tops.length === 0) {
      return { index: 0, progress: 0 };
    }

    const index = findChunkIndex(metric.tops, pane.scrollTop);
    const start = metric.tops[index];
    const span = metric.spans[index] || 1;
    const progress = Math.max(0, Math.min(1, (pane.scrollTop - start) / span));
    return { index: index, progress: progress };
  }

  function stateToTop(paneId, state) {
    const pane = panes[paneId].el;
    const metric = metrics[paneId];
    if (!metric || metric.tops.length === 0) {
      return 0;
    }

    const safeIndex = Math.max(0, Math.min(state.index, metric.tops.length - 1));
    const start = metric.tops[safeIndex];
    const span = metric.spans[safeIndex] || 1;
    const top = start + span * state.progress;
    const maxTop = Math.max(pane.scrollHeight - pane.clientHeight, 0);
    return Math.max(0, Math.min(top, maxTop));
  }

  function updateProgress(paneId, stateOverride) {
    const total = pairs.length;
    if (total === 0) {
      progressEl.textContent = "Chunk 0 / 0";
      return;
    }

    const state = stateOverride || getChunkState(paneId);
    const safeIndex = Math.max(0, Math.min(state.index, total - 1));
    const pct = Math.round(Math.max(0, Math.min(state.progress, 1)) * 100);

    progressEl.textContent = "Chunk " + (safeIndex + 1) + " / " + total + " · " + pct + "%";
    if (!chunkJump.disabled) {
      chunkJump.value = String(safeIndex);
    }
  }

  function syncTargetFromSource(sourcePaneId) {
    refreshMetrics();

    const targetPaneId = sourcePaneId === "en" ? "zh" : "en";
    const sourcePane = panes[sourcePaneId].el;
    const targetPane = panes[targetPaneId].el;

    if (!sourcePane || !targetPane) {
      return;
    }

    const state = getChunkState(sourcePaneId);
    updateProgress(sourcePaneId, state);

    if (!syncLock.checked) {
      return;
    }

    const targetTop = stateToTop(targetPaneId, state);
    if (Math.abs(targetPane.scrollTop - targetTop) <= 0.5) {
      return;
    }

    markSuppressed(targetPaneId, 72);
    targetPane.scrollTop = targetTop;
  }

  function scheduleSyncFrom(paneId) {
    activePaneId = paneId;
    if (syncScheduled) {
      return;
    }

    syncScheduled = true;
    window.requestAnimationFrame(function () {
      syncScheduled = false;
      syncTargetFromSource(activePaneId);
    });
  }

  function goToChunk(index) {
    if (pairs.length === 0) {
      return;
    }

    refreshMetrics();

    const safeIndex = Math.max(0, Math.min(index, pairs.length - 1));
    const state = { index: safeIndex, progress: 0 };

    markSuppressed("en", 90);
    markSuppressed("zh", 90);
    paneEN.scrollTop = stateToTop("en", state);
    paneZH.scrollTop = stateToTop("zh", state);

    activePaneId = "en";
    updateProgress("en", state);
  }

  chunkJump.addEventListener("change", function () {
    goToChunk(Number(chunkJump.value || "0"));
  });

  syncLock.addEventListener("change", function () {
    scheduleSyncFrom(activePaneId);
  });

  function bindPaneEvents(paneId) {
    const pane = panes[paneId].el;

    ["wheel", "touchstart", "pointerdown", "mouseenter"].forEach(function (eventName) {
      pane.addEventListener(eventName, function () {
        activePaneId = paneId;
      }, { passive: true });
    });

    pane.addEventListener("scroll", function () {
      if (isSuppressed(paneId)) {
        return;
      }
      scheduleSyncFrom(paneId);
    }, { passive: true });
  }

  bindPaneEvents("en");
  bindPaneEvents("zh");

  window.addEventListener("resize", function () {
    if (resizeRAF) {
      window.cancelAnimationFrame(resizeRAF);
    }
    resizeRAF = window.requestAnimationFrame(function () {
      resizeRAF = 0;
      scheduleSyncFrom(activePaneId);
    });
  });

  updateProgress("en");
})();
</script>
`)
	_, _ = w.WriteString("</body>\n</html>\n")
}
