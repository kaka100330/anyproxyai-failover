package adapters

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ClaudeToOpenAIAdapter 将 Claude (Anthropic) 格式转换为 OpenAI 格式
type ClaudeToOpenAIAdapter struct {
	// 用于在流式响应中跟踪当前的 tool_use ID 和名称
	currentToolCallID   string // 当前正在处理的 tool_use ID
	currentToolCallName string // 当前正在处理的 tool_use name
}

func init() {
	RegisterAdapter("claude-to-openai", &ClaudeToOpenAIAdapter{})
}

// AdaptRequest 将 Claude 请求转换为 OpenAI 请求
func (a *ClaudeToOpenAIAdapter) AdaptRequest(reqData map[string]interface{}, model string) (map[string]interface{}, error) {
	// Claude 请求格式示例:
	// {
	//   "model": "claude-3-haiku-20240307",
	//   "max_tokens": 1000,
	//   "messages": [
	//     {"role": "user", "content": "Hello"}
	//   ]
	// }
	//
	// Claude Code 的 system 可能是数组格式（带 cache_control）:
	// {
	//   "system": [
	//     {"type": "text", "text": "You are Claude Code...", "cache_control": {"type": "ephemeral"}},
	//     {"type": "text", "text": "Additional instructions...", "cache_control": {"type": "ephemeral"}}
	//   ]
	// }
	//
	// OpenAI 请求格式示例:
	// {
	//   "model": "gpt-3.5-turbo",
	//   "messages": [
	//     {"role": "user", "content": "Hello"}
	//   ],
	//   "max_tokens": 1000
	// }

	openaiReq := make(map[string]interface{})

	// 复制模型名
	openaiReq["model"] = model

	// 处理 system 参数 - Claude 支持单独的 system 字段（字符串或数组），OpenAI 需要作为第一条消息
	systemMessage := convertClaudeSystemToString(reqData["system"])

	// 转换消息格式 - Claude 和 OpenAI 的消息格式基本相同
	if messages, ok := reqData["messages"].([]interface{}); ok {
		openaiMessages := make([]interface{}, 0, len(messages)+1)

		// 如果有 system 消息，添加为第一条消息
		if systemMessage != "" {
			openaiMessages = append(openaiMessages, map[string]interface{}{
				"role":    "system",
				"content": systemMessage,
			})
		}

		for _, msg := range messages {
			if msgMap, ok := msg.(map[string]interface{}); ok {
				role, _ := msgMap["role"].(string)
				content := msgMap["content"]

				// 处理 tool_result - 转换为 OpenAI 的 tool 角色消息
				if role == "user" {
					if contentArr, isArray := content.([]interface{}); isArray {
						// 检查是否包含 tool_result
						convertedMsgs := convertClaudeUserMessage(contentArr)
						openaiMessages = append(openaiMessages, convertedMsgs...)
						continue
					}
				}

				// 处理 assistant 消息 - 可能包含 tool_use
				if role == "assistant" {
					if contentArr, isArray := content.([]interface{}); isArray {
						assistantMsg := convertClaudeAssistantMessage(contentArr)
						openaiMessages = append(openaiMessages, assistantMsg)
						continue
					}
				}

				openaiMsg := make(map[string]interface{})
				openaiMsg["role"] = role

				// 处理 content - 可能是字符串或数组
				switch v := content.(type) {
				case string:
					// 简单的文本内容
					openaiMsg["content"] = v
				case []interface{}:
					// 多模态内容 - Claude 格式
					// [{"type": "text", "text": "..."}]
					// 转换为 OpenAI 格式的文本
					var textContent string
					for _, part := range v {
						if partMap, ok := part.(map[string]interface{}); ok {
							if partMap["type"] == "text" {
								if text, ok := partMap["text"].(string); ok {
									textContent += text
								}
							}
						}
					}
					openaiMsg["content"] = textContent
				default:
					openaiMsg["content"] = fmt.Sprintf("%v", v)
				}

				openaiMessages = append(openaiMessages, openaiMsg)
			}
		}

		openaiReq["messages"] = openaiMessages
	}

	// 转换 tools - Claude 格式转 OpenAI 格式
	if tools, ok := reqData["tools"].([]interface{}); ok && len(tools) > 0 {
		openaiTools := convertClaudeToolsToOpenAI(tools)
		if len(openaiTools) > 0 {
			openaiReq["tools"] = openaiTools
		}
	}

	// 转换 tool_choice
	if toolChoice := reqData["tool_choice"]; toolChoice != nil {
		openaiReq["tool_choice"] = convertClaudeToolChoiceToOpenAI(toolChoice)
	}

	// 转换其他参数
	if maxTokens, ok := reqData["max_tokens"]; ok {
		openaiReq["max_tokens"] = maxTokens
	}

	if temperature, ok := reqData["temperature"]; ok {
		openaiReq["temperature"] = temperature
	}

	if topP, ok := reqData["top_p"]; ok {
		openaiReq["top_p"] = topP
	}

	// 处理流式参数
	if stream, ok := reqData["stream"]; ok {
		openaiReq["stream"] = stream
		// 启用 usage 统计
		if streamBool, ok := stream.(bool); ok && streamBool {
			openaiReq["stream_options"] = map[string]interface{}{
				"include_usage": true,
			}
		}
	}

	// 处理 stop sequences
	if stopSequences, ok := reqData["stop_sequences"]; ok {
		openaiReq["stop"] = stopSequences
	}

	// 注意：不要转发以下 Claude 特有的字段，因为 OpenAI API 不支持：
	// - metadata (Claude 特有)
	// - anthropic_version (Claude 特有)
	// - system (已经合并到 messages 中)
	// - cache_control (Claude Code 特有，忽略)

	return openaiReq, nil
}

