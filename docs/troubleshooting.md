# Troubleshooting

本文覆盖 TransBlog 常见问题及排查步骤。

## 1. `OPENAI_API_KEY is required`

### 现象

运行后立即退出并提示缺少 API Key。

### 处理

```bash
export OPENAI_API_KEY="sk-..."
```

可选网关：

```bash
export OPENAI_BASE_URL="https://your-gateway.example.com/v1"
```

---

## 2. 抓取失败（`fetch_failed`）

### 常见原因

- URL 不可访问（404/500）
- 网络超时
- 目标站点反爬或连接不稳定

### 处理建议

1. 先在浏览器验证 URL 可访问。
2. 增大超时：`--timeout 120s`。
3. 适当重试或换网络环境。
4. 多 URL 时检查 `out/_summary.json`，只重跑失败 URL。

---

## 3. 429 / 5xx（OpenAI 限流或临时故障）

### 现象

- 错误信息出现 `status 429`
- 或 `status 5xx`

### 处理建议

1. 降低并发：`--workers 1` 或 `--workers 2`。
2. 增大重试：`--max-retries 8`。
3. 稍后重跑失败 URL。

---

## 4. 翻译失败（`translate_failed`）

### 常见原因

- 模型返回空内容或结构损坏
- 严格兜底重试后仍未通过质量校验

### 处理建议

1. 查看 `out/_summary.json` 的 `error_message`。
2. 调整 chunk：`--chunk-size 1200` 或更小。
3. 使用术语表减少歧义：`--glossary glossary.json`。
4. 对失败 URL 单独重跑。

---

## 5. 输出失败（`output_failed`）

### 常见原因

- 输出目录不可写
- 文件名冲突或路径非法
- 状态文件写入失败

### 处理建议

1. 检查目录权限，或换 `--out` 到可写目录。
2. 确认磁盘空间充足。
3. 重新运行同 URL，工具会自动 resume 已完成 chunk。

---

## 6. 结果结构异常（标题/列表/代码块看起来不对）

### 处理建议

1. 先看 `_summary.json` 中 `quality_fallback_count` 是否频繁触发。
2. 缩小 `--chunk-size`，减少长段落结构损失风险。
3. 用 `--view` 打开中英对照，快速定位异常 chunk。
4. 必要时切换模型重试。

---

## 7. 成本或 token 不完整

### 现象

summary 里出现 `missing_usage_count > 0` 或 `cost_estimate_partial=true`。

### 说明

部分模型/网关响应可能不返回 usage 字段，工具会降级处理并继续运行。

### 处理建议

1. 保留 `--price-config`，继续拿到可估算部分。
2. 如果必须精确结算，改用稳定返回 usage 的官方/兼容网关。

