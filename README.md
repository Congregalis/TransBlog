# TransBlog

`TransBlog` 是一个把英文技术网页翻译成中文 Markdown/HTML 的 Go CLI。

- 输入一个或多个 URL
- 自动抓取正文并转 Markdown
- 按 chunk 并发翻译，保留结构（标题/列表/链接/代码块）
- 输出 Markdown 或中英对照 HTML 阅读页
- 记录每个 URL 的结果汇总、token 用量和成本估算

## 环境要求

- Go `1.24+`（源码构建时）
- `OPENAI_API_KEY` 环境变量
- 可选：`OPENAI_BASE_URL`（代理或兼容网关）

## 安装

### 方式 1：下载发布包（macOS + Linux）

在 [Releases](https://github.com/Congregalis/TransBlog/releases) 下载对应平台压缩包解压后使用。

### 方式 2：源码构建

```bash
git clone https://github.com/Congregalis/TransBlog.git
cd TransBlog
go build -o transblog ./cmd/transblog
```

## 快速开始（最小可用）

```bash
export OPENAI_API_KEY="sk-..."
./transblog https://boristane.com/blog/how-i-use-claude-code/
```

默认输出到 `./out/`。

## 常用参数

```bash
transblog [flags] <url> [url...]
```

- `--model`：模型名（默认 `gpt-5.2`）
- `--out`：输出路径（单 URL 可指定文件，多 URL 指定目录）
- `--view`：输出中英对照 HTML 阅读页
- `--chunk-size`：目标分块长度（字符数）
- `--workers`：翻译并发（默认 `4`）
- `--max-retries`：OpenAI 请求最大重试次数（默认 `5`）
- `--fail-fast`：首个 URL 失败后立即停止（默认继续跑剩余 URL）
- `--glossary`：术语表 JSON 文件
- `--price-config`：价格配置 JSON（用于成本估算）
- `--timeout`：HTTP 超时（例：`120s`）
- `--version`：打印 `version/commit/build_time`

## 批量示例

### 命令行直接传多个 URL

```bash
./transblog \
  https://example.com/post-a \
  https://example.com/post-b \
  https://example.com/post-c
```

### 从文件批量（借助 shell）

`urls.txt`:

```text
https://example.com/post-a
https://example.com/post-b
https://example.com/post-c
```

执行：

```bash
xargs ./transblog < urls.txt
```

## 成本与用量说明

若配置 `--price-config`，会在终端和 `out/_summary.json` 输出 token 与预估成本。

`price.json` 示例：

```json
{
  "gpt-5.2": {
    "input_per_million": 1.0,
    "output_per_million": 2.0
  },
  "default": {
    "total_per_million": 3.0
  }
}
```

运行：

```bash
./transblog --price-config price.json https://example.com/post
```

## 输出与恢复机制

- 多 URL 会生成每 URL 一个输出文件 + `out/_summary.json`
- `.transblog.state.json` 自动记录未完成进度（按 URL + chunk）
- 再次执行同 URL 会自动复用已完成 chunk（自动 resume）
- URL 翻译全部完成后会自动删除对应 state 条目；全部完成时状态文件会被删除

## 常见错误

- `OPENAI_API_KEY is required`：未设置 API Key
- `status 429`：触发频率限制，建议降低 `--workers` 或稍后重试
- `fetch_failed`：URL 无法访问/超时/返回异常状态码
- `translate_failed`：模型响应异常或质量校验未通过
- `output_failed`：写文件或状态文件失败

详细排查见：`docs/troubleshooting.md`

## 版本信息

```bash
./transblog --version
```

示例输出：

```text
transblog version=v1.0.0 commit=abc1234 build_time=2026-02-24T20:30:00Z
```

## 开发与测试

```bash
gofmt -w .
go vet ./...
go test ./...
go build ./cmd/transblog
```

## 发布

- 本地发布流程清单：`docs/release-checklist.md`
- Tag `v*` 会触发 GitHub Actions + GoReleaser 产出 macOS/Linux 资产

## 贡献与安全

- 贡献指南：`CONTRIBUTING.md`
- 安全策略：`SECURITY.md`
- 许可证：`LICENSE`