// convertClaudeSystemToString 将 Claude system 参数转换为字符串
// 支持字符串格式和数组格式（Claude Code 带 cache_control）
func convertClaudeSystemToString(system interface{}) string {
	if system == nil {
		return ""
	}

	switch sys := system.(type) {
	case string:
		return sys
	case []interface{}:
		// Claude Code 的 system 数组格式
		// [{"type": "text", "text": "...", "cache_control": {"type": "ephemeral"}}]
		var textParts []string
		for _, block := range sys {
			if blockMap, ok := block.(map[string]interface{}); ok {
				if blockType, ok := blockMap["type"].(string); ok && blockType == "text" {
					if text, ok := blockMap["text"].(string); ok {
						textParts = append(textParts, text)
					}
				}
			}
		}
		return strings.Join(textParts, "\n\n")
	default:
		return fmt.Sprintf("%v", sys)
	}
}

// convertClaudeUserMessage 转换包含 tool_result 的用户消息
func convertClaudeUserMessage(contentArr []interface{}) []interface{} {
	result := make([]interface{}, 0)
	var textParts []string

	for _, block := range contentArr {
		blockMap, ok := block.(map[string]interface{})
		if !ok {
			continue
		}

		blockType, _ := blockMap["type"].(string)

		switch blockType {
		case "tool_result":
			// 转换为 OpenAI 的 tool 角色消息
			toolUseID, _ := blockMap["tool_use_id"].(string)
			content := extractClaudeToolResultContent(blockMap["content"])

			result = append(result, map[string]interface{}{
				"role":         "tool",
				"tool_call_id": toolUseID,
				"content":      content,
			})

		case "text":
			if text, ok := blockMap["text"].(string); ok && text != "" {
				textParts = append(textParts, text)
			}
		}
	}

	// 如果有文本内容，添加为用户消息
	if len(textParts) > 0 {
		result = append(result, map[string]interface{}{
			"role":    "user",
			"content": strings.Join(textParts, "\n"),
		})
	}

	return result
}

// convertClaudeAssistantMessage 转换包含 tool_use 的助手消息
func convertClaudeAssistantMessage(contentArr []interface{}) map[string]interface{} {
	assistantMsg := map[string]interface{}{
		"role": "assistant",
	}

	var textParts []string
	var toolCalls []interface{}

	for _, block := range contentArr {
		blockMap, ok := block.(map[string]interface{})
		if !ok {
			continue
		}

		blockType, _ := blockMap["type"].(string)

		switch blockType {
		case "tool_use":
			// 转换为 OpenAI 的 tool_calls 格式
			id, _ := blockMap["id"].(string)
			name, _ := blockMap["name"].(string)
			input := blockMap["input"]

			// 将 input 转为 JSON 字符串
			var arguments string
			if input != nil {
				if inputBytes, err := json.Marshal(input); err == nil {
					arguments = string(inputBytes)
				}
			}

			toolCalls = append(toolCalls, map[string]interface{}{
				"id":   id,
				"type": "function",
				"function": map[string]interface{}{
					"name":      name,
					"arguments": arguments,
				},
			})

		case "text":
			if text, ok := blockMap["text"].(string); ok && text != "" {
				textParts = append(textParts, text)
			}
		}
	}

	if len(textParts) > 0 {
		assistantMsg["content"] = strings.Join(textParts, "\n")
	}

	if len(toolCalls) > 0 {
		assistantMsg["tool_calls"] = toolCalls
	}

	return assistantMsg
}

