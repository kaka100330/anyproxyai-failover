package adapters

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ClaudeCodeToOpenAIAdapter 将 Claude Code 格式转换为 OpenAI 格式
// 专门处理 Claude Code 的特殊格式，包括：
// - system 参数（字符串或数组）转换为 OpenAI 的 system 消息
// - tools 工具链转换
// - tool_result 工具结果转换
// - 多模态内容处理
type ClaudeCodeToOpenAIAdapter struct{}

func init() {
	RegisterAdapter("claudecode-to-openai", &ClaudeCodeToOpenAIAdapter{})
}

// AdaptRequest 将 Claude Code 请求转换为 OpenAI 请求
func (a *ClaudeCodeToOpenAIAdapter) AdaptRequest(reqData map[string]interface{}, model string) (map[string]interface{}, error) {
	openaiReq := make(map[string]interface{})

	// 设置模型
	openaiReq["model"] = model

	// 转换消息
	openaiMessages := make([]interface{}, 0)

	// 1. 处理 system 参数 - Claude 支持单独的 system 字段
	if system := reqData["system"]; system != nil {
		systemContent := extractSystemContent(system)
		if systemContent != "" {
			openaiMessages = append(openaiMessages, map[string]interface{}{
				"role":    "system",
				"content": systemContent,
			})
		}
	}

	// 2. 转换 messages
	if messages, ok := reqData["messages"].([]interface{}); ok {
		for _, msg := range messages {
			if msgMap, ok := msg.(map[string]interface{}); ok {
				convertedMsgs := a.convertMessage(msgMap)
				openaiMessages = append(openaiMessages, convertedMsgs...)
			}
		}
	}

	openaiReq["messages"] = openaiMessages

	// 3. 转换 tools 工具链
	if tools, ok := reqData["tools"].([]interface{}); ok && len(tools) > 0 {
		openaiTools := a.convertTools(tools)
		if len(openaiTools) > 0 {
			openaiReq["tools"] = openaiTools
		}
	}

	// 4. 转换 tool_choice
	if toolChoice := reqData["tool_choice"]; toolChoice != nil {
		openaiReq["tool_choice"] = a.convertToolChoice(toolChoice)
	}

	// 5. 转换其他参数
	if maxTokens, ok := reqData["max_tokens"]; ok {
		openaiReq["max_tokens"] = maxTokens
	}

	if temperature, ok := reqData["temperature"]; ok {
		openaiReq["temperature"] = temperature
	}

	if topP, ok := reqData["top_p"]; ok {
		openaiReq["top_p"] = topP
	}

	if stream, ok := reqData["stream"]; ok {
		openaiReq["stream"] = stream
		// 启用 usage 统计
		if streamBool, ok := stream.(bool); ok && streamBool {
			openaiReq["stream_options"] = map[string]interface{}{
				"include_usage": true,
			}
		}
	}

	if stopSequences, ok := reqData["stop_sequences"]; ok {
		openaiReq["stop"] = stopSequences
	}

	return openaiReq, nil
}

