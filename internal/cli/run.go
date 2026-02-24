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
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"transblog/internal/chunk"
	"transblog/internal/fetch"
	"transblog/internal/glossary"
	"transblog/internal/markdown"
	"transblog/internal/openai"
	"transblog/internal/version"
)

const (
	defaultChunkSize        = 2000
	defaultTranslateWorkers = 4
	defaultMaxRetries       = 5
	defaultOutDir           = "out"
	summaryFileName         = "_summary.json"
	stateFileName           = ".transblog.state.json"

	errorTypeFetch     = "fetch_failed"
	errorTypeConvert   = "convert_failed"
	errorTypeTranslate = "translate_failed"
	errorTypeOutput    = "output_failed"
	errorTypeUnknown   = "unknown"

	defaultQualityRetries = 1
)

type options struct {
	Model       string
	OutPath     string
	View        bool
	ShowVersion bool
	Workers     int
	MaxRetries  int
	FailFast    bool
	PriceConfig string
	Glossary    string
	Timeout     time.Duration
	ChunkSize   int
	ShowHelp    bool
	SourceURLs  []string
}

type outputPlan struct {
	outputDir  string
	singleFile string
	summaryDir string
}

type summaryItem struct {
	SourceURL            string  `json:"source_url"`
	FinalURL             string  `json:"final_url,omitempty"`
	Success              bool    `json:"success"`
	DurationMS           int64   `json:"duration_ms"`
	OutputPath           string  `json:"output_path,omitempty"`
	QualityFallbackCount int     `json:"quality_fallback_count,omitempty"`
	InputTokens          int64   `json:"input_tokens,omitempty"`
	OutputTokens         int64   `json:"output_tokens,omitempty"`
	TotalTokens          int64   `json:"total_tokens,omitempty"`
	MissingUsageCount    int     `json:"missing_usage_count,omitempty"`
	CostEstimate         float64 `json:"cost_estimate,omitempty"`
	CostEstimateModel    string  `json:"cost_estimate_model,omitempty"`
	CostEstimatePartial  bool    `json:"cost_estimate_partial,omitempty"`
	ErrorType            string  `json:"error_type,omitempty"`
	ErrorMessage         string  `json:"error_message,omitempty"`
}

type taskSummary struct {
	GeneratedAt               string        `json:"generated_at"`
	Model                     string        `json:"model"`
	View                      bool          `json:"view"`
	TotalURLs                 int           `json:"total_urls"`
	SuccessCount              int           `json:"success_count"`
	FailureCount              int           `json:"failure_count"`
	TotalDurationMS           int64         `json:"total_duration_ms"`
	QualityFallbackTotalCount int           `json:"quality_fallback_total_count,omitempty"`
	InputTokens               int64         `json:"input_tokens,omitempty"`
	OutputTokens              int64         `json:"output_tokens,omitempty"`
	TotalTokens               int64         `json:"total_tokens,omitempty"`
	MissingUsageCount         int           `json:"missing_usage_count,omitempty"`
	CostEstimate              float64       `json:"cost_estimate,omitempty"`
	CostEstimateModel         string        `json:"cost_estimate_model,omitempty"`
	CostEstimatePartial       bool          `json:"cost_estimate_partial,omitempty"`
	Results                   []summaryItem `json:"results"`
}

type urlOutput struct {
	finalURL             string
	outputPath           string
	qualityFallbackCount int
	usage                usageStats
	costEstimate         float64
	costEstimated        bool
}

type usageStats struct {
	inputTokens       int64
	outputTokens      int64
	totalTokens       int64
	missingUsageCount int
}

type priceConfig map[string]modelPrice

type modelPrice struct {
	InputPerMillion  float64 `json:"input_per_million"`
	OutputPerMillion float64 `json:"output_per_million"`
	TotalPerMillion  float64 `json:"total_per_million"`
}

func (u *usageStats) add(other usageStats) {
	u.inputTokens += other.inputTokens
	u.outputTokens += other.outputTokens
	u.totalTokens += other.totalTokens
	u.missingUsageCount += other.missingUsageCount
}

func (u *usageStats) addOpenAIUsage(usage openai.Usage) {
	if usage.Available {
		u.inputTokens += usage.InputTokens
		u.outputTokens += usage.OutputTokens
		u.totalTokens += usage.TotalTokens
		return
	}
	u.missingUsageCount++
}

type resumeState struct {
	Version   int                        `json:"version"`
	UpdatedAt string                     `json:"updated_at"`
	URLs      map[string]*resumeURLState `json:"urls"`
}

type resumeURLState struct {
	FinalURL   string                      `json:"final_url,omitempty"`
	Title      string                      `json:"title,omitempty"`
	ChunkCount int                         `json:"chunk_count"`
	OutputPath string                      `json:"output_path,omitempty"`
	Completed  bool                        `json:"completed"`
	Chunks     map[string]resumeChunkState `json:"chunks,omitempty"`
}

type resumeChunkState struct {
	Source     string `json:"source"`
	Translated string `json:"translated"`
}

type resumeStore struct {
	path  string
	state resumeState
}

type processError struct {
	errorType            string
	qualityFallbackCount int
	usage                usageStats
	costEstimate         float64
	costEstimated        bool
	costEstimatePartial  bool
	err                  error
}

func (e *processError) Error() string {
	return e.err.Error()
}

func (e *processError) Unwrap() error {
	return e.err
}

type resumePersistError struct {
	err error
}

func (e *resumePersistError) Error() string {
	return e.err.Error()
}

func (e *resumePersistError) Unwrap() error {
	return e.err
}

