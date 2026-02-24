# Changelog

All notable changes to this project will be documented in this file.

## [Unreleased]

### Added

- `--version` flag with `version/commit/build_time` output
- GoReleaser configuration for macOS/Linux release artifacts
- Release workflow (`.github/workflows/release.yml`) for tag-based publishing
- Troubleshooting and release checklist docs

## [0.1.0] - 2026-02-24

### Added

- Initial translation pipeline (fetch HTML -> markdown -> chunk -> translate -> output)
- Multi-URL execution with per-URL outputs and `_summary.json`
- Error classification (`fetch_failed`, `convert_failed`, `translate_failed`, `output_failed`)
- Automatic resume via `.transblog.state.json` with chunk-level reuse and cleanup on completion
- Configurable execution flags (`--workers`, `--max-retries`, `--fail-fast`, `--timeout`, `--chunk-size`)
- Translation quality guard with strict retry and fallback counters
- Token usage + optional cost estimation (`--price-config`)
- HTML split-view mode with synchronized scrolling and chunk navigation
- Expanded unit/e2e coverage and CI quality gates

