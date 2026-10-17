# Cursor API 转换详细说明

## 概述

Cursor IDE 使用类似 OpenAI 的 API 接口，但其 tools 和 messages 格式更接近 Anthropic/Claude 格式。本文档详细说明 Cursor 请求/响应的转换逻辑。

## Cursor 请求格式特点

### 1. Tool 定义格式

**Cursor 扁平格式** (类似 Claude):
```json
{
  "name": "read_file",
  "description": "Read file content",
  "input_schema": {
    "type": "object",
    "properties": {
      "path": {"type": "string"}
    },
    "required": ["path"]
  }
}
```

**标准 OpenAI 嵌套格式**:
```json
{
  "type": "function",
  "function": {
    "name": "read_file",
    "description": "Read file content",
    "parameters": {
      "type": "object",
      "properties": {
        "path": {"type": "string"}
      },
      "required": ["path"]
    }
  }
}
```

### 2. Assistant 消息中的 Tool Calls

**Cursor/Claude 格式** - tool_use 在 content 数组中:
```json
{
  "role": "assistant",
  "content": [
    {"type": "text", "text": "Let me read that file."},
    {
      "type": "tool_use",
      "id": "toolu_01abc",
      "name": "read_file",
      "input": {"path": "/src/main.go"}
    }
  ]
}
```

**OpenAI 格式** - tool_calls 作为独立字段:
```json
{
  "role": "assistant",
  "content": "Let me read that file.",
  "tool_calls": [
    {
      "id": "call_abc123",
      "type": "function",
      "function": {
        "name": "read_file",
        "arguments": "{\"path\": \"/src/main.go\"}"
      }
    }
  ]
}
```

### 3. Tool Results

**Cursor/Claude 格式** - tool_result 在 user 消息的 content 数组中:
```json
{
  "role": "user",
  "content": [
    {
      "type": "tool_result",
      "tool_use_id": "toolu_01abc",
      "content": "File content here..."
    }
  ]
}
```

**OpenAI 格式** - 使用 tool 角色:
```json
{
  "role": "tool",
  "tool_call_id": "call_abc123",
  "content": "File content here..."
}
```

## Thinking/Reasoning 内容处理

### 来源格式

**Claude/Anthropic 格式** - thinking 块:
```json
{
  "role": "assistant",
  "content": [
    {
      "type": "thinking",
      "thinking": "Let me analyze this problem...",
      "signature": "base64_signature_string..."
    },
    {"type": "text", "text": "Here's my answer..."}
  ]
}
```

**OpenAI 格式** - reasoning_content 字段:
```json
{
  "role": "assistant",
  "content": "Here's my answer...",
  "reasoning_content": "Let me analyze this problem..."
}
```

### Thinking Signature 处理

Thinking blocks 需要有效的 signature 才能在后续请求中使用：

1. **有效签名**: signature 长度 >= 50 字符
2. **空 thinking + 任意签名**: 有效（trailing signature 情况）
3. **无效签名处理**:
   - 尝试使用全局存储的签名修复
   - 如果无法修复，将 thinking 块降级为 text 块
   - 空 thinking 块直接丢弃

### 全局签名存储

```python
# Python 实现
_global_thought_signature: Optional[str] = None

def global_thought_signature_store(sig: str):
    """存储签名供后续 tool calls 使用"""
    global _global_thought_signature
    if _global_thought_signature is None or len(sig) > len(_global_thought_signature):
        _global_thought_signature = sig

def global_thought_signature_get() -> Optional[str]:
    """获取存储的签名"""
    return _global_thought_signature
```

## 转换流程

### 请求转换 (Cursor → OpenAI)

```
1. 检测请求格式 (detectRequestFormat)
   - 检查 tools 是否为扁平格式
   - 检查 messages 中是否有 tool_use/tool_result 块

2. 转换 Tools
   - 扁平格式 → 嵌套格式
   - 清理 JSON Schema (移除不支持的字段)

3. 转换 Messages
   - tool_use 块 → tool_calls 字段
   - tool_result 块 → tool 角色消息
   - thinking 块 → reasoning_content 字段

4. 处理 tool_choice
   - {type: "auto"} → "auto"
   - {type: "any"} → "required"
   - {type: "tool", name: "xxx"} → {type: "function", function: {name: "xxx"}}
```