func Run(args []string, stdout io.Writer, stderr io.Writer) error {
	opts, err := parseFlags(args, stderr)
	if err != nil {
		return err
	}
	if opts.ShowHelp {
		return nil
	}
	if opts.ShowVersion {
		_, _ = fmt.Fprintln(stdout, version.String())
		return nil
	}

	opts.SourceURLs = normalizeSourceURLs(opts.SourceURLs)
	if len(opts.SourceURLs) == 0 {
		return errors.New("at least one URL is required")
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

	prices, err := loadPriceConfig(opts.PriceConfig)
	if err != nil {
		return err
	}

	outPlan, err := buildOutputPlan(opts)
	if err != nil {
		return err
	}
	if err := prepareOutputPlan(outPlan); err != nil {
		return err
	}

	statePath := filepath.Join(outPlan.summaryDir, stateFileName)
	stateStore, err := loadResumeStore(statePath)
	if err != nil {
		return err
	}

	openAIClient := openai.NewClient(apiKey, baseURL, httpClient, opts.MaxRetries)
	runCtx, stopSignal := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignal()
	runStart := time.Now()

	summary := taskSummary{
		GeneratedAt: optsStartTime(runStart),
		Model:       opts.Model,
		View:        opts.View,
		TotalURLs:   len(opts.SourceURLs),
		Results:     make([]summaryItem, 0, len(opts.SourceURLs)),
	}

	for _, sourceURL := range opts.SourceURLs {
		itemStart := time.Now()
		item := summaryItem{SourceURL: sourceURL}

		output, err := processURL(runCtx, httpClient, openAIClient, opts, glossaryMap, prices, sourceURL, outPlan, stateStore, stderr)
		item.DurationMS = time.Since(itemStart).Milliseconds()
		if err != nil {
			errorType, errorMessage, fallbackCount, failureUsage, failureCost, failureCostEstimated, failureCostPartial := errorDetails(err)
			item.Success = false
			item.ErrorType = errorType
			item.ErrorMessage = errorMessage
			item.QualityFallbackCount = fallbackCount
			item.InputTokens = failureUsage.inputTokens
			item.OutputTokens = failureUsage.outputTokens
			item.TotalTokens = failureUsage.totalTokens
			item.MissingUsageCount = failureUsage.missingUsageCount
			if failureCostEstimated {
				item.CostEstimate = failureCost
				item.CostEstimateModel = opts.Model
				item.CostEstimatePartial = failureCostPartial
			}
			summary.FailureCount++
			summary.QualityFallbackTotalCount += fallbackCount
			summary.InputTokens += failureUsage.inputTokens
			summary.OutputTokens += failureUsage.outputTokens
			summary.TotalTokens += failureUsage.totalTokens
			summary.MissingUsageCount += failureUsage.missingUsageCount
			if failureCostEstimated {
				summary.CostEstimate += failureCost
				summary.CostEstimateModel = opts.Model
				if failureCostPartial {
					summary.CostEstimatePartial = true
				}
			}
			summary.Results = append(summary.Results, item)
			_, _ = fmt.Fprintf(stderr, "Failed [%s]: %s (%s)\n", errorType, compactURL(sourceURL), errorMessage)
			if opts.FailFast {
				_, _ = fmt.Fprintln(stderr, "Fail-fast enabled: stop after first failure.")
				break
			}
			continue
		}

		item.Success = true
		item.FinalURL = output.finalURL
		item.OutputPath = output.outputPath
		item.QualityFallbackCount = output.qualityFallbackCount
		item.InputTokens = output.usage.inputTokens
		item.OutputTokens = output.usage.outputTokens
		item.TotalTokens = output.usage.totalTokens
		item.MissingUsageCount = output.usage.missingUsageCount
		if output.costEstimated {
			item.CostEstimate = output.costEstimate
			item.CostEstimateModel = opts.Model
			item.CostEstimatePartial = output.usage.missingUsageCount > 0
		}
		summary.SuccessCount++
		summary.QualityFallbackTotalCount += output.qualityFallbackCount
		summary.InputTokens += output.usage.inputTokens
		summary.OutputTokens += output.usage.outputTokens
		summary.TotalTokens += output.usage.totalTokens
		summary.MissingUsageCount += output.usage.missingUsageCount
		if output.costEstimated {
			summary.CostEstimate += output.costEstimate
			summary.CostEstimateModel = opts.Model
			if output.usage.missingUsageCount > 0 {
				summary.CostEstimatePartial = true
			}
		}
		summary.Results = append(summary.Results, item)

		_, _ = fmt.Fprintf(stdout, "Output: %s\n", output.outputPath)
	}

	summary.TotalDurationMS = time.Since(runStart).Milliseconds()

	var summaryPath string
	if len(opts.SourceURLs) > 1 {
		summaryPath = filepath.Join(outPlan.summaryDir, summaryFileName)
		if err := writeSummary(summaryPath, summary); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "Summary: %s\n", summaryPath)
	}

	_, _ = fmt.Fprintf(
		stdout,
		"Done: %d succeeded, %d failed, total %s\n",
		summary.SuccessCount,
		summary.FailureCount,
		time.Duration(summary.TotalDurationMS)*time.Millisecond,
	)

	if summary.InputTokens > 0 || summary.OutputTokens > 0 || summary.TotalTokens > 0 {
		_, _ = fmt.Fprintf(
			stdout,
			"Usage: input=%d output=%d total=%d tokens\n",
			summary.InputTokens,
			summary.OutputTokens,
			summary.TotalTokens,
		)
	}
	if summary.MissingUsageCount > 0 {
		_, _ = fmt.Fprintf(
			stderr,
			"Usage info missing for %d chunk(s); totals may be partial.\n",
			summary.MissingUsageCount,
		)
	}
	if summary.CostEstimateModel != "" {
		_, _ = fmt.Fprintf(stdout, "Estimated cost (%s): $%.6f\n", summary.CostEstimateModel, summary.CostEstimate)
		if summary.CostEstimatePartial {
			_, _ = fmt.Fprintln(stderr, "Cost estimate is partial due to missing usage data.")
		}
	}

	if summary.FailureCount > 0 {
		return fmt.Errorf("%d URL(s) failed", summary.FailureCount)
	}
	return nil
}

func parseFlags(args []string, stderr io.Writer) (options, error) {
	fs := flag.NewFlagSet("transblog", flag.ContinueOnError)
	fs.SetOutput(stderr)

	opts := options{}
	fs.StringVar(&opts.Model, "model", "gpt-5.2", "OpenAI model name")
	fs.StringVar(&opts.OutPath, "out", "", "Output path: file for single URL, directory for multiple URLs (default: ./out/)")
	fs.BoolVar(&opts.View, "view", false, "Generate HTML split-view with Markdown rendering and synchronized scrolling")
	fs.BoolVar(&opts.ShowVersion, "version", false, "Print version information and exit")
	fs.IntVar(&opts.Workers, "workers", defaultTranslateWorkers, "Translation worker count")
	fs.IntVar(&opts.MaxRetries, "max-retries", defaultMaxRetries, "Maximum retries for OpenAI requests")
	fs.BoolVar(&opts.FailFast, "fail-fast", false, "Stop at first URL failure (default: continue for partial success)")
	fs.StringVar(&opts.PriceConfig, "price-config", "", "Optional JSON pricing config file for cost estimation")
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
	if opts.Workers <= 0 {
		return options{}, errors.New("--workers must be greater than 0")
	}
	if opts.MaxRetries < 0 {
		return options{}, errors.New("--max-retries must be 0 or greater")
	}
	if opts.ChunkSize <= 0 {
		opts.ChunkSize = defaultChunkSize
	}

	opts.SourceURLs = fs.Args()
	if opts.ShowVersion {
		return opts, nil
	}
	if len(opts.SourceURLs) == 0 {
		fs.Usage()
		return options{}, errors.New("at least one URL is required")
	}

	return opts, nil
}

