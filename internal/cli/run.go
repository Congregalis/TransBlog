package cli

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/hex"
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
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/joho/godotenv"
	"golang.org/x/term"
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
	cacheFileName           = ".transblog.cache.json"
	defaultEnvFileName      = ".env"

	errorTypeFetch     = "fetch_failed"
	errorTypeConvert   = "convert_failed"
	errorTypeTranslate = "translate_failed"
	errorTypeOutput    = "output_failed"
	errorTypeUnknown   = "unknown"

	defaultQualityRetries = 1
)

var cliInput io.Reader = os.Stdin
var nowFunc = time.Now

type options struct {
	Model        string
	OutPath      string
	DefaultOut   string
	View         bool
	GenerateBoth bool
	ShowVersion  bool
	ConfigPath   string
	Workers      int
	MaxRetries   int
	FailFast     bool
	PriceConfig  string
	Glossary     string
	Refresh      bool
	Timeout      time.Duration
	ChunkSize    int
	ShowHelp     bool
	SourceURLs   []string
	setFlags     map[string]bool
}

type outputPlan struct {
	outputRoot string
	outputDir  string
	singleFile string
	summaryDir string
	stateDir   string
	batchID    string
	batchDate  string
	namer      *outputNamer
}

type outputNamer struct {
	usedBases map[string]struct{}
}

type summaryItem struct {
	SourceURL            string  `json:"source_url"`
	FinalURL             string  `json:"final_url,omitempty"`
	Success              bool    `json:"success"`
	DurationMS           int64   `json:"duration_ms"`
	OutputPath           string  `json:"output_path,omitempty"`
	HTMLOutputPath       string  `json:"html_output_path,omitempty"`
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
	BatchID                   string        `json:"batch_id,omitempty"`
	BatchDate                 string        `json:"batch_date,omitempty"`
	Labels                    []string      `json:"labels,omitempty"`
	RetryableFailedURLs       []string      `json:"retry_failed_urls,omitempty"`
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
	htmlOutputPath       string
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

func (o options) flagSet(name string) bool {
	if o.setFlags == nil {
		return false
	}
	return o.setFlags[name]
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

type translationCacheState struct {
	Version   int                              `json:"version"`
	UpdatedAt string                           `json:"updated_at"`
	Entries   map[string]translationCacheEntry `json:"entries"`
}

type translationCacheEntry struct {
	SourceURL            string       `json:"source_url"`
	FinalURL             string       `json:"final_url,omitempty"`
	SourceTitle          string       `json:"source_title,omitempty"`
	ChunkCount           int          `json:"chunk_count"`
	QualityFallbackCount int          `json:"quality_fallback_count,omitempty"`
	Usage                usageStats   `json:"usage"`
	Pairs                []cachedPair `json:"pairs"`
	ConfigHash           string       `json:"config_hash"`
	CachedAt             string       `json:"cached_at"`
}

type cachedPair struct {
	SourceURL   string `json:"source_url"`
	SourceTitle string `json:"source_title,omitempty"`
	Source      string `json:"source"`
	Translated  string `json:"translated"`
}

type cacheStore struct {
	path  string
	state translationCacheState
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

	envPath := resolveEnvPath(opts.ConfigPath)
	envValues, err := loadLocalEnv(envPath)
	if err != nil {
		return err
	}
	applyEnvDefaults(&opts, envValues)

	apiKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	opts.SourceURLs = normalizeSourceURLs(opts.SourceURLs)
	if apiKey == "" || len(opts.SourceURLs) == 0 {
		opts, apiKey, err = runOnboarding(opts, envPath, envValues, apiKey, stdout, stderr)
		if err != nil {
			return err
		}
	}
	if apiKey == "" {
		return errors.New(buildMissingAPIKeyGuidance(envPath, opts))
	}
	if len(opts.SourceURLs) == 0 {
		return errors.New("at least one URL is required")
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
	if strings.TrimSpace(outPlan.stateDir) != "" {
		statePath = filepath.Join(outPlan.stateDir, stateFileName)
	}
	stateStore, err := loadResumeStore(statePath)
	if err != nil {
		return err
	}

	cachePath := filepath.Join(outPlan.summaryDir, cacheFileName)
	if strings.TrimSpace(outPlan.stateDir) != "" {
		cachePath = filepath.Join(outPlan.stateDir, cacheFileName)
	}
	cache, err := loadCacheStore(cachePath)
	if err != nil {
		return err
	}

	openAIClient := openai.NewClient(apiKey, baseURL, httpClient, opts.MaxRetries)
	runCtx, stopSignal := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignal()
	runStart := time.Now()

	summary := taskSummary{
		GeneratedAt: optsStartTime(runStart),
		BatchID:     outPlan.batchID,
		BatchDate:   outPlan.batchDate,
		Labels:      buildSummaryLabels(opts),
		Model:       opts.Model,
		View:        opts.View,
		TotalURLs:   len(opts.SourceURLs),
		Results:     make([]summaryItem, 0, len(opts.SourceURLs)),
	}

	for _, sourceURL := range opts.SourceURLs {
		itemStart := time.Now()
		item := summaryItem{SourceURL: sourceURL}

		output, err := processURL(runCtx, httpClient, openAIClient, opts, glossaryMap, prices, sourceURL, outPlan, stateStore, cache, stderr)
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
			summary.RetryableFailedURLs = append(summary.RetryableFailedURLs, sourceURL)
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
		item.HTMLOutputPath = output.htmlOutputPath
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
		if output.htmlOutputPath != "" {
			_, _ = fmt.Fprintf(stdout, "Output (HTML): %s\n", output.htmlOutputPath)
		}
	}

	summary.TotalDurationMS = time.Since(runStart).Milliseconds()
	summary.RetryableFailedURLs = uniqueStrings(summary.RetryableFailedURLs)

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
	fs.StringVar(&opts.ConfigPath, "config", "", "Env file path (default: ./.env)")
	fs.IntVar(&opts.Workers, "workers", defaultTranslateWorkers, "Translation worker count")
	fs.IntVar(&opts.MaxRetries, "max-retries", defaultMaxRetries, "Maximum retries for OpenAI requests")
	fs.BoolVar(&opts.FailFast, "fail-fast", false, "Stop at first URL failure (default: continue for partial success)")
	fs.BoolVar(&opts.Refresh, "refresh", false, "Ignore cache and force refetch/retranslate")
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
	opts.setFlags = map[string]bool{}
	fs.Visit(func(f *flag.Flag) {
		opts.setFlags[f.Name] = true
	})

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
		outDir := defaultOutDir
		if configured := strings.TrimSpace(opts.DefaultOut); configured != "" {
			outDir = configured
		}
		return buildBatchOutputPlan(outDir, opts.SourceURLs), nil
	}

	if len(opts.SourceURLs) == 1 {
		stateDir := filepath.Dir(opts.OutPath)
		if stateDir == "." {
			stateDir = ""
		}
		return outputPlan{
			singleFile: opts.OutPath,
			summaryDir: filepath.Dir(opts.OutPath),
			stateDir:   stateDir,
		}, nil
	}

	return buildBatchOutputPlan(opts.OutPath, opts.SourceURLs), nil
}

func buildBatchOutputPlan(baseOutputDir string, sourceURLs []string) outputPlan {
	base := strings.TrimSpace(baseOutputDir)
	if base == "" {
		base = defaultOutDir
	}

	now := nowFunc()
	batchDate := now.Format("2006-01-02")
	baseBatchID := buildBatchID(now, sourceURLs)
	batchID := baseBatchID
	batchDir := filepath.Join(base, batchDate, batchID)
	sequence := 2
	for pathExists(batchDir) {
		batchID = fmt.Sprintf("%s-%d", baseBatchID, sequence)
		batchDir = filepath.Join(base, batchDate, batchID)
		sequence++
	}

	return outputPlan{
		outputRoot: base,
		outputDir:  batchDir,
		summaryDir: batchDir,
		stateDir:   base,
		batchID:    batchID,
		batchDate:  batchDate,
		namer:      &outputNamer{usedBases: map[string]struct{}{}},
	}
}

func buildBatchID(now time.Time, sourceURLs []string) string {
	timePart := now.Format("15-04-05")
	label := batchLabelFromURLs(sourceURLs)
	return timePart + "_" + label
}

func batchLabelFromURLs(sourceURLs []string) string {
	if len(sourceURLs) == 0 {
		return "run"
	}

	first := slugFromURLPath(sourceURLs[0])
	if first == "" {
		first = "run"
	}
	if len(sourceURLs) == 1 {
		return first
	}

	extra := len(sourceURLs) - 1
	label := fmt.Sprintf("%s-and-%d-more", first, extra)
	return trimBaseLength(label, 64)
}

func slugFromURLPath(rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "run"
	}

	pathPart := strings.TrimSpace(parsed.Path)
	if pathPart == "" || pathPart == "/" {
		return slugifyBatchLabel(parsed.Host)
	}

	segments := strings.Split(strings.Trim(pathPart, "/"), "/")
	for i := len(segments) - 1; i >= 0; i-- {
		part := strings.TrimSpace(segments[i])
		if part == "" {
			continue
		}
		part = strings.TrimSuffix(part, filepath.Ext(part))
		if slug := slugifyBatchLabel(part); slug != "" {
			return slug
		}
	}

	return slugifyBatchLabel(parsed.Host)
}

func slugifyBatchLabel(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if normalized == "" {
		return ""
	}

	var b strings.Builder
	lastDash := false

	for _, r := range normalized {
		allowed := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if allowed {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
	}

	out := strings.Trim(b.String(), "-")
	if out == "" {
		return ""
	}
	return trimBaseLength(out, 64)
}

func pathExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
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

	if plan.stateDir != "" && plan.stateDir != "." {
		if err := os.MkdirAll(plan.stateDir, 0o755); err != nil {
			return fmt.Errorf("create state directory: %w", err)
		}
	}

	return nil
}

func resolveEnvPath(rawPath string) string {
	trimmed := strings.TrimSpace(rawPath)
	if trimmed == "" {
		return defaultEnvFileName
	}
	return trimmed
}

func loadLocalEnv(path string) (map[string]string, error) {
	envPath := resolveEnvPath(path)
	if _, err := os.Stat(envPath); errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	} else if err != nil {
		return nil, fmt.Errorf("check env file %s: %w", envPath, err)
	}

	if err := godotenv.Load(envPath); err != nil {
		return nil, fmt.Errorf("load env file %s: %w", envPath, err)
	}

	values, err := godotenv.Read(envPath)
	if err != nil {
		return nil, fmt.Errorf("parse env file %s: %w", envPath, err)
	}

	return values, nil
}

