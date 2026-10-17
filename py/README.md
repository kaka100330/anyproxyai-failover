# Antigravity API Proxy (Python)

将 Claude/OpenAI API 请求转换为 Antigravity (Gemini v1internal) 格式的代理服务器。

## 功能特性

- **双接口支持**: 同时支持 OpenAI 和 Anthropic Claude API 格式
- **协议转换**: Claude/OpenAI → Gemini v1internal
- **流式支持**: 支持 SSE 流式响应
- **配置文件**: 通过 config.json 配置端口和 API Key
- **模型映射**: 自动映射 Claude/Gemini/GPT 模型名称
- **工具调用**: 支持 function calling / tool use
- **URL 降级**: 自动 fallback 到备用 API 端点

## 安装依赖

```bash
pip install aiohttp
```

## 配置

在 `config.json` 中添加以下配置：

```json
{
  "antigravity_host": "0.0.0.0",
  "antigravity_port": 8080,
  "antigravity_api_key": "sk-your-api-key",
  "antigravity_refresh_token": "your-google-oauth-refresh-token",
  "antigravity_project_id": ""
}
```

或使用现有配置字段：
- `host` / `antigravity_host` - 绑定地址
- `port` / `antigravity_port` - 绑定端口
- `local_api_key` / `antigravity_api_key` - API 密钥

## 使用方法

### 启动代理服务器

```bash
# 使用 config.json 配置
python antigravity_proxy.py

# 或命令行参数覆盖
python antigravity_proxy.py --refresh-token YOUR_TOKEN --port 8080 --api-key sk-xxx
```

## API 端点

### Anthropic Claude API

| 端点 | 方法 | 说明 |
|------|------|------|
| `/v1/messages` | POST | Claude Messages API |

### OpenAI API

| 端点 | 方法 | 说明 |
|------|------|------|
| `/v1/chat/completions` | POST | OpenAI Chat Completions API |

### 通用

| 端点 | 方法 | 说明 |
|------|------|------|
| `/v1/models` | GET | 列出支持的模型 |
| `/health` | GET | 健康检查 |

## 请求示例

### Anthropic Claude API

```bash
curl -X POST http://localhost:8080/v1/messages \
  -H "Content-Type: application/json" \
  -H "x-api-key: sk-your-api-key" \
  -d '{
    "model": "claude-sonnet-4-5",
    "messages": [{"role": "user", "content": "Hello!"}],
    "max_tokens": 1024
  }'
```

### OpenAI API

```bash
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-your-api-key" \
  -d '{
    "model": "gpt-4",
    "messages": [{"role": "user", "content": "Hello!"}],
    "max_tokens": 1024
  }'
```

### 流式请求

```bash
# Claude API
curl -X POST http://localhost:8080/v1/messages \
  -H "Content-Type: application/json" \
  -H "x-api-key: sk-your-api-key" \
  -d '{"model": "claude-sonnet-4-5", "messages": [{"role": "user", "content": "Write a poem"}], "stream": true}'

# OpenAI API
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-your-api-key" \
  -d '{"model": "gpt-4", "messages": [{"role": "user", "content": "Write a poem"}], "stream": true}'
```

## 支持的模型

### Claude 模型
- `claude-opus-4-5-thinking`
- `claude-sonnet-4-5`
- `claude-sonnet-4-5-thinking`

### Gemini 模型
- `gemini-2.5-flash`
- `gemini-2.5-flash-lite`
- `gemini-2.5-flash-thinking`
- `gemini-3-flash`
- `gemini-3-pro-low`
- `gemini-3-pro-high`
- `gemini-3-pro-image`

### OpenAI 模型映射
- `gpt-4*` → `claude-sonnet-4-5`
- `gpt-3.5*` → `gemini-2.5-flash`

## 认证方式

支持两种认证方式：

1. **x-api-key Header** (Anthropic 风格)
   ```
   x-api-key: sk-your-api-key
   ```

2. **Authorization Header** (OpenAI 风格)
   ```
   Authorization: Bearer sk-your-api-key
   ```

## License

MIT