func normalizeSourceURLs(urls []string) []string {
	out := make([]string, 0, len(urls))
	for _, raw := range urls {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		out = append(out, trimmed)
	}
	return out
}

func buildOutputPlan(opts options) (outputPlan, error) {
	if opts.OutPath == "" {
		return outputPlan{
			outputDir:  defaultOutDir,
			summaryDir: defaultOutDir,
		}, nil
	}

	if len(opts.SourceURLs) == 1 {
		return outputPlan{
			singleFile: opts.OutPath,
			summaryDir: filepath.Dir(opts.OutPath),
		}, nil
	}

	return outputPlan{
		outputDir:  opts.OutPath,
		summaryDir: opts.OutPath,
	}, nil
}

func prepareOutputPlan(plan outputPlan) error {
	if plan.outputDir != "" {
		if err := os.MkdirAll(plan.outputDir, 0o755); err != nil {
			return fmt.Errorf("create output directory: %w", err)
		}
	}

	if plan.singleFile != "" {
		dir := filepath.Dir(plan.singleFile)
		if dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("create output directory: %w", err)
			}
		}
	}

	if plan.summaryDir != "" && plan.summaryDir != "." {
		if err := os.MkdirAll(plan.summaryDir, 0o755); err != nil {
			return fmt.Errorf("create summary directory: %w", err)
		}
	}

	return nil
}

func loadPriceConfig(path string) (priceConfig, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read price config %s: %w", path, err)
	}

	var cfg priceConfig
	if err := json.Unmarshal(content, &cfg); err != nil {
		return nil, fmt.Errorf("parse price config %s: %w", path, err)
	}

	return cfg, nil
}

func estimateCost(usage usageStats, cfg priceConfig, model string) (cost float64, estimated bool, partial bool) {
	if cfg == nil {
		return 0, false, false
	}

	price, ok := cfg[model]
	if !ok {
		price, ok = cfg["default"]
		if !ok {
			return 0, false, false
		}
	}

	if price.InputPerMillion > 0 || price.OutputPerMillion > 0 {
		cost = float64(usage.inputTokens)*price.InputPerMillion/1_000_000.0 +
			float64(usage.outputTokens)*price.OutputPerMillion/1_000_000.0
	} else if price.TotalPerMillion > 0 {
		cost = float64(usage.totalTokens) * price.TotalPerMillion / 1_000_000.0
	} else {
		return 0, false, false
	}

	return cost, true, usage.missingUsageCount > 0
}

func loadResumeStore(path string) (*resumeStore, error) {
	store := &resumeStore{
		path: path,
		state: resumeState{
			Version: 1,
			URLs:    map[string]*resumeURLState{},
		},
	}

	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read resume state file %s: %w", path, err)
	}

	if strings.TrimSpace(string(content)) == "" {
		return store, nil
	}

	if err := json.Unmarshal(content, &store.state); err != nil {
		return nil, fmt.Errorf("parse resume state file %s: %w", path, err)
	}
	if store.state.URLs == nil {
		store.state.URLs = map[string]*resumeURLState{}
	}
	if store.state.Version == 0 {
		store.state.Version = 1
	}

	return store, nil
}

func (s *resumeStore) prepareURL(sourceURL string, finalURL string, title string, chunkCount int) error {
	entry := s.ensureURLEntry(sourceURL)
	if entry.ChunkCount != chunkCount {
		entry.Chunks = map[string]resumeChunkState{}
	}

	entry.FinalURL = finalURL
	entry.Title = title
	entry.ChunkCount = chunkCount
	entry.OutputPath = ""
	entry.Completed = false

	return s.persist()
}

func (s *resumeStore) loadChunk(sourceURL string, index int, source string, chunkCount int) (string, bool) {
	entry, ok := s.state.URLs[sourceURL]
	if !ok || entry == nil {
		return "", false
	}
	if entry.ChunkCount != chunkCount {
		return "", false
	}

	chunk, ok := entry.Chunks[strconv.Itoa(index)]
	if !ok {
		return "", false
	}
	if chunk.Source != source || chunk.Translated == "" {
		return "", false
	}

	return chunk.Translated, true
}

func (s *resumeStore) saveChunk(sourceURL string, finalURL string, title string, chunkCount int, index int, source string, translated string) error {
	entry := s.ensureURLEntry(sourceURL)
	if entry.ChunkCount != chunkCount {
		entry.Chunks = map[string]resumeChunkState{}
	}

	entry.FinalURL = finalURL
	entry.Title = title
	entry.ChunkCount = chunkCount
	entry.Completed = false
	if entry.Chunks == nil {
		entry.Chunks = map[string]resumeChunkState{}
	}
	entry.Chunks[strconv.Itoa(index)] = resumeChunkState{
		Source:     source,
		Translated: translated,
	}

	return s.persist()
}

func (s *resumeStore) markURLComplete(sourceURL string) error {
	delete(s.state.URLs, sourceURL)
	return s.persist()
}

func (s *resumeStore) ensureURLEntry(sourceURL string) *resumeURLState {
	entry, ok := s.state.URLs[sourceURL]
	if ok && entry != nil {
		if entry.Chunks == nil {
			entry.Chunks = map[string]resumeChunkState{}
		}
		return entry
	}

	entry = &resumeURLState{
		Chunks: map[string]resumeChunkState{},
	}
	s.state.URLs[sourceURL] = entry
	return entry
}

func (s *resumeStore) persist() error {
	if len(s.state.URLs) == 0 {
		if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove resume state file %s: %w", s.path, err)
		}
		return nil
	}

	s.state.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

	payload, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal resume state: %w", err)
	}
	payload = append(payload, '\n')

	tmpPath := s.path + ".tmp"
	if err := os.WriteFile(tmpPath, payload, 0o644); err != nil {
		return fmt.Errorf("write resume state temp file %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("replace resume state file %s: %w", s.path, err)
	}
	return nil
}