func ensureEnvFile(path string) error {
	envPath := resolveEnvPath(path)
	if _, err := os.Stat(envPath); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check env file %s: %w", envPath, err)
	}

	dir := filepath.Dir(envPath)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create env directory %s: %w", dir, err)
		}
	}

	defaultOut := strings.TrimSpace(defaultOutDir)
	values := map[string]string{
		"TRANSBLOG_MODEL":   "gpt-5.2",
		"TRANSBLOG_WORKERS": strconv.Itoa(defaultTranslateWorkers),
		"TRANSBLOG_OUT_DIR": defaultOut,
	}
	if err := godotenv.Write(values, envPath); err != nil {
		return fmt.Errorf("write env file %s: %w", envPath, err)
	}
	return nil
}

func runOnboarding(opts options, envPath string, envValues map[string]string, apiKey string, stdout io.Writer, stderr io.Writer) (options, string, error) {
	reader := bufio.NewReader(cliInput)
	if err := ensureEnvFile(envPath); err != nil {
		return opts, apiKey, err
	}

	mergedEnv := cloneEnvMap(envValues)
	if mergedEnv == nil {
		mergedEnv = map[string]string{}
	}
	changed := false

	if strings.TrimSpace(mergedEnv["TRANSBLOG_MODEL"]) == "" {
		mergedEnv["TRANSBLOG_MODEL"] = opts.Model
		changed = true
	}
	if strings.TrimSpace(mergedEnv["TRANSBLOG_WORKERS"]) == "" {
		mergedEnv["TRANSBLOG_WORKERS"] = strconv.Itoa(opts.Workers)
		changed = true
	}
	if strings.TrimSpace(mergedEnv["TRANSBLOG_OUT_DIR"]) == "" {
		outDir := strings.TrimSpace(opts.DefaultOut)
		if outDir == "" {
			outDir = defaultOutDir
		}
		mergedEnv["TRANSBLOG_OUT_DIR"] = outDir
		changed = true
	}
	if strings.TrimSpace(mergedEnv["OPENAI_API_KEY"]) == "" && strings.TrimSpace(apiKey) != "" {
		mergedEnv["OPENAI_API_KEY"] = strings.TrimSpace(apiKey)
		changed = true
	}
	if strings.TrimSpace(mergedEnv["OPENAI_BASE_URL"]) == "" {
		if baseURL := strings.TrimSpace(os.Getenv("OPENAI_BASE_URL")); baseURL != "" {
			mergedEnv["OPENAI_BASE_URL"] = baseURL
			changed = true
		}
	}

	if strings.TrimSpace(apiKey) == "" {
		_, _ = fmt.Fprintln(stderr, "OPENAI_API_KEY is not set.")
		key, err := promptSecretInput(reader, stdout, "Paste OPENAI_API_KEY: ")
		if err != nil {
			return opts, "", err
		}
		apiKey = strings.TrimSpace(key)
		mergedEnv["OPENAI_API_KEY"] = apiKey
		changed = true
		_ = os.Setenv("OPENAI_API_KEY", apiKey)
	}

	if !opts.flagSet("view") {
		opts.GenerateBoth = true
		_, _ = fmt.Fprintln(stdout, "Output mode: Markdown + HTML (default onboarding mode).")
	}

	reviewChanged, err := reviewOnboardingConfig(reader, stdout, mergedEnv)
	if err != nil {
		return opts, apiKey, err
	}
	if reviewChanged {
		changed = true
	}

	applyEnvDefaults(&opts, mergedEnv)
	resolvedAPIKey := strings.TrimSpace(mergedEnv["OPENAI_API_KEY"])
	if resolvedAPIKey == "" {
		resolvedAPIKey = strings.TrimSpace(apiKey)
	}
	apiKey = resolvedAPIKey
	if resolvedAPIKey != "" {
		_ = os.Setenv("OPENAI_API_KEY", resolvedAPIKey)
	}
	if baseURL, ok := mergedEnv["OPENAI_BASE_URL"]; ok {
		_ = os.Setenv("OPENAI_BASE_URL", strings.TrimSpace(baseURL))
	}

	if changed {
		if err := godotenv.Write(mergedEnv, envPath); err != nil {
			return opts, apiKey, fmt.Errorf("write env file %s: %w", envPath, err)
		}
		_, _ = fmt.Fprintf(stdout, "Updated env file: %s\n", envPath)
	}

	if len(opts.SourceURLs) == 0 {
		_, _ = fmt.Fprintln(stdout, "No URL provided.")
		entered, err := promptRequiredInput(reader, stdout, "Paste URL(s) (space/comma/newline separated): ")
		if err != nil {
			return opts, apiKey, err
		}
		urls := parseEnteredURLs(entered)
		if len(urls) == 0 {
			return opts, apiKey, errors.New("at least one URL is required")
		}
		opts.SourceURLs = urls
	}

	return opts, apiKey, nil
}

