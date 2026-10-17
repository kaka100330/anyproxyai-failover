# Cursor 流式响应调试指南

## 🐛 当前问题

**症状**：
- Cursor 请求显示 `tokens=0`
- 后端（Antigravity）显示请求成功并返回数据
- 流式响应可能没有正确传递到 Cursor

## ✅ 已添加的调试日志

在 `internal/service/proxy_service.go` 的 `streamDirect` 函数中添加了详细的调试日志：

1. **流量统计**：记录传输的总字节数
2. **响应缓冲**：显示响应缓冲区的大小
3. **响应预览**：显示前500字符的响应内容（debug级别）
4. **Token提取**：记录提取到的 token 数量

## 🧪 测试步骤

### 1. 启用 Debug 日志级别

在 `main.go` 中修改日志级别（如果需要看到 Debug 日志）：

```go
// 第94行，将 InfoLevel 改为 DebugLevel
log.SetLevel(log.DebugLevel)  // 原来是 log.InfoLevel
```

### 2. 重新运行程序

```powershell
# 重新编译并运行
go run .
```

### 3. 发送测试请求

从 Cursor 发送一个请求，然后查看日志输出。

### 4. 查看关键日志

应该会看到类似这样的日志：

```
time="..." level=info msg="[Cursor Stream] Received request for model: proxy_auto"
time="..." level=info msg="[Cursor Stream] Routing to: http://127.0.0.1:8545/v1/chat/completions ..."
time="..." level=debug msg="[Stream Direct] Stream completed. Total bytes: 12345"
time="..." level=debug msg="[Stream Direct] Response buffer length: 12345 bytes"
time="..." level=debug msg="[Stream Direct] Response preview: data: {\"id\":\"...\", ..."
time="..." level=info msg="[Stream Direct] Extracted tokens: prompt=100, completion=50, total=150"
time="..." level=info msg="LogRequest: model=claude-opus-4-5-20251101, tokens=150, success=true"
```

## 🔍 诊断要点

### 如果看到 `tokens=0`

检查以下内容：

1. **响应预览中是否包含 `usage` 字段**？
   - OpenAI 格式：`"usage": {"prompt_tokens": N, "completion_tokens": M}`
   - Claude 格式：在 `message_start` 或 `message_delta` 事件中

2. **响应是否完整**？
   - 检查 `Total bytes` 和 `Response buffer length` 是否一致
   - 确认没有 "Stream error" 日志

3. **SSE 格式是否正确**？
   - 应该是 `data: {...}\n\n` 格式
   - 检查是否有 `[DONE]` 标记

### 如果响应中没有 usage 信息

可能的原因：

1. **后端没有发送 usage**：
   - 某些 OpenAI 兼容 API 可能不会在流式模式下发送 usage
   - 需要在请求中添加 `stream_options: {include_usage: true}`

2. **格式不匹配**：
   - 后端可能使用了非标准的格式
   - 需要调整 `extractTokensFromStreamResponse` 函数

## 🛠️ 可能的修复方案

### 方案 1: 确保请求 usage 信息

在 Cursor 流式请求中添加 `stream_options`（第3912行附近）：

```go
else {
    reqData["stream"] = true
    reqData["stream_options"] = map[string]interface{}{
        "include_usage": true,
    }
    transformedBody, _ = json.Marshal(reqData)
    targetURL = buildOpenAIChatURL(route.APIUrl)
}
```

### 方案 2: 从非流式字段提取 tokens

如果后端在每个 chunk 中都包含临时的 token 计数，修改 `extractTokensFromStreamResponse` 来累积这些值。

### 方案 3: Post-hoc Token 计数

如果实在无法从流式响应中获取，可以考虑：
- 使用 tiktoken 库估算 token 数（需要添加依赖）
- 或者在非流式模式下获取准确的 token 数

## 📊 预期结果

修复后应该看到：

```
time="..." level=info msg="[Stream Direct] Extracted tokens: prompt=XXX, completion=YYY, total=ZZZ"
time="..." level=info msg="LogRequest: model=..., tokens=ZZZ, success=true"
time="..." level=info msg="POST /api/cursor/v1/chat/completions 200"
```

其中 XXX、YYY、ZZZ 都是大于 0 的数字。

## 📝 下一步

1. 运行测试，收集日志
2. 根据日志输出确定根本原因
3. 应用相应的修复方案
4. 验证修复效果

---

**调试完成后记得**：将日志级别改回 `log.InfoLevel` 以减少日志输出。
