# Cursor IDE 兼容性实现指南

本文档描述了如何为 Kiro Gateway 添加 Cursor IDE 支持的完整实现细节。

## 问题背景

Cursor IDE 使用的 API 格式与标准 OpenAI 格式有以下差异：

### 1. Tool 定义格式不同

**标准 OpenAI 格式（嵌套）：**
```json
{
  "type": "function",
  "function": {
    "name": "tool_name",
    "description": "...",
    "parameters": {...}
  }
}
```

**Cursor 格式（扁平）：**
```json
{
  "name": "tool_name",
  "description": "...",
  "input_schema": {...}
}
```

### 2. Tool Calls 位置不同

**标准 OpenAI 格式：**
- Assistant 消息有 `tool_calls` 字段

**Cursor/Anthropic 格式：**
- Assistant 消息的 `content` 是数组，包含 `tool_use` 块：
```json
{
  "role": "assistant",
  "content": [
    {"type": "text", "text": "..."},
    {"type": "tool_use", "id": "xxx", "name": "tool_name", "input": {...}}
  ]
}
```

### 3. Tool Results 位置不同

**标准 OpenAI 格式：**
- 使用 `role: "tool"` 的独立消息

**Cursor/Anthropic 格式：**
- User 消息的 `content` 数组中包含 `tool_result` 块：
```json
{
  "role": "user",
  "content": [
    {"type": "tool_result", "tool_use_id": "xxx", "content": "..."}
  ]
}
```

---

## 实现修改

### 文件 1: `models_openai.py`

修改 `Tool` 模型以支持两种格式：

```python
class Tool(BaseModel):
    """
    Tool in OpenAI format.
    
    Supports both standard OpenAI format and Cursor's flat format.
    """
    # OpenAI standard format
    type: Optional[str] = "function"
    function: Optional[ToolFunction] = None
    
    # Cursor flat format fields
    name: Optional[str] = None
    description: Optional[str] = None
    input_schema: Optional[Dict[str, Any]] = None
    
    model_config = {"extra": "allow"}
```

**关键点：**
- `function` 改为 `Optional`，允许 Cursor 格式不提供此字段
- 添加 `name`, `description`, `input_schema` 字段支持 Cursor 扁平格式
- 使用 `extra = "allow"` 允许额外字段

---

### 文件 2: `converters_openai.py`

#### 2.1 修改 `convert_openai_tools_to_unified` 函数

```python
def convert_openai_tools_to_unified(tools: Optional[List[Tool]]) -> Optional[List[UnifiedTool]]:
    if not tools:
        return None
    
    unified_tools = []
    for tool in tools:
        # Check if this is Cursor flat format (has name directly on tool)
        if tool.name:
            # Cursor flat format
            unified_tools.append(UnifiedTool(
                name=tool.name,
                description=tool.description,
                input_schema=tool.input_schema
            ))
        elif tool.function:
            # Standard OpenAI format
            if tool.type and tool.type != "function":
                continue
            
            unified_tools.append(UnifiedTool(
                name=tool.function.name,
                description=tool.function.description,
                input_schema=tool.function.parameters
            ))
    
    return unified_tools if unified_tools else None
```

**关键点：**
- 先检查 `tool.name` 判断是否为 Cursor 格式
- Cursor 格式使用 `input_schema`，OpenAI 格式使用 `function.parameters`

#### 2.2 修改 `_extract_tool_calls_from_openai` 函数

```python
def _extract_tool_calls_from_openai(msg: ChatMessage) -> List[Dict[str, Any]]:
    tool_calls = []
    
    # From tool_calls field (OpenAI format)
    if msg.tool_calls:
        for tc in msg.tool_calls:
            if isinstance(tc, dict):
                tool_calls.append({
                    "id": tc.get("id", ""),
                    "type": "function",
                    "function": {
                        "name": tc.get("function", {}).get("name", ""),
                        "arguments": tc.get("function", {}).get("arguments", "{}")
                    }
                })
    
    # From content blocks (Anthropic/Cursor format)
    if isinstance(msg.content, list):
        for item in msg.content:
            if isinstance(item, dict) and item.get("type") == "tool_use":
                tool_calls.append({
                    "id": item.get("id", ""),
                    "type": "function",
                    "function": {
                        "name": item.get("name", ""),
                        "arguments": item.get("input", {})
                    }
                })
    
    return tool_calls
```

**关键点：**
- 同时检查 `msg.tool_calls` 和 `msg.content` 中的 `tool_use` 块
- Cursor 的 `tool_use` 使用 `id` 和 `input` 字段

---

### 文件 3: `converters_core.py`

#### 3.1 添加 `convert_tool_results_to_kiro_format` 函数

