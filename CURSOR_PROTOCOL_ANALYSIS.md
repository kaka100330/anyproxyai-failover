# Cursor API 协议转换深度分析

> **文档版本**: v1.0  
> **生成时间**: 2026-01-12  
> **分析对象**: `antigravity_proxy.py` 和 Go 内部适配器

---

## 📋 目录

1. [核心概述](#核心概述)
2. [Cursor 接口特征](#cursor-接口特征)
3. [协议转换精髓](#协议转换精髓)
4. [流式输出机制](#流式输出机制)
5. [Thinking 模式处理](#thinking-模式处理)
6. [Tools/Function Calling](#tools-function-calling)
7. [并发问题分析](#并发问题分析)
8. [网络协议细节](#网络协议细节)
9. [改进建议](#改进建议)

---

## 🎯 核心概述

### Cursor 的多重身份

Cursor IDE 同时支持**多种 API 格式**,这是其最大的特点:

```
┌─────────────────────────────────────────┐
│          Cursor API 请求              │
├─────────────────────────────────────────┤
│  1. OpenAI 格式 (基础)                │
│  2. Anthropic/Claude 格式 (扩展)      │
│  3. 混合格式 (常见)                   │
│  4. Responses API (新版)              │
└─────────────────────────────────────────┘
```

### 核心转换链路

```
Cursor 请求 → 格式检测 → 统一转换 → 后端适配 → 流式输出 → Cursor 响应
     ↓            ↓          ↓          ↓          ↓          ↓
 混合格式    自动识别   Claude格式  Gemini/等    SSE流     OpenAI格式
```

---

## 🔍 Cursor 接口特征

### 1. 请求格式识别逻辑

```python
def detect_cursor_format(body):
    """
    核心检测逻辑 - 来自 antigravity_proxy.py 3780-3798行
    """
    is_anthropic = False
    messages = body.get("messages", [])
    
    # 特征1: messages 包含 content 数组
    for msg in messages:
        content = msg.get("content")
        if isinstance(content, list):
            for block in content:
                # Anthropic 特征: type 字段
                if isinstance(block, dict) and "type" in block:
                    is_anthropic = True
                    break
    
    # 特征2: tools 直接包含 name 字段 (非 OpenAI 嵌套格式)
    tools = body.get("tools", [])
    for tool in tools:
        if tool.get("name") and not tool.get("type"):
            is_anthropic = True
            break
    
    return is_anthropic
```

### 2. 混合格式示例

Cursor 经常发送这样的**混合请求**:

```json
{
  "model": "gpt-4",
  "stream": true,
  "messages": [
    {
      "role": "assistant",
      "content": [
        {"type": "thinking", "thinking": "...", "signature": "..."},
        {"type": "text", "text": "Response text"},
        {"type": "tool_use", "id": "...", "name": "...", "input": {...}}
      ]
    },
    {
      "role": "user",
      "content": [
        {"type": "tool_result", "tool_use_id": "...", "content": "..."}
      ]
    }
  ],
  "tools": [
    {"name": "function_name", "description": "...", "input_schema": {...}}
  ]
}
```

**关键特征**:
- ✅ 使用 OpenAI 的基础结构 (`/v1/chat/completions`)
- ✅ 但 `content` 使用 Anthropic 的块数组格式
- ✅ `tools` 使用 Anthropic 的扁平格式
- ✅ 支持 `thinking` 块(推理内容)

---

## ⚙️ 协议转换精髓

### 核心转换器架构

```python
class ProtocolConverter:
    """
    三层转换架构
    """
    
    # 第1层: 输入格式统一
    def normalize_input(self, request):
        if self.is_anthropic_format(request):
            return request  # 已是 Claude 格式
        else:
            return OpenAIConverter.openai_to_claude(request)
    
    # 第2层: 后端适配
    def adapt_to_backend(self, claude_req, backend):
        if backend == "gemini":
            return RequestTransformer().transform(claude_req, ...)
        elif backend == "claude":
            return claude_req
        elif backend == "openai":
            return self.claude_to_openai(claude_req)
    
    # 第3层: 输出格式转换
    def format_output(self, backend_resp, target_format):
        if target_format == "openai":
            return ClaudeToOpenAIConverter.convert(backend_resp)
        elif target_format == "anthropic":
            return backend_resp
```

### 关键转换函数

#### 1. OpenAI → Claude (核心函数)

```python
# antigravity_proxy.py 907-1128行
def openai_to_claude(openai_req):
    """
    最关键的转换逻辑
    """
    messages = []
    system_content = None
    pending_tool_results = []
    
    for msg in openai_req["messages"]:
        role = msg["role"]
        
        if role == "system":
            # System 消息 → Claude system 参数
            system_content = msg["content"]
            
        elif role == "tool":
            # Tool 消息 → user消息 + tool_result块
            pending_tool_results.append({
                "type": "tool_result",
                "tool_use_id": msg["tool_call_id"],
                "content": msg["content"]
            })
            
        elif role == "assistant":
            # 处理 reasoning_content (thinking)
            claude_content = []
            
            # 1. Thinking 必须放在最前面
            if msg.get("reasoning_content"):
                sig = get_global_signature()  # 关键!
                claude_content.append({
                    "type": "thinking",
                    "thinking": msg["reasoning_content"],
                    "signature": sig
                })
            
            # 2. 文本内容
            if msg.get("content"):
                claude_content.append({
                    "type": "text",
                    "text": msg["content"]
                })
            
            # 3. Tool calls → tool_use 块
            for tc in msg.get("tool_calls", []):
                claude_content.append({
                    "type": "tool_use",
                    "id": tc["id"],
                    "name": tc["function"]["name"],
                    "input": json.loads(tc["function"]["arguments"])
                })
            
            messages.append({"role": "assistant", "content": claude_content})
    
    # Tools 转换
    claude_tools = []
    for tool in openai_req.get("tools", []):
        if tool["type"] == "function":
            func = tool["function"]
            claude_tools.append({
                "name": func["name"],
                "description": func["description"],
                "input_schema": clean_json_schema(func["parameters"])
            })
    
    return {
        "model": openai_req["model"],
        "messages": messages,
        "system": system_content,
        "tools": claude_tools,
        "max_tokens": openai_req.get("max_tokens", 4096),
        "stream": openai_req.get("stream", False)
    }
```

#### 2. Claude → OpenAI (响应转换)

```python
# antigravity_proxy.py 1131-1196行
def claude_to_openai_response(claude_resp):
    """
    响应格式转换
    """
    content = ""
    reasoning_content = ""
    tool_calls = []
    
    for block in claude_resp["content"]:
        if block["type"] == "thinking":
            # Thinking → reasoning_content
            reasoning_content += block["thinking"]
            
            # 存储签名供后续使用 (关键!)
            if block.get("signature"):
                global_thought_signature_store(block["signature"])
                
        elif block["type"] == "text":
            content += block["text"]
            
        elif block["type"] == "tool_use":
            tool_calls.append({
                "id": block["id"],
                "type": "function",
                "function": {
                    "name": block["name"],
                    "arguments": json.dumps(block["input"])
                }
            })
    
    message = {
        "role": "assistant",
        "content": content
    }
    
    # 关键: reasoning_content 单独字段
    if reasoning_content:
        message["reasoning_content"] = reasoning_content
        
    if tool_calls:
        message["tool_calls"] = tool_calls
    
    return {
        "id": f"chatcmpl-{claude_resp['id']}",
        "object": "chat.completion",
        "model": claude_resp["model"],
        "choices": [{
            "index": 0,
            "message": message,
            "finish_reason": "tool_calls" if tool_calls else "stop"
        }],
        "usage": {
            "prompt_tokens": claude_resp["usage"]["input_tokens"],
            "completion_tokens": claude_resp["usage"]["output_tokens"],
            "total_tokens": claude_resp["usage"]["input_tokens"] + claude_resp["usage"]["output_tokens"]
        }
    }
```

---

## 🌊 流式输出机制

### SSE (Server-Sent Events) 格式

Cursor 使用标准的 OpenAI SSE 格式:

```
event: message_start
data: {"type":"message_start","message":{...}}

event: content_block_start  
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}

event: content_block_delta
data: {"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"..."}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}

event: message_stop
data: {"type":"message_stop"}
```

### 流式处理器架构

```python
class StreamingProcessor:
    """
    antigravity_proxy.py 1659-1896行
    
    状态机模式处理流式输出
    """
    
    # 块类型
    BLOCK_NONE = 0
    BLOCK_TEXT = 1
    BLOCK_THINKING = 2
    BLOCK_FUNCTION = 3
    
    def __init__(self):
        self.block_type = self.BLOCK_NONE
        self.block_index = 0
        self.pending_signature = ""
        self.trailing_signature = ""  # 尾部签名
        
    def process_part(self, part):
        """
        处理 Gemini 的 part,转换为 Claude SSE 事件
        """
        result = []
        
        # 1. 处理 thinking
        if part.get("thought"):
            text = part.get("text", "")
            signature = part.get("thoughtSignature", "")
            
            # 开始 thinking 块
            if self.block_type != self.BLOCK_THINKING:
                result.append(self._start_block(
                    self.BLOCK_THINKING,
                    {"type": "thinking", "thinking": ""}
                ))
            
            # 发送 thinking 内容
            if text:
                result.append(self._emit_delta(
                    "thinking_delta",
                    {"thinking": text}
                ))
            
            # 暂存签名(在块结束时发送)
            if signature:
                self.pending_signature = signature
        
        # 2. 处理文本
        elif part.get("text"):
            text = part["text"]
            
            if self.block_type != self.BLOCK_TEXT:
                result.append(self._start_block(
                    self.BLOCK_TEXT,
                    {"type": "text", "text": ""}
                ))
            
            result.append(self._emit_delta(
                "text_delta",
                {"text": text}
            ))
        
        # 3. 处理 function call
        elif part.get("functionCall"):
            fc = part["functionCall"]
            
            tool_use = {
                "type": "tool_use",
                "id": fc.get("id"),
                "name": fc["name"],
                "input": {}
            }
            
            result.append(self._start_block(
                self.BLOCK_FUNCTION,
                tool_use
            ))
            
            if fc.get("args"):
                result.append(self._emit_delta(
                    "input_json_delta",
                    {"partial_json": json.dumps(fc["args"])}
                ))
            
            result.append(self._end_block())
        
        return "".join(result)
    
    def _start_block(self, block_type, content_block):
        """开始新块"""
        result = []
        
        # 先结束之前的块
        if self.block_type != self.BLOCK_NONE:
            result.append(self._end_block())
        
        # 发送 content_block_start 事件
        result.append(self._format_sse(
            "content_block_start",
            {
                "type": "content_block_start",
                "index": self.block_index,
                "content_block": content_block
            }
        ))
        
        self.block_type = block_type
        return "".join(result)
    
    def _end_block(self):
        """结束当前块"""
        if self.block_type == self.BLOCK_NONE:
            return ""
        
        result = []
        
        # Thinking 块结束时发送签名
        if self.block_type == self.BLOCK_THINKING and self.pending_signature:
            result.append(self._emit_delta(
                "signature_delta",
                {"signature": self.pending_signature}
            ))
            self.pending_signature = ""
        
        # 发送 content_block_stop 事件
        result.append(self._format_sse(
            "content_block_stop",
            {"type": "content_block_stop", "index": self.block_index}
        ))
        
        self.block_index += 1
        self.block_type = self.BLOCK_NONE
        
        return "".join(result)
    
    def _format_sse(self, event_type, data):
        """格式化 SSE 事件"""
        return f"event: {event_type}\ndata: {json.dumps(data)}\n\n"
```

### OpenAI 流式处理

```python
class OpenAIStreamingProcessor:
    """
    antigravity_proxy.py 1901-2029行
    
    处理 OpenAI 格式的流式输出
    """
    
    def process_claude_event(self, event_type, data):
        """
        将 Claude SSE 事件转换为 OpenAI SSE 格式
        """
        result = []
        
        if event_type == "content_block_delta":
            delta = data.get("delta", {})
            delta_type = delta.get("type")
            
            if delta_type == "text_delta":
                # 文本 delta
                text = delta.get("text", "")
                if text:
                    result.append(self._format_chunk({
                        "content": text
                    }))
            
            elif delta_type == "thinking_delta":
                # Thinking → reasoning_content
                thinking = delta.get("thinking", "")
                if thinking:
                    result.append(self._format_chunk({
                        "role": "assistant",
                        "content": None,
                        "reasoning_content": thinking  # 关键字段!
                    }))
            
            elif delta_type == "signature_delta":
                # 存储签名(不发送给客户端)
                sig = delta.get("signature", "")
                if sig:
                    global_thought_signature_store(sig)
            
            elif delta_type == "input_json_delta":
                # Tool call arguments
                partial = delta.get("partial_json", "")
                if partial:
                    result.append(self._format_chunk({
                        "tool_calls": [{
                            "index": self.current_tool_index,
                            "function": {"arguments": partial}
                        }]
                    }))
        
        return "".join(result)
    
    def _format_chunk(self, delta, finish_reason=None):
        """格式化 OpenAI chunk"""
        chunk = {
            "id": self.chunk_id,
            "object": "chat.completion.chunk",
            "created": self.created_ts,
            "model": self.original_model,
            "choices": [{
                "index": 0,
                "delta": delta,
                "finish_reason": finish_reason
            }]
        }
        return f"data: {json.dumps(chunk)}\n\n"
```

---

## 🧠 Thinking 模式处理

### Thought Signature 机制

这是**最核心的创新点**,解决了跨请求的签名传递问题:

```python
# 全局签名存储 (antigravity_proxy.py 132-149行)
_global_thought_signature = None

def global_thought_signature_store(sig):
    """
    存储签名供后续请求使用
    
    场景:
    1. 第一次请求: Model 返回 thinking + signature
    2. 存储: 保存 signature
    3. Tool 执行: User 提供 tool_result
    4. 第二次请求: 需要带上之前的 signature!
    """
    global _global_thought_signature
    if _global_thought_signature is None or len(sig) > len(_global_thought_signature):
        _global_thought_signature = sig
        print(f"[ThoughtSig] Stored signature (len={len(sig)})")

def global_thought_signature_get():
    """获取存储的签名"""
    return _global_thought_signature
```

### Thinking 块验证

```python
# antigravity_proxy.py 460-481行
MIN_SIGNATURE_LENGTH = 50

def has_valid_signature(block):
    """
    检查 thinking 块是否有效
    
    规则:
    1. 空 thinking + 任意签名 = 有效 (尾部签名)
    2. 非空 thinking + 长签名(≥50) = 有效
    3. 其他 = 无效
    """
    if block["type"] != "thinking":
        return True
    
    thinking = block.get("thinking", "")
    signature = block.get("signature", "")
    
    # 空 thinking + 签名 = 有效
    if not thinking and signature:
        return True
    
    # 非空 thinking + 有效签名 = 有效
    if signature and len(signature) >= MIN_SIGNATURE_LENGTH:
        return True
    
    return False
```

### Thinking 块过滤和修复

```python
# antigravity_proxy.py 484-551行
def filter_invalid_thinking_blocks(messages):
    """
    过滤/修复无效的 thinking 块
    
    策略:
    1. 有效签名: 保留
    2. 无签名但有全局签名: 修复(用全局签名)
    3. 无签名且无全局签名: 降级为文本
    """
    total_filtered = 0
    global_sig = global_thought_signature_get()
    
    for msg in messages:
        if msg["role"] not in ("assistant", "model"):
            continue
        
        content = msg.get("content")
        if not isinstance(content, list):
            continue
        
        new_blocks = []
        for block in content:
            if block["type"] == "thinking":
                if has_valid_signature(block):
                    # 有效 - 保留
                    new_blocks.append({
                        "type": "thinking",
                        "thinking": block.get("thinking", ""),
                        "signature": block.get("signature", "")
                    })
                    
                elif global_sig and len(global_sig) >= MIN_SIGNATURE_LENGTH:
                    # 无效但有全局签名 - 修复
                    print(f"[Thinking-Filter] Repairing with global signature")
                    new_blocks.append({
                        "type": "thinking",
                        "thinking": block.get("thinking", ""),
                        "signature": global_sig
                    })
                    
                else:
                    # 无效且无全局签名 - 降级为文本
                    thinking_text = block.get("thinking", "")
                    if thinking_text.strip():
                        print(f"[Thinking-Filter] Downgrading to text")
                        new_blocks.append({
                            "type": "text",
                            "text": thinking_text
                        })
                    total_filtered += 1
            else:
                new_blocks.append(block)
        
        msg["content"] = new_blocks
    
    return total_filtered
```

### Thinking 模式智能开关

```python
# antigravity_proxy.py 1228-1276行
def transform(claude_req, project_id, mapped_model):
    """
    请求转换时的 thinking 决策
    """
    messages = claude_req["messages"]
    
    # 1. 过滤无效 thinking 块
    filter_invalid_thinking_blocks(messages)
    
    # 2. 检查是否显式请求
    thinking_config = claude_req.get("thinking", {})
    is_thinking_requested = thinking_config.get("type") == "enabled"
    
    # 3. 检查模型是否支持
    target_supports_thinking = model_supports_thinking(mapped_model)
    # gemini-3-pro-*, claude-*, *-thinking 支持
    
    # 4. 检查历史兼容性
    history_compatible = not should_disable_thinking_due_to_history(messages)
    # 如果上一条 assistant 消息有 tool_use 但没 thinking,说明是非 thinking 模式开始的
    
    # 5. 检查函数调用签名
    has_function_calls = any(has_tool_use_in_msg(msg) for msg in messages)
    has_valid_sig = has_valid_signature_for_function_calls(messages)
    
    # 最终决策
    is_thinking = (
        is_thinking_requested and
        target_supports_thinking and
        history_compatible and
        (not has_function_calls or has_valid_sig)
    )
    
    if is_thinking_requested and not is_thinking:
        reasons = []
        if not target_supports_thinking:
            reasons.append(f"model '{mapped_model}' not supported")
        if not history_compatible:
            reasons.append("history has tool_use without thinking")
        if has_function_calls and not has_valid_sig:
            reasons.append("no valid signature")
        print(f"[Transform] Thinking DISABLED: {', '.join(reasons)}")
    
    # 构建请求...
```

---

## 🔧 Tools/Function Calling

### Tool 定义转换

```python
# JSON Schema 清理 (antigravity_proxy.py 378-455行)
EXCLUDED_SCHEMA_KEYS = {
    "$schema", "$id", "$ref", "minLength", "maxLength", "pattern",
    "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum",
    "multipleOf", "uniqueItems", "minItems", "maxItems",
    "oneOf", "anyOf", "allOf", "not", "if", "then", "else",
    "$defs", "definitions", "strict", ...
}

def clean_json_schema(schema):
    """
    清理 JSON Schema 以兼容 Gemini
    
    关键转换:
    1. type: "string" → "STRING" (大写)
    2. 移除不支持的关键字
    3. 处理 union types: ["string", "null"] → "STRING"
    """
    def clean_value(value):
        if isinstance(value, dict):
            result = {}
            for k, v in value.items():
                if k in EXCLUDED_SCHEMA_KEYS:
                    continue
                
                if k == "type":
                    # 转大写
                    if isinstance(v, str):
                        result[k] = v.upper()
                    elif isinstance(v, list):
                        # Union type - 取第一个非 null
                        for t in v:
                            if t.lower() != "null":
                                result[k] = t.upper()
                                break
                
                elif k == "properties":
                    # 递归清理嵌套属性
                    result[k] = {
                        name: clean_value(schema)
                        for name, schema in v.items()
                    }
                
                elif k in ("description", "enum", "required"):
                    result[k] = v
                
                else:
                    result[k] = clean_value(v)
            
            return result
        
        elif isinstance(value, list):
            return [clean_value(item) for item in value]
        
        return value
    
    cleaned = clean_value(schema)
    
    # 确保有 type 和 properties
    if "type" not in cleaned:
        cleaned["type"] = "OBJECT"
    if cleaned["type"] == "OBJECT" and "properties" not in cleaned:
        cleaned["properties"] = {}
    
    return cleaned
```

### Tool 调用流程

```
┌─────────────────────────────────────────────────────────┐
│  1. User Request (带 tools定义)                        │
└────────────────┬────────────────────────────────────────┘
                 │
                 ▼
┌─────────────────────────────────────────────────────────┐
│  2. Model Response                                     │
│     - thinking块 (带signature)                         │
│     - tool_use块 (带id、name、input)                   │
│     - 存储signature到全局变量                          │
└────────────────┬────────────────────────────────────────┘
                 │
                 ▼
┌─────────────────────────────────────────────────────────┐
│  3. Client 执行 Tool                                   │
│     返回 tool_result                                    │
└────────────────┬────────────────────────────────────────┘
                 │
                 ▼
┌─────────────────────────────────────────────────────────┐
│  4. 下一次请求 (带tool_result)                         │
│     - 从全局变量获取signature                          │
│     - 附加到tool_result块                              │
│     - 或用于修复无效的thinking块                        │
└─────────────────────────────────────────────────────────┘
```

### Tool Result 处理

```python
# antigravity_proxy.py 1468-1472行
def build_tool_result_part(block, tool_id_to_name):
    """
    构建 tool_result
    """
    tool_use_id = block["tool_use_id"]
    func_name = block.get("name") or tool_id_to_name.get(tool_use_id)
    result = parse_tool_result(block.get("content"), block.get("is_error"))
    
    return {
        "functionResponse": {
            "name": func_name,
            "response": {"result": result},
            "id": tool_use_id
        }
    }
```

### 签名附加到 Tool Use

```python
# antigravity_proxy.py 1446-1466行
def build_tool_use_part(block):
    """
    构建 tool_use (附加签名)
    """
    part = {
        "functionCall": {
            "name": block["name"],
            "args": block["input"],
            "id": block["id"]
        }
    }
    
    # 签名优先级: 块签名 > 全局签名 > 虚拟签名
    block_sig = block.get("signature", "")
    global_sig = global_thought_signature_get()
    
    if block_sig and len(block_sig) >= MIN_SIGNATURE_LENGTH:
        part["thoughtSignature"] = block_sig
    elif global_sig and len(global_sig) >= MIN_SIGNATURE_LENGTH:
        print(f"[Tool] Using global signature (len={len(global_sig)})")
        part["thoughtSignature"] = global_sig
    elif allow_dummy:
        part["thoughtSignature"] = DUMMY_THOUGHT_SIGNATURE
    
    return part
```

---

## 🔄 并发问题分析

### 当前架构的并发限制

```python
# 1. 全局签名存储 - 非并发安全!
_global_thought_signature = None  # 单个全局变量

# 2. 单实例流处理器
class StreamingProcessor:
    def __init__(self):
        self.block_type = self.BLOCK_NONE  # 状态变量
        self.block_index = 0
        # ...
        
# 问题:
# - 多个并发请求会互相覆盖签名
# - 流处理器状态混乱
```

### 并发问题示例

```
时间线:
T1: Request A 开始, signature = "sig_A"
T2: Request A 存储 sig_A 到全局变量
T3: Request B 开始, signature = "sig_B"  
T4: Request B 存储 sig_B 到全局变量 (覆盖 sig_A!)
T5: Request A 的 tool_result 请求, 获取到 sig_B (错误!)
T6: Request A 失败 - signature 不匹配
```

### 解决方案

#### 方案1: 会话级存储 (推荐)

```python
from typing import Dict
import threading

class SignatureStore:
    """
    线程安全的签名存储
    """
    def __init__(self):
        self._store: Dict[str, str] = {}
        self._lock = threading.RLock()
    
    def store(self, session_id: str, signature: str):
        """存储签名"""
        with self._lock:
            if not self._store.get(session_id) or len(signature) > len(self._store[session_id]):
                self._store[session_id] = signature
                print(f"[Sig] Stored for session {session_id[:8]}")
    
    def get(self, session_id: str) -> Optional[str]:
        """获取签名"""
        with self._lock:
            return self._store.get(session_id)
    
    def clear(self, session_id: str):
        """清除签名"""
        with self._lock:
            self._store.pop(session_id, None)

# 全局实例
_signature_store = SignatureStore()

# 使用
session_id = generate_session_id(messages)  # 基于消息内容生成
_signature_store.store(session_id, signature)
```

#### 方案2: Redis 存储 (生产环境)

```python
import redis
import json

class RedisSignatureStore:
    """
    基于 Redis 的签名存储 - 支持分布式
    """
    def __init__(self, redis_url="redis://localhost:6379/0"):
        self.redis = redis.from_url(redis_url)
        self.ttl = 3600  # 1小时过期
    
    def store(self, session_id: str, signature: str):
        key = f"thought_sig:{session_id}"
        self.redis.setex(key, self.ttl, signature)
    
    def get(self, session_id: str) -> Optional[str]:
        key = f"thought_sig:{session_id}"
        value = self.redis.get(key)
        return value.decode() if value else None
    
    def clear(self, session_id: str):
        key = f"thought_sig:{session_id}"
        self.redis.delete(key)
```

#### 方案3: 请求上下文 (aiohttp)

```python
from contextvars import ContextVar

# 使用 contextvars 实现请求级隔离
_request_context: ContextVar[dict] = ContextVar('request_context', default={})

def store_signature_in_context(signature: str):
    """存储到当前请求上下文"""
    ctx = _request_context.get()
    ctx['thought_signature'] = signature
    _request_context.set(ctx)

def get_signature_from_context() -> Optional[str]:
    """从当前请求上下文获取"""
    ctx = _request_context.get()
    return ctx.get('thought_signature')
```

---

## 🌐 网络协议细节

### Cursor 的真实行为

根据代码分析和网络搜索,Cursor 的 API 调用特点:

1. **混合格式请求**
   - 使用 OpenAI 的 `/v1/chat/completions` 端点
   - 但请求体混合 OpenAI 和 Anthropic 格式
   - 自动根据响应格式判断协议

2. **知识库调用**
   ```json
   {
     "model": "cursor-kb",  // 特殊模型名
     "messages": [...],
     "context": {
       "files": [...],      // 选中的文件
       "codebase": true     // 是否使用代码库搜索
     }
   }
   ```

3. **Agent 模式**
   - Cursor Agent 会自主发起多轮对话
   - 每轮都包含 tool_use → tool_result 循环
   - 签名必须在多轮间保持

4. **流式优化**
   - Cursor 会并行显示多个块:
     ```
     [Thinking] ...
     [Text]     ...
     [Tool]     Calling function_name()
     ```
   - 需要正确的 SSE 事件顺序

### SSE 事件序列示例

```
# 正确的序列
event: message_start
data: {...}

event: content_block_start
data: {"index":0,"content_block":{"type":"thinking"}}

event: content_block_delta
data: {"index":0,"delta":{"type":"thinking_delta","thinking":"分析问题..."}}

event: content_block_delta
data: {"index":0,"delta":{"type":"signature_delta","signature":"..."}}

event: content_block_stop
data: {"index":0}

event: content_block_start
data: {"index":1,"content_block":{"type":"text"}}

event: content_block_delta
data: {"index":1,"delta":{"type":"text_delta","text":"根据分析..."}}

event: content_block_stop
data: {"index":1}

event: content_block_start
data: {"index":2,"content_block":{"type":"tool_use","id":"...","name":"..."}}

event: content_block_delta
data: {"index":2,"delta":{"type":"input_json_delta","partial_json":"..."}}

event: content_block_stop
data: {"index":2}

event: message_delta
data: {"delta":{"stop_reason":"tool_use"}}

event: message_stop
data: {"type":"message_stop"}
```

### Cursor 私有协议?

**结论: 没有私有协议,但有特殊约定**

1. **不是私有协议**
   - Cursor 遵循 OpenAI + Anthropic 公开标准
   - SSE 格式标准化
   
2. **特殊约定**
   - ✅ 混合格式要求同时支持两种协议
   - ✅ `reasoning_content` 字段(OpenAI o1 引入)
   - ✅ Thinking 块必须在最前
   - ✅ Tool 签名传递机制
   
3. **知识库扩展**
   - 可能有额外的 `context` 字段
   - 但不影响基本协议

---

## 📊 改进建议

### 1. Go 版本改进

#### 添加并发安全的签名存储

```go
// internal/adapters/signature_store.go
package adapters

import (
    "sync"
    "time"
)

type SignatureStore struct {
    store map[string]*SignatureEntry
    mu    sync.RWMutex
}

type SignatureEntry struct {
    Signature string
    ExpiresAt time.Time
}

var globalStore = &SignatureStore{
    store: make(map[string]*SignatureEntry),
}

func StoreSignatureForSession(sessionID, signature string) {
    globalStore.mu.Lock()
    defer globalStore.mu.Unlock()
    
    globalStore.store[sessionID] = &SignatureEntry{
        Signature: signature,
        ExpiresAt: time.Now().Add(1 * time.Hour),
    }
    
    log.Debugf("[Sig] Stored for session %s", sessionID[:8])
}

func GetSignatureForSession(sessionID string) string {
    globalStore.mu.RLock()
    defer globalStore.mu.RUnlock()
    
    entry, ok := globalStore.store[sessionID]
    if !ok || time.Now().After(entry.ExpiresAt) {
        return ""
    }
    
    return entry.Signature
}

// 后台清理过期签名
func init() {
    go func() {
        ticker := time.NewTicker(10 * time.Minute)
        for range ticker.C {
            globalStore.mu.Lock()
            for id, entry := range globalStore.store {
                if time.Now().After(entry.ExpiresAt) {
                    delete(globalStore.store, id)
                }
            }
            globalStore.mu.Unlock()
        }
    }()
}
```

#### 改进 Cursor Adapter

```go
// internal/adapters/cursor_adapter.go

func (a *CursorAdapter) AdaptRequest(reqData map[string]interface{}, model string) (map[string]interface{}, error) {
    // 1. 生成会话 ID
    sessionID := generateSessionID(reqData)
    
    // 2. 获取会话签名
    storedSig := GetSignatureForSession(sessionID)
    
    // 3. 转换消息
    messages := convertMessages(reqData["messages"], storedSig)
    
    // 4. 过滤无效 thinking 块
    filtered := FilterInvalidThinkingBlocks(messages)
    if filtered > 0 {
        log.Debugf("[Cursor] Filtered %d invalid thinking blocks", filtered)
    }
    
    // ... 其余转换逻辑
    
    return converted, nil
}

func (a *CursorAdapter) AdaptStreamChunk(chunk map[string]interface{}) (map[string]interface{}, error) {
    // 提取并存储签名
    if sig := extractSignature(chunk); sig != "" && len(sig) >= MinSignatureLength {
        sessionID := a.currentSessionID  // 需要在结构体中保存
        StoreSignatureForSession(sessionID, sig)
    }
    
    return chunk, nil
}
```

### 2. Python 版本改进

#### 修复并发问题

```python
# py/antigravity_proxy.py

import asyncio
from typing import Dict, Optional
from dataclasses import dataclass, field
from datetime import datetime, timedelta

@dataclass
class SignatureEntry:
    signature: str
    expires_at: datetime
    created_at: datetime = field(default_factory=datetime.now)

class SessionSignatureStore:
    """会话级签名存储 - 线程/协程安全"""
    
    def __init__(self, ttl_seconds: int = 3600):
        self._store: Dict[str, SignatureEntry] = {}
        self._lock = asyncio.Lock()
        self.ttl = timedelta(seconds=ttl_seconds)
    
    async def store(self, session_id: str, signature: str):
        async with self._lock:
            entry = self._store.get(session_id)
            # 只存储更长的签名
            if not entry or len(signature) > len(entry.signature):
                self._store[session_id] = SignatureEntry(
                    signature=signature,
                    expires_at=datetime.now() + self.ttl
                )
                debug_print(f"[Sig] Stored for session {session_id[:8]}")
    
    async def get(self, session_id: str) -> Optional[str]:
        async with self._lock:
            entry = self._store.get(session_id)
            if not entry:
                return None
            if datetime.now() > entry.expires_at:
                del self._store[session_id]
                return None
            return entry.signature
    
    async def cleanup_expired(self):
        """清理过期条目"""
        async with self._lock:
            now = datetime.now()
            expired = [
                sid for sid, entry in self._store.items()
                if now > entry.expires_at
            ]
            for sid in expired:
                del self._store[sid]
            if expired:
                debug_print(f"[Sig] Cleaned {len(expired)} expired entries")

# 全局实例
_session_signature_store = SessionSignatureStore()

# 后台清理任务
async def cleanup_task():
    while True:
        await asyncio.sleep(600)  # 每10分钟
        await _session_signature_store.cleanup_expired()

# 启动时启动清理任务
asyncio.create_task(cleanup_task())
```

#### 改进请求处理

```python
class APIServer:
    async def handle_cursor(self, request: web.Request):
        # 1. 生成/获取会话 ID
        body = await request.json()
        session_id = generate_session_id(body.get("messages", []))
        request["session_id"] = session_id  # 保存到请求
        
        # 2. 获取会话签名
        stored_sig = await _session_signature_store.get(session_id)
        if stored_sig:
            debug_print(f"[Cursor] Using stored sig for session {session_id[:8]}")
        
        # 3. 转换请求(传入签名)
        claude_req = self._convert_request(body, stored_sig)
        
        # 4. 处理响应
        if body.get("stream"):
            return await self._handle_streaming(request, claude_req)
        else:
            return await self._handle_non_streaming(request, claude_req)
    
    async def _handle_streaming(self, request, claude_req):
        session_id = request["session_id"]
        
        # ... 上游请求 ...
        
        processor = StreamingProcessor(original_model)
        
        async for chunk in upstream_response:
            # 提取签名并存储
            if sig := extract_signature(chunk):
                await _session_signature_store.store(session_id, sig)
            
            # 处理块
            output = processor.process(chunk)
            if output:
                await response.write(output.encode())
        
        return response
```

### 3. 统一改进

#### 更好的格式检测

```python
def detect_request_format(body: dict) -> str:
    """
    更准确的格式检测
    
    返回: "openai", "anthropic", "cursor", "gemini"
    """
    # 1. 检查端点特征
    # (在路由层已经知道了)
    
    # 2. 检查消息格式
    messages = body.get("messages", [])
    has_content_blocks = False
    has_tool_result = False
    has_thinking = False
    
    for msg in messages:
        content = msg.get("content")
        if isinstance(content, list):
            has_content_blocks = True
            for block in content:
                if isinstance(block, dict):
                    bt = block.get("type", "")
                    if bt == "tool_result":
                        has_tool_result = True
                    elif bt == "thinking":
                        has_thinking = True
    
    # 3. 检查工具格式
    tools = body.get("tools", [])
    has_anthropic_tools = any(
        t.get("name") and not t.get("type")
        for t in tools
    )
    
    # 4. 决策
    if has_thinking or (has_content_blocks and has_anthropic_tools):
        return "cursor"  # Cursor 混合格式
    elif has_content_blocks or has_tool_result:
        return "anthropic"
    else:
        return "openai"
```

#### 增强日志

```python
def log_request_details(req: dict, format: str):
    """详细的请求日志"""
    print(f"\n{'='*60}")
    print(f"[Request] Format: {format}")
    print(f"[Request] Model: {req.get('model')}")
    print(f"[Request] Stream: {req.get('stream')}")
    
    messages = req.get("messages", [])
    print(f"[Request] Messages: {len(messages)}")
    
    for i, msg in enumerate(messages):
        role = msg.get("role")
        content = msg.get("content")
        
        if isinstance(content, str):
            preview = content[:50] + "..." if len(content) > 50 else content
            print(f"  [{i}] {role}: \"{preview}\"")
        elif isinstance(content, list):
            types = [b.get("type") for b in content if isinstance(b, dict)]
            print(f"  [{i}] {role}: [{', '.join(types)}]")
    
    tools = req.get("tools", [])
    if tools:
        print(f"[Request] Tools: {len(tools)}")
        for i, t in enumerate(tools[:3]):
            name = t.get("name") or t.get("function", {}).get("name")
            print(f"  [{i}] {name}")
    
    print(f"{'='*60}\n")
```

---

## 🎓 总结

### 关键要点

1. **Cursor 不是私有协议**
   - 基于 OpenAI + Anthropic 开放标准
   - 混合使用两种格式的特性
   
2. **核心转换链路**
   ```
   检测格式 → 统一到Claude格式 → 后端适配 → 流式输出 → 目标格式
   ```

3. **Thinking 机制**
   - Thought signature 是关键
   - 需要跨请求传递
   - 必须处理验证和修复

4. **并发问题**
   - 当前代码不支持并发
   - 需要会话级签名存储
   - Go 和 Python 都需要改进

5. **工具调用**
   - 签名附加机制复杂
   - Schema 清理很重要
   - 多轮对话要保持上下文

### 实现建议优先级

**高优先级**
1. ✅ 会话级签名存储(修复并发)
2. ✅ 格式自动检测增强
3. ✅ Thinking 块验证和修复

**中优先级**
4. ✅ 详细日志和调试输出
5. ✅ 错误处理改进
6. ✅ Schema 清理优化

**低优先级**
7. ✅ Redis 分布式存储
8. ✅ 性能监控和指标
9. ✅ 单元测试覆盖

---

## 📚 参考资料

1. **OpenAI API 文档**
   - Chat Completions: https://platform.openai.com/docs/api-reference/chat
   - Streaming: https://platform.openai.com/docs/api-reference/streaming
   - Reasoning (o1): https://platform.openai.com/docs/guides/reasoning

2. **Anthropic Claude API**
   - Messages API: https://docs.anthropic.com/claude/reference/messages
   - Thinking: https://docs.anthropic.com/claude/docs/thinking-beta
   - Tool Use: https://docs.anthropic.com/claude/docs/tool-use

3. **Cursor 相关**
   - Agent CLI: https://cursor.com/docs/agent-cli
   - Streaming formats: SSE standard

4. **协议标准**
   - SSE Specification: https://html.spec.whatwg.org/multipage/server-sent-events.html
   - JSON Schema: https://json-schema.org/

---

**文档结束**