func cloneEnvMap(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}

	dst := make(map[string]string, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func promptRequiredInput(reader *bufio.Reader, out io.Writer, prompt string) (string, error) {
	for {
		line, err := promptOptionalInput(reader, out, prompt)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(line) != "" {
			return strings.TrimSpace(line), nil
		}
	}
}

func promptOptionalInput(reader *bufio.Reader, out io.Writer, prompt string) (string, error) {
	_, _ = io.WriteString(out, prompt)
	line, err := reader.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) {
			return strings.TrimSpace(line), nil
		}
		return "", fmt.Errorf("read input: %w", err)
	}
	return strings.TrimSpace(line), nil
}

func promptSecretInput(reader *bufio.Reader, out io.Writer, prompt string) (string, error) {
	inputFile, ok := cliInput.(*os.File)
	if !ok || inputFile != os.Stdin || !term.IsTerminal(int(inputFile.Fd())) {
		return promptRequiredInput(reader, out, prompt)
	}

	for {
		_, _ = io.WriteString(out, prompt)
		state, err := term.MakeRaw(int(inputFile.Fd()))
		if err != nil {
			return promptRequiredInput(reader, out, prompt)
		}

		secret, readErr := readMaskedSecret(inputFile, out)
		_ = term.Restore(int(inputFile.Fd()), state)
		if readErr != nil {
			return "", readErr
		}

		secret = strings.TrimSpace(secret)
		if secret != "" {
			return secret, nil
		}
	}
}

func readMaskedSecret(input *os.File, out io.Writer) (string, error) {
	var buffer []byte
	byteBuf := make([]byte, 1)

	for {
		_, err := input.Read(byteBuf)
		if err != nil {
			if errors.Is(err, io.EOF) {
				_, _ = io.WriteString(out, "\n")
				return strings.TrimSpace(string(buffer)), nil
			}
			return "", fmt.Errorf("read input: %w", err)
		}

		b := byteBuf[0]
		switch b {
		case '\r', '\n':
			_, _ = io.WriteString(out, "\n")
			return strings.TrimSpace(string(buffer)), nil
		case 127, 8:
			if len(buffer) > 0 {
				buffer = buffer[:len(buffer)-1]
				_, _ = io.WriteString(out, "\b \b")
			}
		case 3:
			return "", errors.New("input cancelled")
		default:
			if b >= 32 && b <= 126 {
				buffer = append(buffer, b)
				_, _ = io.WriteString(out, "*")
			}
		}
	}
}

func parseEnteredURLs(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	replacer := strings.NewReplacer(",", " ", "\n", " ", "\t", " ")
	normalized := replacer.Replace(raw)
	return normalizeSourceURLs(strings.Fields(normalized))
}