func processURL(
	ctx context.Context,
	httpClient *http.Client,
	openAIClient *openai.Client,
	opts options,
	glossaryMap map[string]string,
	prices priceConfig,
	sourceURL string,
	outPlan outputPlan,
	stateStore *resumeStore,
	progress io.Writer,
) (urlOutput, error) {
	doc, err := fetch.HTML(ctx, httpClient, sourceURL)
	if err != nil {
		return urlOutput{}, newProcessError(errorTypeFetch, fmt.Errorf("fetch %s: %w", sourceURL, err))
	}

	markdownText, err := markdown.FromHTML(doc.HTML)
	if err != nil {
		return urlOutput{}, newProcessError(errorTypeConvert, fmt.Errorf("convert %s HTML to markdown: %w", sourceURL, err))
	}

	sourceDoc := sourceDocument{
		markdown: markdownText,
		title:    doc.Title,
		finalURL: doc.FinalURL,
	}

	taskSourceURL := sourceURL
	if sourceDoc.finalURL != "" {
		taskSourceURL = sourceDoc.finalURL
	}

	chunks := chunk.SplitMarkdown(sourceDoc.markdown, opts.ChunkSize)
	if len(chunks) == 0 {
		return urlOutput{}, newProcessError(errorTypeConvert, errors.New("no content after markdown conversion"))
	}

	if err := stateStore.prepareURL(sourceURL, taskSourceURL, sourceDoc.title, len(chunks)); err != nil {
		return urlOutput{}, newProcessError(errorTypeOutput, fmt.Errorf("prepare resume state for %s: %w", sourceURL, err))
	}

	pairs := make([]pair, len(chunks))

	tasks := make([]translationTask, 0, len(chunks))
	resumedChunkCount := 0
	for idx, mdChunk := range chunks {
		if translated, ok := stateStore.loadChunk(sourceURL, idx, mdChunk, len(chunks)); ok {
			pairs[idx] = pair{
				sourceURL:   taskSourceURL,
				sourceTitle: sourceDoc.title,
				source:      mdChunk,
				translated:  translated,
			}
			resumedChunkCount++
			continue
		}

		tasks = append(tasks, translationTask{
			outputIndex: idx,
			sourceURL:   taskSourceURL,
			sourceTitle: sourceDoc.title,
			sourceChunk: mdChunk,
			chunkNumber: idx + 1,
			chunkTotal:  len(chunks),
		})
	}

	if resumedChunkCount > 0 {
		_, _ = fmt.Fprintf(
			progress,
			"Resume: reused %d/%d chunks for %s\n",
			resumedChunkCount,
			len(chunks),
			compactURL(taskSourceURL),
		)
	}

	urlQualityFallbackCount := 0
	urlUsage := usageStats{}
	if len(tasks) > 0 {
		translatedPairs, qualityFallbackCount, translatedUsage, err := translateAllChunks(
			ctx,
			openAIClient,
			opts.Model,
			glossaryMap,
			tasks,
			progress,
			opts.Workers,
			opts.FailFast,
			func(task translationTask, translated string) error {
				if err := stateStore.saveChunk(
					sourceURL,
					taskSourceURL,
					sourceDoc.title,
					len(chunks),
					task.outputIndex,
					task.sourceChunk,
					translated,
				); err != nil {
					return &resumePersistError{err: fmt.Errorf("save resume chunk %d for %s: %w", task.chunkNumber, sourceURL, err)}
				}
				return nil
			},
		)
		if err != nil {
			var persistErr *resumePersistError
			if errors.As(err, &persistErr) {
				return urlOutput{}, newProcessErrorWithDetails(errorTypeOutput, persistErr, qualityFallbackCount, translatedUsage, 0, false, false)
			}
			cost, costEstimated, costPartial := estimateCost(translatedUsage, prices, opts.Model)
			return urlOutput{}, newProcessErrorWithDetails(errorTypeTranslate, err, qualityFallbackCount, translatedUsage, cost, costEstimated, costPartial)
		}

		urlQualityFallbackCount = qualityFallbackCount
		urlUsage = translatedUsage
		if qualityFallbackCount > 0 {
			_, _ = fmt.Fprintf(
				progress,
				"Quality fallback: triggered %d time(s) for %s\n",
				qualityFallbackCount,
				compactURL(taskSourceURL),
			)
		}

		for idx, translatedPair := range translatedPairs {
			if translatedPair.source == "" {
				continue
			}
			pairs[idx] = translatedPair
		}
	}

	for idx, p := range pairs {
		if p.source == "" {
			return urlOutput{}, newProcessError(errorTypeTranslate, fmt.Errorf("missing translated chunk %d for %s", idx+1, sourceURL))
		}
	}

	perURLOpts := opts
	perURLOpts.SourceURLs = []string{taskSourceURL}

	var output strings.Builder
	if opts.View {
		writeHTMLView(&output, pairs, perURLOpts)
	} else {
		writeMarkdown(&output, pairs, perURLOpts)
	}

	cost, costEstimated, costPartial := estimateCost(urlUsage, prices, opts.Model)

	outPath, err := outputPathForURL(outPlan, taskSourceURL, opts.View)
	if err != nil {
		return urlOutput{}, newProcessErrorWithDetails(errorTypeOutput, err, urlQualityFallbackCount, urlUsage, cost, costEstimated, costPartial)
	}
	if err := os.WriteFile(outPath, []byte(output.String()), 0o644); err != nil {
		return urlOutput{}, newProcessErrorWithDetails(errorTypeOutput, fmt.Errorf("write output file %s: %w", outPath, err), urlQualityFallbackCount, urlUsage, cost, costEstimated, costPartial)
	}

	if err := stateStore.markURLComplete(sourceURL); err != nil {
		return urlOutput{}, newProcessErrorWithDetails(errorTypeOutput, fmt.Errorf("finalize resume state for %s: %w", sourceURL, err), urlQualityFallbackCount, urlUsage, cost, costEstimated, costPartial)
	}

	return urlOutput{
		finalURL:             taskSourceURL,
		outputPath:           outPath,
		qualityFallbackCount: urlQualityFallbackCount,
		usage:                urlUsage,
		costEstimate:         cost,
		costEstimated:        costEstimated,
	}, nil
}

func outputPathForURL(plan outputPlan, sourceURL string, view bool) (string, error) {
	if plan.singleFile != "" {
		return plan.singleFile, nil
	}

	filename, err := filenameFromURL(sourceURL, view)
	if err != nil {
		return "", err
	}
	return filepath.Join(plan.outputDir, filename), nil
}

func filenameFromURL(rawURL string, view bool) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("invalid URL: %s", rawURL)
	}

	base := parsed.Host + parsed.Path
	if parsed.RawQuery != "" {
		base += "_" + parsed.RawQuery
	}
	base = strings.ReplaceAll(base, "/", "_")
	base = sanitizeFilename(base)
	base = strings.Trim(base, "_")
	if base == "" {
		base = "output"
	}

	ext := ".md"
	if view {
		ext = ".html"
	}
	return base + ext, nil
}

func sanitizeFilename(s string) string {
	var b strings.Builder
	lastUnderscore := false

	for _, r := range s {
		allowed := (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '.' || r == '-' || r == '_'

		if allowed {
			b.WriteRune(r)
			lastUnderscore = r == '_'
			continue
		}

		if !lastUnderscore {
			b.WriteByte('_')
			lastUnderscore = true
		}
	}

	return b.String()
}

func writeSummary(path string, summary taskSummary) error {
	payload, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal summary JSON: %w", err)
	}
	payload = append(payload, '\n')
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		return fmt.Errorf("write summary file %s: %w", path, err)
	}
	return nil
}

