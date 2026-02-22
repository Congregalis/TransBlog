# Contributing

Thanks for your interest in contributing.

## Development Setup

1. Install Go (version from `go.mod`, currently `1.24.x`).
2. Copy environment template:

```bash
cp .env.example .env
```

3. Set `OPENAI_API_KEY` in `.env`.

## Common Commands

```bash
go test ./...
go build ./cmd/transblog
```

## Contribution Workflow

1. Fork the repository and create a feature branch.
2. Make small, focused changes.
3. Run formatting and tests before opening a PR.
4. Open a pull request with:
   - clear problem statement
   - implementation notes
   - test evidence

## Commit Message Style

Use Conventional Commits when possible:

- `feat: ...`
- `fix: ...`
- `docs: ...`
- `chore: ...`
- `refactor: ...`
- `test: ...`

## Pull Request Checklist

- [ ] Code compiles locally
- [ ] Tests pass (`go test ./...`)
- [ ] Documentation updated if behavior changed
- [ ] No secrets or credentials included