func reviewOnboardingConfig(reader *bufio.Reader, out io.Writer, env map[string]string) (bool, error) {
	if env == nil {
		return false, nil
	}

	printConfigSnapshot(out, env)
	answer, err := promptOptionalInput(reader, out, "Edit config before start? [y/N]: ")
	if err != nil {
		return false, err
	}
	if !isYes(answer) {
		return false, nil
	}

	changed := false
	for {
		printConfigSnapshot(out, env)
		choice, err := promptOptionalInput(reader, out, "Choose item to edit [1-5, Enter to continue]: ")
		if err != nil {
			return changed, err
		}
		if strings.TrimSpace(choice) == "" {
			return changed, nil
		}

		switch strings.TrimSpace(choice) {
		case "1":
			secret, err := promptSecretInput(reader, out, "OPENAI_API_KEY: ")
			if err != nil {
				return changed, err
			}
			env["OPENAI_API_KEY"] = strings.TrimSpace(secret)
			changed = true
		case "2":
			baseURL, err := promptOptionalInput(reader, out, "OPENAI_BASE_URL (empty to clear): ")
			if err != nil {
				return changed, err
			}
			env["OPENAI_BASE_URL"] = strings.TrimSpace(baseURL)
			changed = true
		case "3":
			model, err := promptRequiredInput(reader, out, "TRANSBLOG_MODEL: ")
			if err != nil {
				return changed, err
			}
			env["TRANSBLOG_MODEL"] = strings.TrimSpace(model)
			changed = true
		case "4":
			workersRaw, err := promptRequiredInput(reader, out, "TRANSBLOG_WORKERS: ")
			if err != nil {
				return changed, err
			}
			workers, convErr := strconv.Atoi(strings.TrimSpace(workersRaw))
			if convErr != nil || workers <= 0 {
				_, _ = fmt.Fprintln(out, "Workers must be a positive integer.")
				continue
			}
			env["TRANSBLOG_WORKERS"] = strconv.Itoa(workers)
			changed = true
		case "5":
			outDir, err := promptRequiredInput(reader, out, "TRANSBLOG_OUT_DIR: ")
			if err != nil {
				return changed, err
			}
			env["TRANSBLOG_OUT_DIR"] = strings.TrimSpace(outDir)
			changed = true
		default:
			_, _ = fmt.Fprintln(out, "Invalid selection. Choose 1-5 or press Enter.")
		}
	}
}

func printConfigSnapshot(out io.Writer, env map[string]string) {
	_, _ = fmt.Fprintln(out, "Current config:")
	_, _ = fmt.Fprintf(out, "  1) OPENAI_API_KEY=%s\n", maskSecret(env["OPENAI_API_KEY"]))
	_, _ = fmt.Fprintf(out, "  2) OPENAI_BASE_URL=%s\n", displayValue(env["OPENAI_BASE_URL"]))
	_, _ = fmt.Fprintf(out, "  3) TRANSBLOG_MODEL=%s\n", displayValue(env["TRANSBLOG_MODEL"]))
	_, _ = fmt.Fprintf(out, "  4) TRANSBLOG_WORKERS=%s\n", displayValue(env["TRANSBLOG_WORKERS"]))
	_, _ = fmt.Fprintf(out, "  5) TRANSBLOG_OUT_DIR=%s\n", displayValue(env["TRANSBLOG_OUT_DIR"]))
}

func displayValue(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "(empty)"
	}
	return trimmed
}

func maskSecret(secret string) string {
	trimmed := strings.TrimSpace(secret)
	if trimmed == "" {
		return "(empty)"
	}
	if len(trimmed) <= 4 {
		return strings.Repeat("*", len(trimmed))
	}
	return strings.Repeat("*", len(trimmed)-4) + trimmed[len(trimmed)-4:]
}

func isYes(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

func applyEnvDefaults(opts *options, envValues map[string]string) {
	if opts == nil {
		return
	}

	if !opts.flagSet("model") {
		if model := strings.TrimSpace(envValues["TRANSBLOG_MODEL"]); model != "" {
			opts.Model = model
		}
	}

	if !opts.flagSet("workers") {
		if workersRaw := strings.TrimSpace(envValues["TRANSBLOG_WORKERS"]); workersRaw != "" {
			if workers, err := strconv.Atoi(workersRaw); err == nil && workers > 0 {
				opts.Workers = workers
			}
		}
	}

	if !opts.flagSet("out") {
		if outDir := strings.TrimSpace(envValues["TRANSBLOG_OUT_DIR"]); outDir != "" {
			opts.DefaultOut = outDir
		}
	}
}

func buildMissingAPIKeyGuidance(envPath string, opts options) string {
	outPath := strings.TrimSpace(opts.OutPath)
	if outPath == "" {
		outPath = strings.TrimSpace(opts.DefaultOut)
	}
	if outPath == "" {
		outPath = defaultOutDir
	}

	return strings.Join([]string{
		"OPENAI_API_KEY is required.",
		"",
		"First-time setup:",
		"  1) export OPENAI_API_KEY=\"sk-...\"",
		fmt.Sprintf("  2) (optional) edit %s to tune TRANSBLOG_MODEL/TRANSBLOG_WORKERS/TRANSBLOG_OUT_DIR", envPath),
		"  3) run: transblog <url>",
		"",
		"Current defaults:",
		fmt.Sprintf("  - model: %s", opts.Model),
		fmt.Sprintf("  - workers: %d", opts.Workers),
		fmt.Sprintf("  - out: %s", outPath),
	}, "\n")
}

func buildSummaryLabels(opts options) []string {
	mode := resolveWriteMode(opts)
	labels := []string{
		"model:" + strings.TrimSpace(opts.Model),
		"mode:" + mode,
		fmt.Sprintf("workers:%d", opts.Workers),
		fmt.Sprintf("refresh:%t", opts.Refresh),
	}
	return labels
}

func uniqueStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	return out
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

func loadCacheStore(path string) (*cacheStore, error) {
	store := &cacheStore{
		path: path,
		state: translationCacheState{
			Version: 1,
			Entries: map[string]translationCacheEntry{},
		},
	}

	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read cache file %s: %w", path, err)
	}
	if strings.TrimSpace(string(content)) == "" {
		return store, nil
	}

	if err := json.Unmarshal(content, &store.state); err != nil {
		return nil, fmt.Errorf("parse cache file %s: %w", path, err)
	}
	if store.state.Version == 0 {
		store.state.Version = 1
	}
	if store.state.Entries == nil {
		store.state.Entries = map[string]translationCacheEntry{}
	}
	return store, nil
}

func (s *cacheStore) get(cacheKey string) (translationCacheEntry, bool) {
	if s == nil {
		return translationCacheEntry{}, false
	}
	entry, ok := s.state.Entries[cacheKey]
	if !ok {
		return translationCacheEntry{}, false
	}
	if len(entry.Pairs) == 0 {
		return translationCacheEntry{}, false
	}
	return entry, true
}