// extractSystemContent 从 system 参数提取内容
func extractSystemContent(system interface{}) string {
	switch sys := system.(type) {
	case string:
		return sys
	case []interface{}:
		// Claude Code 的 system 可能是数组格式
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

// convertMessage 转换单条消息
func (a *ClaudeCodeToOpenAIAdapter) convertMessage(msgMap map[string]interface{}) []interface{} {
	result := make([]interface{}, 0)
	role, _ := msgMap["role"].(string)

	// 处理 content
	contentVal := msgMap["content"]

	switch c := contentVal.(type) {
	case string:
		// 简单文本消息
		result = append(result, map[string]interface{}{
			"role":    role,
			"content": c,
		})

	case []interface{}:
		// 复杂内容数组 - 可能包含 text, tool_use, tool_result 等
		if role == "user" {
			// 检查是否包含 tool_result
			hasToolResult := false
			for _, block := range c {
				if blockMap, ok := block.(map[string]interface{}); ok {
					if blockType, _ := blockMap["type"].(string); blockType == "tool_result" {
						hasToolResult = true
						break
					}
				}
			}

			if hasToolResult {
				// 处理包含 tool_result 的用户消息
				result = append(result, a.convertToolResultMessage(c)...)
			} else {
				// 普通用户消息，提取文本
				textContent := a.extractTextFromBlocks(c)
				if textContent != "" {
					result = append(result, map[string]interface{}{
						"role":    role,
						"content": textContent,
					})
				}
			}
		} else if role == "assistant" {
			// 助手消息 - 可能包含 text 和 tool_use
			textContent := ""
			var toolCalls []interface{}

			for _, block := range c {
				if blockMap, ok := block.(map[string]interface{}); ok {
					blockType, _ := blockMap["type"].(string)

					switch blockType {
					case "text":
						if text, ok := blockMap["text"].(string); ok {
							textContent += text
						}
					case "tool_use":
						// 转换 tool_use 为 OpenAI 的 tool_calls
						toolCall := a.convertToolUse(blockMap)
						if toolCall != nil {
							toolCalls = append(toolCalls, toolCall)
						}
					}
				}
			}

			assistantMsg := map[string]interface{}{
				"role": "assistant",
			}
			if textContent != "" {
				assistantMsg["content"] = textContent
			}
			if len(toolCalls) > 0 {
				assistantMsg["tool_calls"] = toolCalls
			}
			result = append(result, assistantMsg)
		}

	default:
		// 其他类型，尝试转为字符串
		result = append(result, map[string]interface{}{
			"role":    role,
			"content": fmt.Sprintf("%v", contentVal),
		})
	}

	return result
}

// extractTextFromBlocks 从内容块数组中提取文本
func (a *ClaudeCodeToOpenAIAdapter) extractTextFromBlocks(blocks []interface{}) string {
	var textParts []string
	for _, block := range blocks {
		if blockMap, ok := block.(map[string]interface{}); ok {
			if blockType, _ := blockMap["type"].(string); blockType == "text" {
				if text, ok := blockMap["text"].(string); ok {
					textParts = append(textParts, text)
				}
			}
		}
	}
	return strings.Join(textParts, "\n")
}

// convertToolUse 转换 tool_use 为 OpenAI 的 tool_call
func (a *ClaudeCodeToOpenAIAdapter) convertToolUse(toolUse map[string]interface{}) map[string]interface{} {
	id, _ := toolUse["id"].(string)
	name, _ := toolUse["name"].(string)
	input := toolUse["input"]

	// 将 input 转为 JSON 字符串
	var arguments string
	if input != nil {
		if inputBytes, err := json.Marshal(input); err == nil {
			arguments = string(inputBytes)
		}
	}

	return map[string]interface{}{
		"id":   id,
		"type": "function",
		"function": map[string]interface{}{
			"name":      name,
			"arguments": arguments,
		},
	}
}

// convertToolResultMessage 转换包含 tool_result 的消息
func (a *ClaudeCodeToOpenAIAdapter) convertToolResultMessage(blocks []interface{}) []interface{} {
	result := make([]interface{}, 0)

	for _, block := range blocks {
		if blockMap, ok := block.(map[string]interface{}); ok {
			blockType, _ := blockMap["type"].(string)

			switch blockType {
			case "tool_result":
				// 转换为 OpenAI 的 tool 角色消息
				toolUseID, _ := blockMap["tool_use_id"].(string)
				content := extractToolResultContent(blockMap["content"])

				result = append(result, map[string]interface{}{
					"role":         "tool",
					"tool_call_id": toolUseID,
					"content":      content,
				})

			case "text":
				// 普通文本，作为用户消息
				if text, ok := blockMap["text"].(string); ok && text != "" {
					result = append(result, map[string]interface{}{
						"role":    "user",
						"content": text,
					})
				}
			}
		}
	}

	return result
}

// extractToolResultContent 提取 tool_result 的内容
func extractToolResultContent(content interface{}) string {
	if content == nil {
		return "No content provided"
	}

	switch c := content.(type) {
	case string:
		return c
	case []interface{}:
		var parts []string
		for _, item := range c {
			if itemMap, ok := item.(map[string]interface{}); ok {
				if itemType, _ := itemMap["type"].(string); itemType == "text" {
					if text, ok := itemMap["text"].(string); ok {
						parts = append(parts, text)
					}
				} else {
					// 其他类型，序列化为 JSON
					if jsonBytes, err := json.Marshal(itemMap); err == nil {
						parts = append(parts, string(jsonBytes))
					}
				}
			} else if str, ok := item.(string); ok {
				parts = append(parts, str)
			}
		}
		return strings.Join(parts, "\n")
	case map[string]interface{}:
		if contentType, _ := c["type"].(string); contentType == "text" {
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

// convertTools 转换工具定义
func (a *ClaudeCodeToOpenAIAdapter) convertTools(tools []interface{}) []interface{} {
	openaiTools := make([]interface{}, 0, len(tools))

	for _, tool := range tools {
		if toolMap, ok := tool.(map[string]interface{}); ok {
			name, _ := toolMap["name"].(string)
			description, _ := toolMap["description"].(string)
			inputSchema := toolMap["input_schema"]

			openaiTool := map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        name,
					"description": description,
					"parameters":  inputSchema,
				},
			}
			openaiTools = append(openaiTools, openaiTool)
		}
	}

	return openaiTools
}

// convertToolChoice 转换 tool_choice
func (a *ClaudeCodeToOpenAIAdapter) convertToolChoice(toolChoice interface{}) interface{} {
	switch tc := toolChoice.(type) {
	case map[string]interface{}:
		choiceType, _ := tc["type"].(string)
		switch choiceType {
		case "auto":
			return "auto"
		case "any":
			return "required"
		case "tool":
			if name, ok := tc["name"].(string); ok {
				return map[string]interface{}{
					"type": "function",
					"function": map[string]string{
						"name": name,
					},
				}
			}
		}
	case string:
		return tc
	}
	return "auto"
}

// AdaptResponse 将 OpenAI 响应转换为 Claude 响应
func (a *ClaudeCodeToOpenAIAdapter) AdaptResponse(respData map[string]interface{}) (map[string]interface{}, error) {
	claudeResp := make(map[string]interface{})

	// 基本字段
	if id, ok := respData["id"].(string); ok {
		claudeResp["id"] = id
	} else {
		claudeResp["id"] = "msg_default"
	}
	claudeResp["type"] = "message"
	claudeResp["role"] = "assistant"

	if model, ok := respData["model"].(string); ok {
		claudeResp["model"] = model
	}

	// 转换 content
	content := make([]map[string]interface{}, 0)

	if choices, ok := respData["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			if message, ok := choice["message"].(map[string]interface{}); ok {
				// 文本内容
				if msgContent, ok := message["content"].(string); ok && msgContent != "" {
					content = append(content, map[string]interface{}{
						"type": "text",
						"text": msgContent,
					})
				}

				// 工具调用
				if toolCalls, ok := message["tool_calls"].([]interface{}); ok {
					for _, tc := range toolCalls {
						if tcMap, ok := tc.(map[string]interface{}); ok {
							toolUse := a.convertOpenAIToolCallToClaude(tcMap)
							if toolUse != nil {
								content = append(content, toolUse)
							}
						}
					}
				}
			}

			// 转换 finish_reason
			if finishReason, ok := choice["finish_reason"].(string); ok {
				switch finishReason {
				case "stop":
					claudeResp["stop_reason"] = "end_turn"
				case "length":
					claudeResp["stop_reason"] = "max_tokens"
				case "tool_calls":
					claudeResp["stop_reason"] = "tool_use"
				default:
					claudeResp["stop_reason"] = finishReason
				}
			}
		}
	}

	if len(content) == 0 {
		content = append(content, map[string]interface{}{
			"type": "text",
			"text": "",
		})
	}
	claudeResp["content"] = content
	claudeResp["stop_sequence"] = nil

	// 转换 usage
	if usage, ok := respData["usage"].(map[string]interface{}); ok {
		claudeResp["usage"] = map[string]interface{}{
			"input_tokens":  getIntValue(usage, "prompt_tokens", 0),
			"output_tokens": getIntValue(usage, "completion_tokens", 0),
		}
	}

	return claudeResp, nil
}

// convertOpenAIToolCallToClaude 转换 OpenAI tool_call 为 Claude tool_use
func (a *ClaudeCodeToOpenAIAdapter) convertOpenAIToolCallToClaude(toolCall map[string]interface{}) map[string]interface{} {
	id, _ := toolCall["id"].(string)

	if function, ok := toolCall["function"].(map[string]interface{}); ok {
		name, _ := function["name"].(string)
		arguments, _ := function["arguments"].(string)

		var input map[string]interface{}
		if err := json.Unmarshal([]byte(arguments), &input); err != nil {
			input = map[string]interface{}{"raw": arguments}
		}

		return map[string]interface{}{
			"type":  "tool_use",
			"id":    id,
			"name":  name,
			"input": input,
		}
	}

	return nil
}

// getIntValue 从 map 中获取整数值
func getIntValue(m map[string]interface{}, key string, defaultValue int) int {
	if val, ok := m[key]; ok {
		if f, ok := val.(float64); ok {
			return int(f)
		}
		if i, ok := val.(int); ok {
			return i
		}
	}
	return defaultValue
}

// AdaptStreamChunk 转换流式响应块
func (a *ClaudeCodeToOpenAIAdapter) AdaptStreamChunk(chunk map[string]interface{}) (map[string]interface{}, error) {
	// 这个方法在 streamOpenAIToClaude 中处理，这里返回 nil
	return nil, nil
}

// AdaptStreamStart 流式响应开始
func (a *ClaudeCodeToOpenAIAdapter) AdaptStreamStart(model string) []map[string]interface{} {
	return nil
}

// AdaptStreamEnd 流式响应结束
func (a *ClaudeCodeToOpenAIAdapter) AdaptStreamEnd() []map[string]interface{} {
	return nil
}
