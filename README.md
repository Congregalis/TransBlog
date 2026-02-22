# TransBlog

`TransBlog` 是一个面向中文开发者的 Go 命令行工具：

- 输入英文技术博客 URL
- 自动抓取页面并转换为 Markdown
- 按分块翻译为中文，尽量保持原始 Markdown 结构
- 可选生成中英双栏对照 HTML 阅读页，方便学习

项目目标很直接：把英文技术博客翻译成中文，降低学习门槛。

## 功能特性

- Go 1.24+ CLI（入口：`cmd/transblog`）
- 支持一次输入一个或多个 URL
- 使用 `github.com/JohannesKaufmann/html-to-markdown/v2` 将 HTML 转成 Markdown
- 翻译时尽量保留标题、列表、链接、代码块等 Markdown 结构
- 自动按块拆分（默认目标：2000 字符）
- 使用 OpenAI Responses API（`/v1/responses`）并内置 429/5xx 重试与退避
- 支持术语表（glossary JSON）保证术语一致性
- 默认输出到当前工作目录 `./out/`，并在终端打印文件路径
- 支持 `--out` 自定义输出路径
- 支持 `--view` 生成双栏对照 HTML：
  - `marked.js` 渲染 Markdown
  - `DOMPurify` 清理 HTML
  - `highlight.js` 代码高亮
  - 双向同步滚动（可开关）
  - Chunk 快速跳转 + 阅读进度

## 环境要求

- Go 1.24+
- `OPENAI_API_KEY` 环境变量
- 可选：`OPENAI_BASE_URL`（代理或兼容网关）

## 安装与构建

```bash
go build ./cmd/transblog
```

## 使用方式

```bash
transblog [flags] <url> [url...]
```

### 基础示例

```bash
transblog https://boristane.com/blog/how-i-use-claude-code/
```

### 生成中英双栏阅读页

```bash
transblog --view https://boristane.com/blog/how-i-use-claude-code/
```

### 使用术语表并指定输出

```bash
transblog \
  --glossary glossary.json \
  --model gpt-5.2 \
  --timeout 120s \
  --out out.md \
  https://example.com/post
```

## 常用参数

- `--model`：模型名称（默认 `gpt-5.2`）
- `--out`：输出文件路径（默认写入 `./out/`）
- `--view`：输出 HTML 双栏阅读页（默认输出 Markdown）
- `--glossary`：术语表 JSON 文件路径
- `--timeout`：HTTP 超时（示例：`120s`）
- `--chunk-size`：目标分块长度（字符数）

## 术语表格式

准备一个 JSON 文件，键值均为字符串：

```json
{
  "agent": "智能体",
  "chunk": "分块",
  "prompt": "提示词"
}
```

## 输出说明

默认不传 `--out` 时，会写入当前目录下的 `out/`，并打印类似：

```text
Output: out/<generated-name>.md
```

当启用 `--view` 时，输出扩展名为 `.html`。

Markdown 输出会包含 YAML front-matter，例如：

```yaml
---
source_urls:
  - https://example.com/post
generated_at: 2026-02-22T10:00:00Z
model: gpt-5.2
---
```

随后每个分块按以下结构写入：

```markdown
<英文原文分块>

---

<中文翻译分块>
```

## 测试

```bash
go test ./...
```

## 贡献

欢迎提 Issue 或 PR，提交前请先阅读：`CONTRIBUTING.md`

## 安全

安全问题请参考：`SECURITY.md`

## 许可证

MIT，见：`LICENSE`