func (s *cacheStore) set(cacheKey string, entry translationCacheEntry) error {
	if s == nil {
		return nil
	}
	if strings.TrimSpace(cacheKey) == "" {
		return nil
	}
	if s.state.Entries == nil {
		s.state.Entries = map[string]translationCacheEntry{}
	}

	entry.CachedAt = time.Now().UTC().Format(time.RFC3339)
	s.state.Entries[cacheKey] = entry
	return s.persist()
}

func (s *cacheStore) persist() error {
	if s == nil {
		return nil
	}

	s.state.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	payload, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal cache state: %w", err)
	}
	payload = append(payload, '\n')

	tmpPath := s.path + ".tmp"
	if err := os.WriteFile(tmpPath, payload, 0o644); err != nil {
		return fmt.Errorf("write cache temp file %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("replace cache file %s: %w", s.path, err)
	}
	return nil
}

func buildCacheConfigHash(opts options, glossaryMap map[string]string) string {
	keys := make([]string, 0, len(glossaryMap))
	for key := range glossaryMap {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	glossaryPairs := make([][2]string, 0, len(keys))
	for _, key := range keys {
		glossaryPairs = append(glossaryPairs, [2]string{key, glossaryMap[key]})
	}

	payload := struct {
		Model     string      `json:"model"`
		ChunkSize int         `json:"chunk_size"`
		Glossary  [][2]string `json:"glossary"`
		Version   string      `json:"version"`
	}{
		Model:     strings.TrimSpace(opts.Model),
		ChunkSize: opts.ChunkSize,
		Glossary:  glossaryPairs,
		Version:   version.String(),
	}

	return sha1Text(mustJSON(payload))
}

func buildURLCacheKey(sourceURL string, configHash string) string {
	trimmed := strings.TrimSpace(sourceURL)
	return sha1Text(trimmed + "|" + strings.TrimSpace(configHash))
}

func sha1Text(text string) string {
	sum := sha1.Sum([]byte(text))
	return hex.EncodeToString(sum[:])
}

func mustJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

func cachedPairsToPairs(cached []cachedPair) []pair {
	out := make([]pair, 0, len(cached))
	for _, item := range cached {
		out = append(out, pair{
			sourceURL:   item.SourceURL,
			sourceTitle: item.SourceTitle,
			source:      item.Source,
			translated:  item.Translated,
		})
	}
	return out
}

func pairsToCachedPairs(items []pair) []cachedPair {
	out := make([]cachedPair, 0, len(items))
	for _, item := range items {
		out = append(out, cachedPair{
			SourceURL:   item.sourceURL,
			SourceTitle: item.sourceTitle,
			Source:      item.source,
			Translated:  item.translated,
		})
	}
	return out
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
	cache *cacheStore,
	progress io.Writer,
) (urlOutput, error) {
	urlStart := time.Now()
	cacheConfigHash := buildCacheConfigHash(opts, glossaryMap)
	cacheKey := buildURLCacheKey(sourceURL, cacheConfigHash)

	if !opts.Refresh {
		if cachedEntry, ok := cache.get(cacheKey); ok {
			cachedPairs := cachedPairsToPairs(cachedEntry.Pairs)
			if len(cachedPairs) > 0 {
				_, _ = fmt.Fprintf(progress, "Cache hit: reused %d chunks for %s\n", len(cachedPairs), compactURL(sourceURL))

				meta := outputMetadata{
					Title:      cachedEntry.SourceTitle,
					Lang:       "zh-CN",
					ChunkCount: cachedEntry.ChunkCount,
					Duration:   time.Since(urlStart),
					Usage:      cachedEntry.Usage,
				}
				targetURL := strings.TrimSpace(cachedEntry.FinalURL)
				if targetURL == "" {
					targetURL = sourceURL
				}
				markdownPath, htmlPath, err := writeOutputsForPairs(cachedPairs, opts, outPlan, targetURL, meta)
				if err != nil {
					cost, costEstimated, costPartial := estimateCost(cachedEntry.Usage, prices, opts.Model)
					return urlOutput{}, newProcessErrorWithDetails(errorTypeOutput, err, cachedEntry.QualityFallbackCount, cachedEntry.Usage, cost, costEstimated, costPartial)
				}

				if err := stateStore.markURLComplete(sourceURL); err != nil {
					cost, costEstimated, costPartial := estimateCost(cachedEntry.Usage, prices, opts.Model)
					return urlOutput{}, newProcessErrorWithDetails(errorTypeOutput, fmt.Errorf("finalize resume state for %s: %w", sourceURL, err), cachedEntry.QualityFallbackCount, cachedEntry.Usage, cost, costEstimated, costPartial)
				}

				cost, costEstimated, _ := estimateCost(cachedEntry.Usage, prices, opts.Model)
				mainOutputPath := markdownPath
				if mainOutputPath == "" {
					mainOutputPath = htmlPath
				}

				finalURL := strings.TrimSpace(cachedEntry.FinalURL)
				if finalURL == "" {
					finalURL = sourceURL
				}

				return urlOutput{
					finalURL:             finalURL,
					outputPath:           mainOutputPath,
					htmlOutputPath:       htmlPath,
					qualityFallbackCount: cachedEntry.QualityFallbackCount,
					usage:                cachedEntry.Usage,
					costEstimate:         cost,
					costEstimated:        costEstimated,
				}, nil
			}
		}
	}

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

	cost, costEstimated, costPartial := estimateCost(urlUsage, prices, opts.Model)
	meta := outputMetadata{
		Title:      sourceDoc.title,
		Lang:       "zh-CN",
		ChunkCount: len(pairs),
		Duration:   time.Since(urlStart),
		Usage:      urlUsage,
	}
	markdownPath, htmlPath, err := writeOutputsForPairs(pairs, perURLOpts, outPlan, taskSourceURL, meta)
	if err != nil {
		return urlOutput{}, newProcessErrorWithDetails(errorTypeOutput, err, urlQualityFallbackCount, urlUsage, cost, costEstimated, costPartial)
	}

	if err := cache.set(cacheKey, translationCacheEntry{
		SourceURL:            sourceURL,
		FinalURL:             taskSourceURL,
		SourceTitle:          sourceDoc.title,
		ChunkCount:           len(pairs),
		QualityFallbackCount: urlQualityFallbackCount,
		Usage:                urlUsage,
		Pairs:                pairsToCachedPairs(pairs),
		ConfigHash:           cacheConfigHash,
	}); err != nil {
		_, _ = fmt.Fprintf(progress, "Cache warning: failed to persist for %s (%v)\n", compactURL(taskSourceURL), err)
	}

	if err := stateStore.markURLComplete(sourceURL); err != nil {
		return urlOutput{}, newProcessErrorWithDetails(errorTypeOutput, fmt.Errorf("finalize resume state for %s: %w", sourceURL, err), urlQualityFallbackCount, urlUsage, cost, costEstimated, costPartial)
	}

	mainOutputPath := markdownPath
	if mainOutputPath == "" {
		mainOutputPath = htmlPath
	}

	return urlOutput{
		finalURL:             taskSourceURL,
		outputPath:           mainOutputPath,
		htmlOutputPath:       htmlPath,
		qualityFallbackCount: urlQualityFallbackCount,
		usage:                urlUsage,
		costEstimate:         cost,
		costEstimated:        costEstimated,
	}, nil
}

type outputPaths struct {
	markdown string
	html     string
}

type outputMetadata struct {
	Title      string
	Lang       string
	ChunkCount int
	Duration   time.Duration
	Usage      usageStats
}

func resolveWriteMode(opts options) string {
	if opts.GenerateBoth {
		return "both"
	}
	if opts.View {
		return "html"
	}
	return "markdown"
}

func writeOutputsForPairs(
	pairs []pair,
	opts options,
	outPlan outputPlan,
	sourceURL string,
	meta outputMetadata,
) (string, string, error) {
	if meta.ChunkCount <= 0 {
		meta.ChunkCount = len(pairs)
	}
	if strings.TrimSpace(meta.Lang) == "" {
		meta.Lang = "zh-CN"
	}

	writeMode := resolveWriteMode(opts)
	outputPaths, err := outputPathsForURL(outPlan, meta.Title, sourceURL)
	if err != nil {
		return "", "", err
	}

	var markdownPath string
	var htmlPath string

	if writeMode == "markdown" || writeMode == "both" {
		var markdownOutput strings.Builder
		writeMarkdown(&markdownOutput, pairs, opts, meta)

		markdownPath = outputPaths.markdown
		if err := os.WriteFile(markdownPath, []byte(markdownOutput.String()), 0o644); err != nil {
			return "", "", fmt.Errorf("write output file %s: %w", markdownPath, err)
		}
	}

	if writeMode == "html" || writeMode == "both" {
		var htmlOutput strings.Builder
		writeHTMLView(&htmlOutput, pairs, opts, meta)

		htmlPath = outputPaths.html
		if err := os.WriteFile(htmlPath, []byte(htmlOutput.String()), 0o644); err != nil {
			return "", "", fmt.Errorf("write output file %s: %w", htmlPath, err)
		}
	}

	return markdownPath, htmlPath, nil
}

func outputPathsForURL(plan outputPlan, title string, sourceURL string) (outputPaths, error) {
	if plan.singleFile != "" {
		return outputPaths{
			markdown: plan.singleFile,
			html:     htmlCompanionPath(plan.singleFile),
		}, nil
	}

	baseName, err := reserveOutputBaseName(plan, title, sourceURL)
	if err != nil {
		return outputPaths{}, err
	}

	return outputPaths{
		markdown: filepath.Join(plan.outputDir, baseName+".md"),
		html:     filepath.Join(plan.outputDir, baseName+".html"),
	}, nil
}

func reserveOutputBaseName(plan outputPlan, title string, sourceURL string) (string, error) {
	preferred := filenameBaseFromTitle(title)
	fallback, err := filenameBaseFromURL(sourceURL)
	if err != nil && preferred == "" {
		return "", err
	}
	if preferred == "" {
		preferred = fallback
	}
	if strings.TrimSpace(preferred) == "" {
		preferred = "output"
	}

	if plan.namer == nil {
		plan.namer = &outputNamer{usedBases: map[string]struct{}{}}
	}
	return plan.namer.reserve(plan.outputDir, preferred), nil
}

func (n *outputNamer) reserve(outputDir string, desiredBase string) string {
	base := strings.TrimSpace(desiredBase)
	if base == "" {
		base = "output"
	}

	if n.usedBases == nil {
		n.usedBases = map[string]struct{}{}
	}

	base = trimBaseLength(base, 80)
	if base == "" {
		base = "output"
	}

	for suffix := 1; ; suffix++ {
		candidate := base
		if suffix > 1 {
			candidate = withNumericSuffix(base, suffix)
		}

		key := strings.ToLower(candidate)
		if _, exists := n.usedBases[key]; exists {
			continue
		}
		if pathExists(filepath.Join(outputDir, candidate+".md")) || pathExists(filepath.Join(outputDir, candidate+".html")) {
			continue
		}

		n.usedBases[key] = struct{}{}
		return candidate
	}
}

func withNumericSuffix(base string, suffix int) string {
	suffixText := fmt.Sprintf("-%d", suffix)
	maxBaseLen := 80 - len(suffixText)
	if maxBaseLen < 1 {
		maxBaseLen = 1
	}
	return trimBaseLength(base, maxBaseLen) + suffixText
}

func trimBaseLength(value string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return strings.Trim(value, "-_.")
	}
	return strings.Trim(string(runes[:maxRunes]), "-_.")
}