// extractClaudeToolResultContent 提取 tool_result 的内容
func extractClaudeToolResultContent(content interface{}) string {
	if content == nil {
		return "(empty result)"
	}

	switch c := content.(type) {
	case string:
		if c == "" {
			return "(empty result)"
		}
		return c
	case []interface{}:
		var parts []string
		for _, item := range c {
			if itemMap, ok := item.(map[string]interface{}); ok {
				if itemMap["type"] == "text" {
					if text, ok := itemMap["text"].(string); ok {
						parts = append(parts, text)
					}
				} else {
					// 其他类型序列化为 JSON
					if jsonBytes, err := json.Marshal(itemMap); err == nil {
						parts = append(parts, string(jsonBytes))
					}
				}
			} else if str, ok := item.(string); ok {
				parts = append(parts, str)
			}
		}
		result := strings.Join(parts, "\n")
		if result == "" {
			return "(empty result)"
		}
		return result
	case map[string]interface{}:
		if c["type"] == "text" {
			if text, ok := c["text"].(string); ok {
				return text
			}
		}
		if jsonBytes, err := json.Marshal(c); err == nil {
			return string(jsonBytes)
		}
	}

	return fmt.Sprintf("%v", content)
}

// convertClaudeToolsToOpenAI 将 Claude 工具定义转换为 OpenAI 格式
func convertClaudeToolsToOpenAI(tools []interface{}) []interface{} {
	openaiTools := make([]interface{}, 0, len(tools))

	for _, tool := range tools {
		toolMap, ok := tool.(map[string]interface{})
		if !ok {
			continue
		}

		name, _ := toolMap["name"].(string)
		description, _ := toolMap["description"].(string)
		inputSchema := toolMap["input_schema"]

		// 清理 JSON Schema
		cleanedSchema := sanitizeClaudeJSONSchema(inputSchema)

		openaiTools = append(openaiTools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        name,
				"description": description,
				"parameters":  cleanedSchema,
			},
		})
	}

	return openaiTools
}

// convertClaudeToolChoiceToOpenAI 将 Claude tool_choice 转换为 OpenAI 格式
func convertClaudeToolChoiceToOpenAI(toolChoice interface{}) interface{} {
	switch tc := toolChoice.(type) {
	case string:
		return tc
	case map[string]interface{}:
		if tcType, ok := tc["type"].(string); ok {
			switch tcType {
			case "auto":
				return "auto"
			case "any":
				return "required"
			case "tool":
				if name, ok := tc["name"].(string); ok {
					return map[string]interface{}{
						"type": "function",
						"function": map[string]interface{}{
							"name": name,
						},
					}
				}
			}
		}
		// 可能已经是 OpenAI 格式
		if tc["type"] == "function" {
			return tc
		}
	}
	return "auto"
}