func newProcessError(errorType string, err error) error {
	return newProcessErrorWithDetails(errorType, err, 0, usageStats{}, 0, false, false)
}

func newProcessErrorWithDetails(
	errorType string,
	err error,
	qualityFallbackCount int,
	usage usageStats,
	costEstimate float64,
	costEstimated bool,
	costEstimatePartial bool,
) error {
	if err == nil {
		return nil
	}
	return &processError{
		errorType:            errorType,
		qualityFallbackCount: qualityFallbackCount,
		usage:                usage,
		costEstimate:         costEstimate,
		costEstimated:        costEstimated,
		costEstimatePartial:  costEstimatePartial,
		err:                  err,
	}
}

func errorDetails(err error) (
	errorType string,
	errorMessage string,
	qualityFallbackCount int,
	usage usageStats,
	costEstimate float64,
	costEstimated bool,
	costEstimatePartial bool,
) {
	if err == nil {
		return "", "", 0, usageStats{}, 0, false, false
	}

	var stageErr *processError
	if errors.As(err, &stageErr) {
		return stageErr.errorType,
			stageErr.err.Error(),
			stageErr.qualityFallbackCount,
			stageErr.usage,
			stageErr.costEstimate,
			stageErr.costEstimated,
			stageErr.costEstimatePartial
	}

	return errorTypeUnknown, err.Error(), 0, usageStats{}, 0, false, false
}

func optsStartTime(start time.Time) string {
	return start.UTC().Format(time.RFC3339)
}

type sourceDocument struct {
	markdown string
	title    string
	finalURL string
}

type translationTask struct {
	outputIndex int
	sourceURL   string
	sourceTitle string
	sourceChunk string
	chunkNumber int
	chunkTotal  int
}

type translationResult struct {
	task                 translationTask
	translated           string
	qualityFallbackCount int
	usage                usageStats
	err                  error
}

func translateAllChunks(
	ctx context.Context,
	client *openai.Client,
	model string,
	glossaryMap map[string]string,
	tasks []translationTask,
	progress io.Writer,
	workers int,
	failFast bool,
	onChunkTranslated func(task translationTask, translated string) error,
) ([]pair, int, usageStats, error) {
	if len(tasks) == 0 {
		return nil, 0, usageStats{}, nil
	}
	if progress == nil {
		progress = io.Discard
	}

	workerCount := workers
	if workerCount > len(tasks) {
		workerCount = len(tasks)
	}
	if workerCount < 1 {
		workerCount = 1
	}

	_, _ = fmt.Fprintf(progress, "Translating %d chunks with %d workers...\n", len(tasks), workerCount)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan translationTask)
	results := make(chan translationResult, len(tasks))

	var wg sync.WaitGroup
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for task := range jobs {
				if runCtx.Err() != nil {
					return
				}

				translated, chunkUsage, fallbackCount, err := translateChunkWithQualityGuard(
					runCtx,
					client,
					model,
					task.sourceChunk,
					glossaryMap,
				)
				if err != nil {
					select {
					case results <- translationResult{
						task:                 task,
						qualityFallbackCount: fallbackCount,
						usage:                chunkUsage,
						err:                  fmt.Errorf("translate chunk %d for %s: %w", task.chunkNumber, task.sourceURL, err),
					}:
					case <-runCtx.Done():
					}
					if failFast {
						cancel()
						return
					}
					continue
				}
				select {
				case results <- translationResult{
					task:                 task,
					translated:           translated,
					qualityFallbackCount: fallbackCount,
					usage:                chunkUsage,
				}:
				case <-runCtx.Done():
					return
				}
			}
		}()
	}

	go func() {
		defer close(jobs)
		for _, task := range tasks {
			select {
			case <-runCtx.Done():
				return
			case jobs <- task:
			}
		}
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	maxOutputIndex := -1
	for _, task := range tasks {
		if task.outputIndex > maxOutputIndex {
			maxOutputIndex = task.outputIndex
		}
	}

	pairs := make([]pair, maxOutputIndex+1)
	completed := 0
	qualityFallbackCount := 0
	totalUsage := usageStats{}
	var firstErr error

	for result := range results {
		totalUsage.add(result.usage)
		if result.err != nil {
			qualityFallbackCount += result.qualityFallbackCount
			if firstErr == nil {
				firstErr = result.err
			}
			continue
		}

		task := result.task
		pairs[task.outputIndex] = pair{
			sourceURL:   task.sourceURL,
			sourceTitle: task.sourceTitle,
			source:      task.sourceChunk,
			translated:  result.translated,
		}

		if onChunkTranslated != nil {
			if err := onChunkTranslated(task, result.translated); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				cancel()
				continue
			}
		}

		qualityFallbackCount += result.qualityFallbackCount
		completed++
		percent := completed * 100 / len(tasks)
		_, _ = fmt.Fprintf(
			progress,
			"[%d/%d] %d%% translated chunk %d/%d for %s\n",
			completed,
			len(tasks),
			percent,
			task.chunkNumber,
			task.chunkTotal,
			compactURL(task.sourceURL),
		)
	}

	if firstErr != nil {
		return nil, qualityFallbackCount, totalUsage, firstErr
	}

	_, _ = fmt.Fprintf(progress, "Translation complete: %d chunks.\n", completed)
	return pairs, qualityFallbackCount, totalUsage, nil
}

func translateChunkWithQualityGuard(
	ctx context.Context,
	client *openai.Client,
	model string,
	sourceChunk string,
	glossaryMap map[string]string,
) (string, usageStats, int, error) {
	totalUsage := usageStats{}

	translated, usage, err := client.TranslateMarkdownChunkWithUsage(ctx, model, sourceChunk, glossaryMap)
	if err != nil {
		return "", totalUsage, 0, err
	}
	totalUsage.addOpenAIUsage(usage)
	translated = glossary.Apply(translated, glossaryMap)

	if err := validateTranslatedChunk(sourceChunk, translated); err == nil {
		return translated, totalUsage, 0, nil
	} else {
		lastValidationErr := err
		fallbackCount := 0

		for retry := 0; retry < defaultQualityRetries; retry++ {
			fallbackCount++

			strictTranslated, strictUsage, strictErr := client.TranslateMarkdownChunkStrictWithUsage(
				ctx,
				model,
				sourceChunk,
				glossaryMap,
				lastValidationErr.Error(),
			)
			if strictErr != nil {
				return "", totalUsage, fallbackCount, fmt.Errorf("strict quality retry failed: %w", strictErr)
			}
			totalUsage.addOpenAIUsage(strictUsage)

			strictTranslated = glossary.Apply(strictTranslated, glossaryMap)
			if validationErr := validateTranslatedChunk(sourceChunk, strictTranslated); validationErr == nil {
				return strictTranslated, totalUsage, fallbackCount, nil
			} else {
				lastValidationErr = validationErr
			}
		}

		return "", totalUsage, fallbackCount, fmt.Errorf("translated markdown failed quality validation: %w", lastValidationErr)
	}
}