func htmlCompanionPath(path string) string {
	ext := filepath.Ext(path)
	if strings.EqualFold(ext, ".html") {
		return path
	}
	if ext == "" {
		return path + ".html"
	}
	return strings.TrimSuffix(path, ext) + ".html"
}

func filenameFromURL(rawURL string, view bool) (string, error) {
	base, err := filenameBaseFromURL(rawURL)
	if err != nil {
		return "", err
	}

	ext := ".md"
	if view {
		ext = ".html"
	}
	return base + ext, nil
}

func filenameBaseFromURL(rawURL string) (string, error) {
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

	return base, nil
}

func filenameBaseFromTitle(title string) string {
	trimmed := strings.TrimSpace(title)
	if trimmed == "" {
		return ""
	}

	var b strings.Builder
	lastSeparator := false

	for _, r := range trimmed {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			lastSeparator = false
		case r == '-' || r == '_' || r == '.':
			if b.Len() == 0 || lastSeparator {
				continue
			}
			b.WriteRune(r)
			lastSeparator = true
		default:
			if b.Len() == 0 || lastSeparator {
				continue
			}
			b.WriteByte('-')
			lastSeparator = true
		}
	}

	candidate := strings.Trim(b.String(), "-_.")
	return trimBaseLength(candidate, 80)
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