```python
def convert_tool_results_to_kiro_format(tool_results: List[Dict[str, Any]]) -> List[Dict[str, Any]]:
    """
    Converts tool results from unified/OpenAI format to Kiro API format.
    
    Input format (OpenAI/unified):
    {
        "type": "tool_result",
        "tool_use_id": "...",
        "content": "..."
    }
    
    Output format (Kiro):
    {
        "content": [{"text": "..."}],
        "status": "success",
        "toolUseId": "..."
    }
    """
    kiro_results = []
    
    for result in tool_results:
        # Check if already in Kiro format
        if "toolUseId" in result:
            kiro_results.append(result)
            continue
        
        # Convert from OpenAI/unified format
        content_text = result.get("content", "")
        if isinstance(content_text, list):
            content = content_text
        else:
            content = [{"text": str(content_text) if content_text else "(empty result)"}]
        
        kiro_results.append({
            "content": content,
            "status": "success",
            "toolUseId": result.get("tool_use_id", result.get("toolUseId", ""))
        })
    
    return kiro_results
```

#### 3.2 修改 `sanitize_json_schema` 函数

```python
def sanitize_json_schema(schema: Optional[Dict[str, Any]]) -> Dict[str, Any]:
    if not schema:
        return {}
    
    # Fields that Kiro API doesn't accept
    SKIP_FIELDS = {
        "additionalProperties",
        "$schema",
        "title",
        "default",
    }
    
    result = {}
    
    for key, value in schema.items():
        # Skip empty required arrays
        if key == "required" and isinstance(value, list) and len(value) == 0:
            continue
        
        # Skip unsupported fields
        if key in SKIP_FIELDS:
            continue
        
        # Handle anyOf - simplify by taking first non-null option
        if key == "anyOf" and isinstance(value, list):
            for option in value:
                if isinstance(option, dict):
                    if option.get("type") == "null" or "not" in option:
                        continue
                    sanitized_option = sanitize_json_schema(option)
                    result.update(sanitized_option)
                    break
            continue
        
        # Recursively process nested objects
        if key == "properties" and isinstance(value, dict):
            result[key] = {
                prop_name: sanitize_json_schema(prop_value) if isinstance(prop_value, dict) else prop_value
                for prop_name, prop_value in value.items()
            }
        elif isinstance(value, dict):
            result[key] = sanitize_json_schema(value)
        elif isinstance(value, list):
            result[key] = [
                sanitize_json_schema(item) if isinstance(item, dict) else item
                for item in value
            ]
        else:
            result[key] = value
    
    return result
```

**关键点：**
- 移除 Kiro 不支持的字段：`additionalProperties`, `$schema`, `title`, `default`
- 处理 `anyOf` 联合类型，简化为第一个非 null 选项

#### 3.3 修改 `build_kiro_history` 和 `build_kiro_payload`

在处理 `tool_results` 时调用转换函数：

```python
# In build_kiro_history:
tool_results = msg.tool_results or extract_tool_results_from_content(msg.content)
if tool_results:
    kiro_tool_results = convert_tool_results_to_kiro_format(tool_results)
    user_input["userInputMessageContext"] = {"toolResults": kiro_tool_results}

# In build_kiro_payload:
tool_results = current_message.tool_results or extract_tool_results_from_content(current_message.content)
if tool_results:
    kiro_tool_results = convert_tool_results_to_kiro_format(tool_results)
    user_input_context["toolResults"] = kiro_tool_results
```

---

## 格式转换流程

```
Cursor 请求
    │
    ▼
┌─────────────────────────────────────┐
│  Tool 定义转换                       │
│  {name, input_schema} → UnifiedTool │
└─────────────────────────────────────┘
    │
    ▼
┌─────────────────────────────────────┐
│  Tool Calls 提取                     │
│  content[tool_use] → tool_calls     │
└─────────────────────────────────────┘
    │
    ▼
┌─────────────────────────────────────┐
│  Tool Results 转换                   │
│  {tool_use_id} → {toolUseId}        │
└─────────────────────────────────────┘
    │
    ▼
┌─────────────────────────────────────┐
│  JSON Schema 清理                    │
│  移除 anyOf, title, default 等      │
└─────────────────────────────────────┘
    │
    ▼
Kiro API 请求
```

---

## 测试验证

修改后应支持以下场景：

1. **标准 OpenAI 客户端** - 继续正常工作
2. **Cursor IDE** - 使用扁平 tool 格式和 content 中的 tool_use/tool_result
3. **Anthropic SDK** - 使用 Anthropic 风格的消息格式

---

## 注意事项

1. 修改是向后兼容的，不影响现有 OpenAI 格式的请求
2. 无需创建单独的 `/cursor/v1` 路由，现有 `/v1/chat/completions` 自动兼容
3. Tool 定义中的 `input_schema` 和 `parameters` 都会被正确处理