func validateTranslatedChunk(source string, translated string) error {
	if strings.TrimSpace(translated) == "" {
		return errors.New("empty translated markdown")
	}

	if err := validateFenceBalance(translated); err != nil {
		return err
	}

	sourceShape := markdownShapeFromText(source)
	translatedShape := markdownShapeFromText(translated)
	if err := validateMarkdownShape(sourceShape, translatedShape); err != nil {
		return err
	}

	return nil
}

type markdownShape struct {
	headings      int
	unorderedList int
	orderedList   int
	blockquote    int
	tableRows     int
}

func markdownShapeFromText(markdownText string) markdownShape {
	lines := strings.Split(markdownText, "\n")

	var shape markdownShape
	inFence := false
	fenceMarker := byte(0)
	fenceLen := 0

	for _, rawLine := range lines {
		trimmed := strings.TrimSpace(rawLine)
		if trimmed == "" {
			continue
		}

		if marker, markerLen, ok := parseFenceMarker(trimmed); ok {
			if !inFence {
				inFence = true
				fenceMarker = marker
				fenceLen = markerLen
			} else if marker == fenceMarker && markerLen >= fenceLen {
				inFence = false
				fenceMarker = 0
				fenceLen = 0
			}
			continue
		}
		if inFence {
			continue
		}

		if isHeadingLine(trimmed) {
			shape.headings++
		}
		if isUnorderedListLine(trimmed) {
			shape.unorderedList++
		}
		if isOrderedListLine(trimmed) {
			shape.orderedList++
		}
		if strings.HasPrefix(trimmed, ">") {
			shape.blockquote++
		}
		if strings.Count(trimmed, "|") >= 2 {
			shape.tableRows++
		}
	}

	return shape
}

func validateMarkdownShape(source markdownShape, translated markdownShape) error {
	if source.headings > 0 && translated.headings == 0 {
		return fmt.Errorf("missing headings: source=%d translated=%d", source.headings, translated.headings)
	}
	if source.blockquote > 0 && translated.blockquote == 0 {
		return fmt.Errorf("missing blockquote: source=%d translated=%d", source.blockquote, translated.blockquote)
	}
	if source.tableRows > 1 && translated.tableRows == 0 {
		return fmt.Errorf("missing table rows: source=%d translated=%d", source.tableRows, translated.tableRows)
	}

	sourceListTotal := source.unorderedList + source.orderedList
	translatedListTotal := translated.unorderedList + translated.orderedList
	if sourceListTotal >= 3 && translatedListTotal < sourceListTotal/2 {
		return fmt.Errorf("list structure collapsed: source=%d translated=%d", sourceListTotal, translatedListTotal)
	}
	if sourceListTotal > 0 && translatedListTotal == 0 {
		return fmt.Errorf("missing list structure: source=%d translated=%d", sourceListTotal, translatedListTotal)
	}

	return nil
}

func validateFenceBalance(markdownText string) error {
	lines := strings.Split(markdownText, "\n")
	inFence := false
	fenceMarker := byte(0)
	fenceLen := 0

	for _, rawLine := range lines {
		trimmed := strings.TrimSpace(rawLine)
		if marker, markerLen, ok := parseFenceMarker(trimmed); ok {
			if !inFence {
				inFence = true
				fenceMarker = marker
				fenceLen = markerLen
			} else if marker == fenceMarker && markerLen >= fenceLen {
				inFence = false
				fenceMarker = 0
				fenceLen = 0
			}
		}
	}

	if inFence {
		return errors.New("unbalanced fenced code block")
	}
	return nil
}

func parseFenceMarker(line string) (byte, int, bool) {
	if len(line) < 3 {
		return 0, 0, false
	}

	switch line[0] {
	case '`', '~':
		marker := line[0]
		count := 0
		for count < len(line) && line[count] == marker {
			count++
		}
		if count >= 3 {
			return marker, count, true
		}
	}

	return 0, 0, false
}

func isHeadingLine(trimmed string) bool {
	if !strings.HasPrefix(trimmed, "#") {
		return false
	}
	if len(trimmed) == 1 {
		return true
	}
	return trimmed[1] == ' '
}

func isUnorderedListLine(trimmed string) bool {
	if len(trimmed) < 2 {
		return false
	}
	switch trimmed[0] {
	case '-', '*', '+':
		return trimmed[1] == ' '
	default:
		return false
	}
}

func isOrderedListLine(trimmed string) bool {
	dot := strings.IndexByte(trimmed, '.')
	if dot <= 0 || dot+1 >= len(trimmed) {
		return false
	}
	for i := 0; i < dot; i++ {
		if trimmed[i] < '0' || trimmed[i] > '9' {
			return false
		}
	}
	return trimmed[dot+1] == ' '
}

func compactURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return rawURL
	}

	path := strings.TrimSuffix(parsed.Path, "/")
	if path == "" {
		return parsed.Host
	}

	return parsed.Host + path
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
	sourceURL   string
	sourceTitle string
	source      string
	translated  string
}

func writeMarkdown(w io.StringWriter, pairs []pair, opts options) {
	writeFrontMatter(w, opts)

	for i, p := range pairs {
		if i > 0 {
			_, _ = w.WriteString("\n\n")
		}
		if p.sourceTitle != "" {
			_, _ = w.WriteString("## Source: ")
			_, _ = w.WriteString(p.sourceTitle)
			_, _ = w.WriteString(" (")
			_, _ = w.WriteString(p.sourceURL)
			_, _ = w.WriteString(")\n\n")
		} else {
			_, _ = w.WriteString("## Source: ")
			_, _ = w.WriteString(p.sourceURL)
			_, _ = w.WriteString("\n\n")
		}
		_, _ = w.WriteString(p.source)
		_, _ = w.WriteString("\n\n---\n\n")
		_, _ = w.WriteString(p.translated)
	}
}