func writeFrontMatter(w io.StringWriter, opts options, meta outputMetadata) {
	now := time.Now().UTC().Format(time.RFC3339)
	_, _ = w.WriteString("---\n")
	_, _ = w.WriteString("title: ")
	_, _ = w.WriteString(yamlQuote(meta.Title))
	_, _ = w.WriteString("\n")
	_, _ = w.WriteString("lang: ")
	_, _ = w.WriteString(yamlQuote(meta.Lang))
	_, _ = w.WriteString("\n")
	_, _ = w.WriteString("chunk_count: ")
	_, _ = w.WriteString(strconv.Itoa(meta.ChunkCount))
	_, _ = w.WriteString("\n")
	_, _ = w.WriteString("duration: ")
	_, _ = w.WriteString(yamlQuote(meta.Duration.String()))
	_, _ = w.WriteString("\n")
	_, _ = w.WriteString("duration_ms: ")
	_, _ = w.WriteString(strconv.FormatInt(meta.Duration.Milliseconds(), 10))
	_, _ = w.WriteString("\n")
	_, _ = w.WriteString("token_usage:\n")
	_, _ = w.WriteString("  input: ")
	_, _ = w.WriteString(strconv.FormatInt(meta.Usage.inputTokens, 10))
	_, _ = w.WriteString("\n")
	_, _ = w.WriteString("  output: ")
	_, _ = w.WriteString(strconv.FormatInt(meta.Usage.outputTokens, 10))
	_, _ = w.WriteString("\n")
	_, _ = w.WriteString("  total: ")
	_, _ = w.WriteString(strconv.FormatInt(meta.Usage.totalTokens, 10))
	_, _ = w.WriteString("\n")
	_, _ = w.WriteString("  missing_count: ")
	_, _ = w.WriteString(strconv.Itoa(meta.Usage.missingUsageCount))
	_, _ = w.WriteString("\n")
	_, _ = w.WriteString("source_urls:\n")

	for _, sourceURL := range opts.SourceURLs {
		_, _ = w.WriteString("  - ")
		_, _ = w.WriteString(yamlQuote(sourceURL))
		_, _ = w.WriteString("\n")
	}

	_, _ = w.WriteString("generated_at: ")
	_, _ = w.WriteString(yamlQuote(now))
	_, _ = w.WriteString("\n")
	_, _ = w.WriteString("model: ")
	_, _ = w.WriteString(yamlQuote(opts.Model))
	_, _ = w.WriteString("\n")
	_, _ = w.WriteString("---\n\n")
}

func yamlQuote(value string) string {
	return strconv.Quote(strings.TrimSpace(value))
}

type pair struct {
	sourceURL   string
	sourceTitle string
	source      string
	translated  string
}