### 响应转换 (OpenAI → Cursor)

```
1. 流式响应
   - delta.content → 文本内容
   - delta.reasoning_content → thinking 内容
   - delta.tool_calls → tool_use 块

2. 非流式响应
   - message.content → text 块
   - message.reasoning_content → thinking 块
   - message.tool_calls → tool_use 块

3. 签名处理
   - 从响应中提取 signature
   - 存储到全局签名存储
   - 用于后续请求的 thinking 块修复
```

## JSON Schema 清理

Gemini API 对 JSON Schema 有严格要求：

```go
// 需要移除的字段
skipFields := map[string]bool{
    "additionalProperties": true,
    "$schema":              true,
    "title":                true,
    "default":              true,
}

// type 需要大写
// "string" → "STRING"
// "object" → "OBJECT"

// anyOf 处理
// 简化为第一个非 null 选项
```

## 历史兼容性检查

在启用 thinking 模式前需要检查历史消息：

```python
def should_disable_thinking_due_to_history(messages: list) -> bool:
    """检查历史是否与 thinking 模式兼容"""
    for msg in reversed(messages):
        if msg.get("role") in ("assistant", "model"):
            content = msg.get("content")
            if isinstance(content, list):
                has_tool_use = any(b.get("type") == "tool_use" for b in content)
                has_thinking = any(b.get("type") == "thinking" for b in content)
                
                # Tool use 但没有 thinking = 不兼容
                if has_tool_use and not has_thinking:
                    return True
            return False
    return False
```

## 流式处理

### Cursor 流式处理器

```python
class CursorStreamingProcessor:
    """直接处理 Gemini v1internal SSE 流到 OpenAI 格式"""
    
    def process_line(self, line: str) -> str:
        # 解析 Gemini SSE 数据
        # 转换为 OpenAI chunk 格式
        
        # Thinking 内容
        if part.get("thought"):
            return self._format_chunk({
                "role": "assistant",
                "content": None,
                "reasoning_content": text
            })
        
        # Function call
        if part.get("functionCall"):
            return self._format_chunk({
                "tool_calls": [{
                    "index": self.current_tool_index,
                    "id": tool_id,
                    "type": "function",
                    "function": {
                        "name": tool_name,
                        "arguments": json.dumps(tool_args)
                    }
                }]
            })
        
        # 普通文本
        return self._format_chunk({"content": text})
```

## 错误处理

### Signature 相关错误

当上游返回 400 错误且包含 "thought_signature" 或 "signature" 时：

1. **第一阶段**: 禁用 thinking + thinking 块降级为 text
2. **第二阶段**: 如果仍然失败且错误指向 tool/function，同时降级 tool_use/tool_result 块

```go
func isGeminiSignatureRelatedError(respBody []byte) bool {
    msg := strings.ToLower(extractErrorMessage(respBody))
    return strings.Contains(msg, "thought_signature") || 
           strings.Contains(msg, "signature")
}
```

## 模型支持

### 支持 Thinking 的模型

```python
def model_supports_thinking(model: str) -> bool:
    model_lower = model.lower()
    
    # 显式 thinking 模型
    if "-thinking" in model_lower:
        return True
    
    # Gemini 3 Pro 系列
    if "gemini-3-pro" in model_lower:
        return True
    
    # Claude 模型
    if model_lower.startswith("claude-"):
        return True
    
    # 普通 Gemini 模型不支持
    return False
```

### 默认启用 Thinking 的模型

```python
def should_enable_thinking_by_default(model: str) -> bool:
    model_lower = model.lower()
    
    # Opus 4.5 系列
    if "opus-4-5" in model_lower or "opus-4.5" in model_lower:
        return True
    
    # 显式 -thinking 后缀
    if "-thinking" in model_lower:
        return True
    
    return False
```

## 配置示例

```json
{
  "enable_thinking": true,
  "thinking_budget": 10000,
  "rate_limit_requests": 10,
  "rate_limit_window": 60.0,
  "rate_limit_interval": 2.0
}
```