// sanitizeClaudeJSONSchema 清理 JSON Schema，移除不支持的字段
func sanitizeClaudeJSONSchema(schema interface{}) interface{} {
	if schema == nil {
		return map[string]interface{}{}
	}

	schemaMap, ok := schema.(map[string]interface{})
	if !ok {
		return schema
	}

	// 需要跳过的字段
	skipFields := map[string]bool{
		"additionalProperties": true,
		"$schema":              true,
		"title":                true,
		"default":              true,
	}

	result := make(map[string]interface{})

	for key, value := range schemaMap {
		// 跳过空的 required 数组
		if key == "required" {
			if arr, ok := value.([]interface{}); ok && len(arr) == 0 {
				continue
			}
		}

		// 跳过不支持的字段
		if skipFields[key] {
			continue
		}

		// 处理 anyOf - 简化为第一个非 null 选项
		if key == "anyOf" {
			if anyOfArr, ok := value.([]interface{}); ok {
				for _, option := range anyOfArr {
					if optionMap, ok := option.(map[string]interface{}); ok {
						// 跳过 null 类型和 not 约束
						if optionMap["type"] == "null" {
							continue
						}
						if _, hasNot := optionMap["not"]; hasNot {
							continue
						}
						// 使用第一个有效选项
						sanitized := sanitizeClaudeJSONSchema(optionMap)
						if sanitizedMap, ok := sanitized.(map[string]interface{}); ok {
							for k, v := range sanitizedMap {
								result[k] = v
							}
						}
						break
					}
				}
				continue
			}
		}

		// 递归处理嵌套对象
		if key == "properties" {
			if propsMap, ok := value.(map[string]interface{}); ok {
				sanitizedProps := make(map[string]interface{})
				for propName, propValue := range propsMap {
					sanitizedProps[propName] = sanitizeClaudeJSONSchema(propValue)
				}
				result[key] = sanitizedProps
				continue
			}
		}

		// 递归处理其他嵌套对象
		if valueMap, ok := value.(map[string]interface{}); ok {
			result[key] = sanitizeClaudeJSONSchema(valueMap)
		} else if valueArr, ok := value.([]interface{}); ok {
			sanitizedArr := make([]interface{}, len(valueArr))
			for i, item := range valueArr {
				if itemMap, ok := item.(map[string]interface{}); ok {
					sanitizedArr[i] = sanitizeClaudeJSONSchema(itemMap)
				} else {
					sanitizedArr[i] = item
				}
			}
			result[key] = sanitizedArr
		} else {
			result[key] = value
		}
	}

	return result
}

// AdaptResponse 将 OpenAI 响应转换为 Claude 响应（不需要，因为 Claude 接口直接返回 OpenAI 响应）
func (a *ClaudeToOpenAIAdapter) AdaptResponse(respData map[string]interface{}) (map[string]interface{}, error) {
	// 这个适配器主要用于请求转换，响应不需要转换
	// 因为 /api/anthropic 接口返回的是 OpenAI 格式的响应
	return respData, nil
}