func writeMarkdown(w io.StringWriter, pairs []pair, opts options, meta outputMetadata) {
	if strings.TrimSpace(meta.Title) == "" && len(pairs) > 0 {
		meta.Title = pairs[0].sourceTitle
	}
	if strings.TrimSpace(meta.Lang) == "" {
		meta.Lang = "zh-CN"
	}
	if meta.ChunkCount <= 0 {
		meta.ChunkCount = len(pairs)
	}

	writeFrontMatter(w, opts, meta)

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

func writeHTMLView(w io.StringWriter, pairs []pair, opts options, meta outputMetadata) {
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
	if strings.TrimSpace(meta.Title) == "" && len(pairs) > 0 {
		meta.Title = pairs[0].sourceTitle
	}
	if strings.TrimSpace(meta.Title) == "" {
		meta.Title = "TransBlog"
	}
	if strings.TrimSpace(meta.Lang) == "" {
		meta.Lang = "zh-CN"
	}
	if meta.ChunkCount <= 0 {
		meta.ChunkCount = len(pairs)
	}
	durationText := meta.Duration.String()
	if strings.TrimSpace(durationText) == "" || durationText == "0s" {
		durationText = "-"
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
:root {
  color-scheme: light;
  --reader-font-size: 16px;
}
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
  font-size: var(--reader-font-size);
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
.markdown-body .heading-anchor {
  margin-left: 8px;
  font-size: 0.85em;
  opacity: 0;
  text-decoration: none;
}
.markdown-body h1:hover .heading-anchor,
.markdown-body h2:hover .heading-anchor {
  opacity: 1;
}
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
.code-block-wrap {
  position: relative;
}
.copy-code-btn {
  position: absolute;
  top: 8px;
  right: 8px;
  border: 1px solid rgba(148, 163, 184, 0.45);
  border-radius: 6px;
  background: rgba(15, 23, 42, 0.85);
  color: #f8fafc;
  font-size: 11px;
  font-family: "Avenir Next", "Segoe UI", sans-serif;
  padding: 4px 8px;
  cursor: pointer;
}
.copy-code-btn:hover {
  background: rgba(30, 41, 59, 0.92);
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
body.theme-dark .copy-code-btn {
  border-color: rgba(148, 163, 184, 0.45);
  background: rgba(15, 23, 42, 0.82);
  color: #e2e8f0;
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
	_, _ = w.WriteString("<h1 id=\"page-title\">")
	_, _ = w.WriteString(meta.Title)
	_, _ = w.WriteString("</h1>\n")
	_, _ = w.WriteString("<div class=\"meta\">Generated: ")
	_, _ = w.WriteString(now)
	_, _ = w.WriteString(" | Model: ")
	_, _ = w.WriteString(opts.Model)
	_, _ = w.WriteString(" | Lang: ")
	_, _ = w.WriteString(meta.Lang)
	_, _ = w.WriteString(" | Chunks: ")
	_, _ = w.WriteString(fmt.Sprintf("%d", meta.ChunkCount))
	_, _ = w.WriteString(" | Duration: ")
	_, _ = w.WriteString(durationText)
	_, _ = w.WriteString(" | Tokens: ")
	_, _ = w.WriteString(strconv.FormatInt(meta.Usage.totalTokens, 10))
	_, _ = w.WriteString("</div>\n")
	_, _ = w.WriteString("<div class=\"toolbar\">\n")
	_, _ = w.WriteString("  <label class=\"toolbar-item\">")
	_, _ = w.WriteString("<input id=\"sync-lock\" type=\"checkbox\" checked> 同步滚动</label>\n")
	_, _ = w.WriteString("  <label class=\"toolbar-item\">")
	_, _ = w.WriteString("<input id=\"theme-toggle\" type=\"checkbox\"> 深色模式</label>\n")
	_, _ = w.WriteString("  <button id=\"toc-toggle\" class=\"toolbar-item\" type=\"button\" aria-expanded=\"true\">收起目录</button>\n")
	_, _ = w.WriteString("  <label class=\"toolbar-item\">Chunk")
	_, _ = w.WriteString("<select id=\"chunk-jump\"></select></label>\n")
	_, _ = w.WriteString("  <label class=\"toolbar-item\">字号")
	_, _ = w.WriteString("<select id=\"font-size-select\"><option value=\"14\">小</option><option value=\"16\" selected>中</option><option value=\"18\">大</option><option value=\"20\">特大</option></select></label>\n")
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
  const fontSizeSelect = document.getElementById("font-size-select");

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

  function copyText(text) {
    if (navigator.clipboard && typeof navigator.clipboard.writeText === "function") {
      return navigator.clipboard.writeText(text);
    }

    return new Promise(function (resolve, reject) {
      try {
        const el = document.createElement("textarea");
        el.value = text;
        el.setAttribute("readonly", "readonly");
        el.style.position = "fixed";
        el.style.opacity = "0";
        document.body.appendChild(el);
        el.select();
        const ok = document.execCommand("copy");
        document.body.removeChild(el);
        if (!ok) {
          reject(new Error("copy failed"));
          return;
        }
        resolve();
      } catch (err) {
        reject(err);
      }
    });
  }

  function decorateCodeBlocks(root) {
    if (!root) {
      return;
    }
    root.querySelectorAll("pre").forEach(function (pre) {
      if (pre.parentElement && pre.parentElement.classList.contains("code-block-wrap")) {
        return;
      }
      const wrapper = document.createElement("div");
      wrapper.className = "code-block-wrap";
      pre.parentNode.insertBefore(wrapper, pre);
      wrapper.appendChild(pre);

      const button = document.createElement("button");
      button.type = "button";
      button.className = "copy-code-btn";
      button.textContent = "Copy";
      button.addEventListener("click", function () {
        copyText(pre.innerText || "")
          .then(function () {
            button.textContent = "Copied";
            window.setTimeout(function () {
              button.textContent = "Copy";
            }, 1200);
          })
          .catch(function () {
            button.textContent = "Failed";
            window.setTimeout(function () {
              button.textContent = "Copy";
            }, 1200);
          });
      });
      wrapper.appendChild(button);
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

  function decorateHeadingAnchors(root) {
    if (!root) {
      return;
    }
    root.querySelectorAll("h1, h2").forEach(function (heading) {
      if (!heading.id || heading.querySelector(".heading-anchor")) {
        return;
      }
      const anchor = document.createElement("a");
      anchor.className = "heading-anchor";
      anchor.href = "#" + heading.id;
      anchor.textContent = "#";
      anchor.setAttribute("aria-label", "锚点链接");
      heading.appendChild(anchor);
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
        decorateHeadingAnchors(body);
      }
      fixImageAttributes(body, item.sourceURL);
      applyHighlighting(body);
      decorateCodeBlocks(body);

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

  function setLocationHash(targetId) {
    if (!targetId) {
      return;
    }
    if (history && typeof history.replaceState === "function") {
      history.replaceState(null, "", "#" + encodeURIComponent(targetId));
      return;
    }
    window.location.hash = targetId;
  }

  function scrollToHeadingByID(targetId, behavior) {
    if (!targetId) {
      return;
    }
    const heading = document.getElementById(targetId);
    if (!heading) {
      return;
    }
    activePaneId = "en";
    const targetTop = Math.max(elementTopInPane(heading, paneEN) - 8, 0);
    markSuppressed("en", 140);
    paneEN.scrollTo({ top: targetTop, behavior: behavior || "auto" });
    window.setTimeout(function () {
      scheduleSyncFrom("en");
    }, 160);
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
        setLocationHash(heading.id);
        scrollToHeadingByID(heading.id, "smooth");
      });
      tocNav.appendChild(button);
    });

    updateTOCActive();
  }

  renderPane("en", "source");
  renderPane("zh", "translated");
  updateTitleFromContent();
  buildTOC();

  paneEN.addEventListener("click", function (event) {
    const anchor = event.target.closest(".heading-anchor");
    if (!anchor) {
      return;
    }
    event.preventDefault();
    const targetId = anchor.getAttribute("href").replace(/^#/, "");
    setLocationHash(targetId);
    scrollToHeadingByID(targetId, "smooth");
  });

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

  function applyFontSize(px) {
    const value = Math.max(12, Math.min(24, Number(px) || 16));
    document.documentElement.style.setProperty("--reader-font-size", String(value) + "px");
    if (fontSizeSelect) {
      fontSizeSelect.value = String(value);
    }
    try {
      window.localStorage.setItem("transblog_font_size", String(value));
    } catch (_) {}
  }

  if (fontSizeSelect) {
    let initial = 16;
    try {
      const saved = Number(window.localStorage.getItem("transblog_font_size"));
      if (saved >= 12 && saved <= 24) {
        initial = saved;
      }
    } catch (_) {}
    applyFontSize(initial);
    fontSizeSelect.addEventListener("change", function () {
      applyFontSize(fontSizeSelect.value);
      refreshMetrics();
      scheduleSyncFrom(activePaneId);
    });
  }

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

  function syncFromLocationHash() {
    const raw = window.location.hash.replace(/^#/, "");
    if (!raw) {
      return;
    }
    const decoded = decodeURIComponent(raw);
    if (!decoded) {
      return;
    }
    scrollToHeadingByID(decoded, "auto");
  }

  window.addEventListener("hashchange", function () {
    syncFromLocationHash();
  });

  updateProgress("en");
  applyTOCState(false);
  window.setTimeout(function () {
    syncFromLocationHash();
  }, 0);
})();
</script>
`)
	_, _ = w.WriteString("</body>\n</html>\n")
}