func writeHTMLView(w io.StringWriter, pairs []pair, opts options) {
	type viewPair struct {
		SourceURL   string `json:"sourceURL"`
		SourceTitle string `json:"sourceTitle,omitempty"`
		Source      string `json:"source"`
		Translated  string `json:"translated"`
	}

	viewPairs := make([]viewPair, 0, len(pairs))
	for _, p := range pairs {
		viewPairs = append(viewPairs, viewPair{
			SourceURL:   p.sourceURL,
			SourceTitle: p.sourceTitle,
			Source:      p.source,
			Translated:  p.translated,
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
<title>TransBlog</title>
<link rel="icon" href="data:,">
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
button.toolbar-item {
  color: inherit;
  cursor: pointer;
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
  grid-template-columns: 260px 1fr 1fr;
  gap: 12px;
  padding: 12px;
}
.toc-collapsed .layout {
  grid-template-columns: 1fr 1fr;
}
.toc-collapsed .toc-pane {
  display: none;
}
.toc-pane {
  display: flex;
  flex-direction: column;
  min-height: 0;
  border: 1px solid #dbe3ef;
  border-radius: 12px;
  overflow: hidden;
  background: #ffffff;
  box-shadow: 0 12px 36px rgba(15, 23, 42, 0.1);
}
.toc-scroll {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  padding: 10px;
}
.toc-link {
  display: block;
  width: 100%;
  text-align: left;
  border: 0;
  background: transparent;
  color: #334155;
  border-radius: 8px;
  padding: 7px 10px;
  margin-bottom: 4px;
  font-family: "Avenir Next", "Segoe UI", sans-serif;
  font-size: 13px;
  cursor: pointer;
}
.toc-link.level-2 {
  padding-left: 18px;
  font-size: 12px;
}
.toc-link:hover {
  background: #e2e8f0;
}
.toc-link.active {
  background: #dbeafe;
  color: #0f172a;
  font-weight: 700;
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
body.theme-dark {
  color: #e2e8f0;
  background: radial-gradient(circle at 20% 0%, #0f172a 0%, #020617 44%, #030712 100%);
}
body.theme-dark .topbar {
  background: #020617;
  color: #e2e8f0;
  border-bottom-color: rgba(148, 163, 184, 0.3);
}
body.theme-dark .toolbar-item {
  border-color: rgba(148, 163, 184, 0.45);
  background: rgba(148, 163, 184, 0.15);
  color: #e2e8f0;
}
body.theme-dark .toolbar-item select {
  border-color: rgba(148, 163, 184, 0.55);
  background: #0f172a;
  color: #e2e8f0;
}
body.theme-dark .toc-pane,
body.theme-dark .pane {
  background: #0f172a;
  border-color: #334155;
  box-shadow: 0 12px 36px rgba(2, 6, 23, 0.45);
}
body.theme-dark .pane-head {
  background: linear-gradient(180deg, #111827 0%, #0b1220 100%);
  border-bottom-color: #334155;
  color: #cbd5e1;
}
body.theme-dark .chunk {
  border-bottom-color: #334155;
}
body.theme-dark .chunk-meta,
body.theme-dark .empty-state {
  color: #94a3b8;
}
body.theme-dark .toc-link {
  color: #cbd5e1;
}
body.theme-dark .toc-link:hover {
  background: #1e293b;
}
body.theme-dark .toc-link.active {
  background: #1d4ed8;
  color: #e2e8f0;
}
body.theme-dark .markdown-body code {
  background: #1e293b;
  color: #e2e8f0;
}
body.theme-dark .markdown-body pre {
  background: #020617;
  color: #dbeafe;
}
body.theme-dark .markdown-body blockquote {
  border-left-color: #60a5fa;
  color: #cbd5e1;
}
body.theme-dark .markdown-body a {
  color: #93c5fd;
}
body.theme-dark .markdown-body th,
body.theme-dark .markdown-body td {
  border-color: #334155;
}
@media (max-width: 1240px) {
  .layout {
    grid-template-columns: 1fr 1fr;
    grid-template-rows: minmax(180px, 28vh) minmax(0, 1fr);
    height: calc(100vh - 96px);
    min-height: 0;
  }
  .toc-pane {
    grid-column: 1 / -1;
    min-height: 0;
  }
  .toc-collapsed .layout {
    grid-template-columns: 1fr 1fr;
    grid-template-rows: minmax(0, 1fr);
  }
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
  .toc-pane {
    min-height: 200px;
  }
  .toc-collapsed .layout {
    grid-template-columns: 1fr;
  }
  .pane { min-height: 44vh; }
}
</style>
</head>
<body>
`)

	_, _ = w.WriteString("<header class=\"topbar\">\n")
	_, _ = w.WriteString("<h1 id=\"page-title\">TransBlog</h1>\n")
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
	_, _ = w.WriteString("  <label class=\"toolbar-item\">")
	_, _ = w.WriteString("<input id=\"theme-toggle\" type=\"checkbox\"> 深色模式</label>\n")
	_, _ = w.WriteString("  <button id=\"toc-toggle\" class=\"toolbar-item\" type=\"button\" aria-expanded=\"true\">收起目录</button>\n")
	_, _ = w.WriteString("  <label class=\"toolbar-item\">Chunk")
	_, _ = w.WriteString("<select id=\"chunk-jump\"></select></label>\n")
	_, _ = w.WriteString("  <div class=\"toolbar-item progress-chip\" id=\"reading-progress\">Chunk 0 / 0</div>\n")
	_, _ = w.WriteString("</div>\n")
	_, _ = w.WriteString("</header>\n")

	_, _ = w.WriteString("<main class=\"layout\">\n")
	_, _ = w.WriteString("  <aside class=\"toc-pane\">\n")
	_, _ = w.WriteString("    <div class=\"pane-head\">目录</div>\n")
	_, _ = w.WriteString("    <div id=\"toc-nav\" class=\"toc-scroll\"></div>\n")
	_, _ = w.WriteString("  </aside>\n")
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
  const tocNav = document.getElementById("toc-nav");
  const syncLock = document.getElementById("sync-lock");
  const themeToggle = document.getElementById("theme-toggle");
  const tocToggle = document.getElementById("toc-toggle");
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
  let tocCollapsed = false;

  if (window.marked && typeof window.marked.setOptions === "function") {
    window.marked.setOptions({
      gfm: true,
      breaks: false,
      headerIds: false,
      mangle: false,
    });
  }

  function applyTheme(themeName) {
    const dark = themeName === "dark";
    document.body.classList.toggle("theme-dark", dark);
    if (themeToggle) {
      themeToggle.checked = dark;
    }
  }

  if (themeToggle) {
    const prefersDark = window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches;
    applyTheme(prefersDark ? "dark" : "light");
    themeToggle.addEventListener("change", function () {
      applyTheme(themeToggle.checked ? "dark" : "light");
    });
  }

  function applyTOCState(collapsed) {
    tocCollapsed = Boolean(collapsed);
    document.body.classList.toggle("toc-collapsed", tocCollapsed);
    if (!tocToggle) {
      return;
    }
    tocToggle.setAttribute("aria-expanded", tocCollapsed ? "false" : "true");
    tocToggle.textContent = tocCollapsed ? "展开目录" : "收起目录";
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

  function fixImageAttributes(root, baseURL) {
    if (!root) {
      return;
    }
    root.querySelectorAll("img").forEach(function (img) {
      const src = img.getAttribute("src");
      if (src && !src.startsWith("http:") && !src.startsWith("https:") && !src.startsWith("data:")) {
        try {
          img.src = new URL(src, baseURL).href;
        } catch (_) {
          img.src = src;
        }
      }
      img.setAttribute("referrerpolicy", "no-referrer");
      img.setAttribute("loading", "lazy");
    });
  }

  function slugifyHeading(text) {
    return String(text || "")
      .trim()
      .toLowerCase()
      .replace(/[^a-z0-9\u4e00-\u9fff\s-]/g, "")
      .replace(/\s+/g, "-")
      .replace(/-+/g, "-")
      .replace(/^-|-$/g, "");
  }

  function annotateEnglishHeadings(root, chunkIndex) {
    if (!root) {
      return;
    }
    const headings = root.querySelectorAll("h1, h2");
    headings.forEach(function (heading, headingIndex) {
      const slug = slugifyHeading(heading.textContent);
      const suffix = slug || "section";
      heading.id = "en-heading-" + chunkIndex + "-" + headingIndex + "-" + suffix;
    });
  }

  function elementTopInPane(element, pane) {
    const elementRect = element.getBoundingClientRect();
    const paneRect = pane.getBoundingClientRect();
    return elementRect.top - paneRect.top + pane.scrollTop;
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
      if (paneId === "en") {
        annotateEnglishHeadings(body, index);
      }
      fixImageAttributes(body, item.sourceURL);
      applyHighlighting(body);

      chunk.appendChild(meta);
      chunk.appendChild(body);
      pane.appendChild(chunk);
    });

    chunkNodes[paneId] = Array.from(pane.querySelectorAll(".chunk"));
  }

  function updateTitleFromContent() {
    const titleEl = document.getElementById("page-title");
    const firstH1 = paneEN.querySelector("h1");
    const title = firstH1 && firstH1.textContent ? firstH1.textContent.trim() : "";
    const finalTitle = title || "TransBlog";
    if (titleEl) {
      titleEl.textContent = finalTitle;
    }
    document.title = finalTitle;
  }

  function updateTOCActive() {
    if (!tocNav) {
      return;
    }
    const links = Array.from(tocNav.querySelectorAll(".toc-link"));
    if (links.length === 0) {
      return;
    }

    let activeIndex = 0;
    const threshold = paneEN.scrollTop + 12;
    links.forEach(function (link, index) {
      const targetId = link.getAttribute("data-target-id");
      if (!targetId) {
        return;
      }
      const heading = document.getElementById(targetId);
      if (heading && elementTopInPane(heading, paneEN) <= threshold) {
        activeIndex = index;
      }
    });

    links.forEach(function (link, index) {
      link.classList.toggle("active", index === activeIndex);
    });
  }

  function buildTOC() {
    if (!tocNav) {
      return;
    }

    tocNav.innerHTML = "";
    const headings = Array.from(paneEN.querySelectorAll("h1, h2"));
    if (headings.length === 0) {
      const empty = document.createElement("p");
      empty.className = "empty-state";
      empty.textContent = "No headings available.";
      tocNav.appendChild(empty);
      return;
    }

    headings.forEach(function (heading, index) {
      const text = heading.textContent ? heading.textContent.trim() : "";
      const label = text || ("Section " + (index + 1));
      const level = heading.tagName === "H1" ? 1 : 2;
      const button = document.createElement("button");
      button.type = "button";
      button.className = "toc-link level-" + level;
      button.textContent = label;
      button.setAttribute("data-target-id", heading.id);
      button.addEventListener("click", function () {
        activePaneId = "en";
        const targetTop = Math.max(elementTopInPane(heading, paneEN) - 8, 0);
        markSuppressed("en", 140);
        paneEN.scrollTo({ top: targetTop, behavior: "smooth" });
        window.setTimeout(function () {
          scheduleSyncFrom("en");
        }, 160);
      });
      tocNav.appendChild(button);
    });

    updateTOCActive();
  }

  renderPane("en", "source");
  renderPane("zh", "translated");
  updateTitleFromContent();
  buildTOC();

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
    const maxTop = Math.max(pane.scrollHeight - pane.clientHeight, 0);
    const spans = chunks.map(function (_, index) {
      const start = tops[index];
      const end = index + 1 < tops.length ? tops[index + 1] : maxTop;
      return Math.max(end - start, 1);
    });
    return { tops: tops, spans: spans, maxTop: maxTop };
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
    const maxTop = metric.maxTop || Math.max(pane.scrollHeight - pane.clientHeight, 0);
    if (pane.scrollTop >= maxTop - 1) {
      return { index: metric.tops.length - 1, progress: 1 };
    }

    const firstTop = metric.tops[0] || 0;
    if (pane.scrollTop <= firstTop) {
      const leadRatio = firstTop > 0 ? pane.scrollTop / firstTop : 0;
      return {
        index: 0,
        progress: 0,
        leadRatio: Math.max(0, Math.min(leadRatio, 1)),
      };
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
    const maxTop = metric.maxTop || Math.max(pane.scrollHeight - pane.clientHeight, 0);

    if (typeof state.leadRatio === "number") {
      const firstTop = metric.tops[0] || 0;
      const leadTop = firstTop * Math.max(0, Math.min(state.leadRatio, 1));
      return Math.max(0, Math.min(leadTop, maxTop));
    }

    const safeIndex = Math.max(0, Math.min(state.index, metric.tops.length - 1));
    const start = metric.tops[safeIndex];
    const span = metric.spans[safeIndex] || 1;
    const top = start + span * state.progress;
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
    updateTOCActive();
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

    const sourceMetric = metrics[sourcePaneId];
    const targetMetric = metrics[targetPaneId];
    if (sourceMetric && targetMetric) {
      const sourceMaxTop = sourceMetric.maxTop || Math.max(sourcePane.scrollHeight - sourcePane.clientHeight, 0);
      const targetMaxTop = targetMetric.maxTop || Math.max(targetPane.scrollHeight - targetPane.clientHeight, 0);

      if (sourcePane.scrollTop >= sourceMaxTop - 1) {
        if (Math.abs(targetPane.scrollTop - targetMaxTop) > 0.5) {
          markSuppressed(targetPaneId, 72);
          targetPane.scrollTop = targetMaxTop;
        }
        return;
      }
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

  if (tocToggle) {
    tocToggle.addEventListener("click", function () {
      applyTOCState(!tocCollapsed);
      window.requestAnimationFrame(function () {
        scheduleSyncFrom(activePaneId);
      });
    });
  }

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
  applyTOCState(false);
})();
</script>
`)
	_, _ = w.WriteString("</body>\n</html>\n")
}
