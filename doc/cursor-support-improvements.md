# Cursor IDE 支持改进

## 概述

本次更新增强了对 Cursor IDE 的支持，主要改进包括：

1. **Thinking/Reasoning 内容处理** - 支持 Claude 风格的 thinking 块和 OpenAI 风格的 reasoning_content
2. **Thought Signature 管理** - 全局签名存储和自动修复机制
3. **历史兼容性检查** - 自动检测并处理不兼容的消息历史
4. **无效 Thinking 块过滤** - 自动过滤或修复无效的 thinking 块

## 新增功能

### 1. 全局 Thought Signature 存储

```go
// 存储签名
adapters.StoreThoughtSignature(signature)

// 获取签名
sig := adapters.GetThoughtSignature()

// 清除签名
adapters.ClearThoughtSignature()
```

签名在以下情况下会被存储：
- 从响应中提取到有效签名时
- 从 thinking 块中提取到签名时

### 2. Thinking 块验证

```go
// 检查 thinking 块是否有有效签名
valid := adapters.HasValidSignature(block)
```

有效签名的条件：
- 签名长度 >= 50 字符
- 或者 thinking 内容为空但有任意签名

### 3. 无效 Thinking 块过滤

```go
// 过滤消息中的无效 thinking 块
filtered := adapters.FilterInvalidThinkingBlocks(messages)
```

处理逻辑：
1. 有效签名 → 保留
2. 无效签名但有全局签名 → 使用全局签名修复
3. 无效签名且无全局签名 → 转换为 text 块或丢弃

### 4. 历史兼容性检查

```go
// 检查是否需要禁用 thinking
disable := adapters.ShouldDisableThinkingDueToHistory(messages)
```

当最后一个 assistant 消息有 tool_use 但没有 thinking 时，说明这是一个非 thinking 模式开始的对话流，强制启用 thinking 会导致错误。

## 消息转换

### Assistant 消息中的 Thinking 块

**输入 (Cursor/Claude 格式)**:
```json
{
  "role": "assistant",
  "content": [
    {
      "type": "thinking",
      "thinking": "Let me analyze...",
      "signature": "base64..."
    },
    {"type": "text", "text": "Here's my answer"}
  ]
}
```

**输出 (OpenAI 格式)**:
```json
{
  "role": "assistant",
  "content": "Here's my answer",
  "reasoning_content": "Let me analyze..."
}
```

### Tool Calls 转换

**输入 (Cursor/Claude 格式)**:
```json
{
  "role": "assistant",
  "content": [
    {
      "type": "tool_use",
      "id": "toolu_01abc",
      "name": "read_file",
      "input": {"path": "/src/main.go"}
    }
  ]
}
```

**输出 (OpenAI 格式)**:
```json
{
  "role": "assistant",
  "tool_calls": [
    {
      "id": "toolu_01abc",
      "type": "function",
      "function": {
        "name": "read_file",
        "arguments": "{\"path\": \"/src/main.go\"}"
      }
    }
  ]
}
```

## 配置

在 `config.json` 中设置：

```json
{
  "debug": false,
  "enable_file_log": false
}
```

- `debug`: 设置为 false 关闭调试日志
- `enable_file_log`: 设置为 false 关闭文件日志

## 错误处理

### Signature 相关错误

当上游返回 400 错误且包含 "thought_signature" 或 "signature" 时：

1. 第一阶段：禁用 thinking + thinking 块降级为 text
2. 第二阶段：如果仍然失败，同时降级 tool_use/tool_result 块

## 测试

使用 Python 测试脚本验证功能：

```bash
cd py
python test_antigravity.py
```

测试脚本会验证：
- 基本对话功能
- Tool calls 转换
- Thinking 内容处理
- 流式响应