// AdaptStreamChunk 转换流式响应块 - Claude SSE → OpenAI SSE
// 支持：text_delta, thinking_delta, input_json_delta (tool_use)
func (a *ClaudeToOpenAIAdapter) AdaptStreamChunk(chunk map[string]interface{}) (map[string]interface{}, error) {
	chunkType, _ := chunk["type"].(string)

	switch chunkType {
	case "message_start":
		// 跳过 message_start 事件，OpenAI 不需要
		return nil, nil

	case "content_block_start":
		// 检查 content_block 类型
		if contentBlock, ok := chunk["content_block"].(map[string]interface{}); ok {
			blockType, _ := contentBlock["type"].(string)

			switch blockType {
			case "tool_use":
				// 保存 tool_use 信息到适配器状态，用于后续的 input_json_delta 事件
				// 注意：Claude 流式响应中，id 和 name 在 content_block_start 事件中提供
				// 但 input_json_delta 事件不包含这些信息，需要从适配器状态中获取
				if id, ok := contentBlock["id"].(string); ok {
					a.currentToolCallID = id
				}
				if name, ok := contentBlock["name"].(string); ok {
					a.currentToolCallName = name
				}
				// 对于 tool_use 类型，返回 nil 以等待 input_json_delta 事件
				return nil, nil
			case "thinking":
				// thinking 不需要特殊处理
			case "text":
				// text 不需要特殊处理
			}
		}
		return nil, nil

	case "content_block_delta":
		// 提取并转换不同类型的 delta
		if delta, ok := chunk["delta"].(map[string]interface{}); ok {
			deltaType, _ := delta["type"].(string)

			switch deltaType {
			case "text_delta":
				// 普通文本
				if text, ok := delta["text"].(string); ok {
					return map[string]interface{}{
						"id":      "chatcmpl-" + fmt.Sprintf("%d", time.Now().UnixNano()),
						"object":  "chat.completion.chunk",
						"created": time.Now().Unix(),
						"model":   "claude",
						"choices": []interface{}{
							map[string]interface{}{
								"index":         0,
								"delta":         map[string]interface{}{"content": text},
								"finish_reason": nil,
							},
						},
					}, nil
				}

			case "thinking_delta":
				// Thinking 推理内容 → 转换为 OpenAI 的 reasoning_content
				if thinking, ok := delta["thinking"].(string); ok {
					return map[string]interface{}{
						"id":      "chatcmpl-" + fmt.Sprintf("%d", time.Now().UnixNano()),
						"object":  "chat.completion.chunk",
						"created": time.Now().Unix(),
						"model":   "claude",
						"choices": []interface{}{
							map[string]interface{}{
								"index": 0,
								"delta": map[string]interface{}{
									"reasoning_content": thinking,
								},
								"finish_reason": nil,
							},
						},
					}, nil
				}

			case "input_json_delta":
				// Tool Use 参数 → 转换为 OpenAI 的 tool_calls
				// 注意：OpenAI 流式发送 tool_calls 是分片的
				// 这里需要调用辅助方法处理
				return a.adaptToolUseDelta(delta, chunk), nil
			}
		}
		return nil, nil

	case "content_block_stop":
		// 不需要发送 block stop 事件
		return nil, nil

	case "message_delta":
		// 提取 finish_reason 并发送最终的 chunk
		if delta, ok := chunk["delta"].(map[string]interface{}); ok {
			stopReason, _ := delta["stop_reason"].(string)

			// 转换 stop_reason: end_turn → stop, max_tokens → length
			openaiStopReason := "stop"
			if stopReason == "max_tokens" {
				openaiStopReason = "length"
			}
			// tool_use 也映射为 tool_calls
			if stopReason == "tool_use" {
				openaiStopReason = "tool_calls"
			}

			return map[string]interface{}{
				"id":      "chatcmpl-" + fmt.Sprintf("%d", time.Now().UnixNano()),
				"object":  "chat.completion.chunk",
				"created": time.Now().Unix(),
				"model":   "claude",
				"choices": []interface{}{
					map[string]interface{}{
						"index":         0,
						"delta":         map[string]interface{}{},
						"finish_reason": openaiStopReason,
					},
				},
			}, nil
		}
		return nil, nil

	case "message_stop":
		// 已经在 message_delta 中处理了 finish_reason，跳过
		return nil, nil

	default:
		// 未知类型，跳过
		return nil, nil
	}
}

// AdaptStreamStart 流式响应开始
func (a *ClaudeToOpenAIAdapter) AdaptStreamStart(model string) []map[string]interface{} {
	// 不需要额外的开始消息
	return nil
}

// AdaptStreamEnd 流式响应结束
func (a *ClaudeToOpenAIAdapter) AdaptStreamEnd() []map[string]interface{} {
	// 不需要额外的结束消息
	return nil
}

// adaptToolUseDelta 处理 Claude tool_use 的 input_json_delta 并转换为 OpenAI 格式
func (a *ClaudeToOpenAIAdapter) adaptToolUseDelta(delta map[string]interface{}, chunk map[string]interface{}) map[string]interface{} {
	// 从 content_block 中提取 tool_use 信息
	index, _ := chunk["index"].(float64)

	// 从适配器状态中获取当前 tool_use 的 ID 和 name
	toolID := a.currentToolCallID
	toolName := a.currentToolCallName

	// 提取 partial_json
	partialJSON, _ := delta["partial_json"].(string)

	// 构建 OpenAI 格式的 tool_calls delta
	toolCallDelta := map[string]interface{}{
		"index": int(index),
	}

	// 如果有 toolID，添加 id 字段
	if toolID != "" {
		toolCallDelta["id"] = toolID
	}

	toolCallDelta["type"] = "function"
	toolCallDelta["function"] = map[string]interface{}{
		"arguments": partialJSON,
	}

	// 如果有 toolName，添加 name 到 function 中
	if toolName != "" {
		toolCallDelta["function"].(map[string]interface{})["name"] = toolName
	}

	return map[string]interface{}{
		"id":      "chatcmpl-" + fmt.Sprintf("%d", time.Now().UnixNano()),
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   "claude",
		"choices": []interface{}{
			map[string]interface{}{
				"index":         int(index),
				"delta":         map[string]interface{}{"tool_calls": []interface{}{toolCallDelta}},
				"finish_reason": nil,
			},
		},
	}
}
