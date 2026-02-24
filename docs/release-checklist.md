# Release Checklist

发布前请按顺序执行。

## 1) 代码与测试

- [ ] `gofmt -w .`
- [ ] `go vet ./...`
- [ ] `go test ./...`
- [ ] `go build ./cmd/transblog`
- [ ] CI 全绿（含 coverage 门禁、e2e）

## 2) 文档与变更记录

- [ ] 更新 `README.md`（参数、示例、已知限制）
- [ ] 更新 `docs/troubleshooting.md`
- [ ] 更新 `CHANGELOG.md`，新增版本条目（含日期和重点变更）

## 3) 版本与构建信息

- [ ] 确认 `--version` 输出包含 `version/commit/build_time`
- [ ] 用 ldflags 本地验证一次：

```bash
go build -ldflags "-X transblog/internal/version.Version=vX.Y.Z -X transblog/internal/version.Commit=$(git rev-parse --short HEAD) -X transblog/internal/version.BuildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)" -o transblog ./cmd/transblog
./transblog --version
```

## 4) Tag 与发布资产

- [ ] 创建并推送 tag：`vX.Y.Z`
- [ ] 确认 `Release` workflow 成功执行
- [ ] 检查发布资产包含：
  - [ ] `linux/amd64`
  - [ ] `linux/arm64`
  - [ ] `darwin/amd64`
  - [ ] `darwin/arm64`
  - [ ] `checksums.txt`

## 5) 发布后验证

- [ ] 从 Release 下载一个包，解压后执行 `transblog --version`
- [ ] 用真实 URL 跑一次最小示例
- [ ] 抽查输出内容与 `_summary.json`

