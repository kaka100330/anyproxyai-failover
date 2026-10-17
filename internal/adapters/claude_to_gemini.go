package adapters

import (
	"encoding/json"
	"fmt"
	"time"
)

// ClaudeToGeminiAdapter 将 Claude 格式转换为 Gemini 格式
type ClaudeToGeminiAdapter struct{}

func init() {
	RegisterAdapter("claude-to-gemini", &ClaudeToGeminiAdapter{})
}

// AdaptRequest 将 Claude 请求转换为 Gemini 请求
func (a *ClaudeToGeminiAdapter) AdaptRequest(reqData map[string]interface{}, model string) (map[string]interface{}, error) {
	geminiReq := make(map[string]interface{})

	// 转换 system 为 systemInstruction
	if system, ok := reqData["system"].(string); ok && system != "" {
		geminiReq["systemInstruction"] = map[string]interface{}{
			"parts": []interface{}{
				map[string]interface{}{"text": system},
			},
		}
	}

	// 转换 messages 为 contents
	contents := make([]interface{}, 0)
	if messages, ok := reqData["messages"].([]interface{}); ok {
		for _, msg := range messages {
			if msgMap, ok := msg.(map[string]interface{}); ok {
				role, _ := msgMap["role"].(string)
				content := msgMap["content"]

				// 转换角色: Claude 的 "assistant" -> Gemini 的 "model"
				geminiRole := role
				if role == "assistant" {
					geminiRole = "model"
				}

				// 处理内容
				parts := make([]interface{}, 0)

				// 检查内容类型
				switch c := content.(type) {
				case string:
					// 纯文本消息
					parts = append(parts, map[string]interface{}{"text": c})

				case []interface{}:
					// 结构化内容
					for _, block := range c {
						if blockMap, ok := block.(map[string]interface{}); ok {
							blockType, _ := blockMap["type"].(string)

							switch blockType {
							case "text":
								if text, ok := blockMap["text"].(string); ok {
									parts = append(parts, map[string]interface{}{"text": text})
								}

							case "tool_use":
								name, _ := blockMap["name"].(string)
								input := blockMap["input"]

								parts = append(parts, map[string]interface{}{
									"functionCall": map[string]interface{}{
										"name": name,
										"args": input,
									},
								})

							case "tool_result":
								toolUseID, _ := blockMap["tool_use_id"].(string)
								resultContent := blockMap["content"]

								var resultStr string
								if rs, ok := resultContent.(string); ok {
									resultStr = rs
								} else {
									resultJSON, _ := json.Marshal(resultContent)
									resultStr = string(resultJSON)
								}

								// 提取函数名(从 tool_use_id)
								functionName := extractFunctionNameFromID(toolUseID)

								parts = append(parts, map[string]interface{}{
									"functionResponse": map[string]interface{}{
										"name": functionName,
										"response": map[string]interface{}{
											"result": resultStr,
										},
									},
								})
							}
						}
					}
				}

				if len(parts) > 0 {
					contents = append(contents, map[string]interface{}{
						"role":  geminiRole,
						"parts": parts,
					})
				}
			}
		}
	}

	geminiReq["contents"] = contents

	// 转换生成配置
	generationConfig := make(map[string]interface{})

	if maxTokens, ok := reqData["max_tokens"]; ok {
		generationConfig["maxOutputTokens"] = maxTokens
	}

	if temperature, ok := reqData["temperature"]; ok {
		generationConfig["temperature"] = temperature
	}

	if topP, ok := reqData["top_p"]; ok {
		generationConfig["topP"] = topP
	}

	if stopSequences, ok := reqData["stop_sequences"]; ok {
		generationConfig["stopSequences"] = stopSequences
	}

	if len(generationConfig) > 0 {
		geminiReq["generationConfig"] = generationConfig
	}

	// 转换 tools
	if tools, ok := reqData["tools"].([]interface{}); ok && len(tools) > 0 {
		functionDeclarations := make([]interface{}, 0)
		for _, tool := range tools {
			if toolMap, ok := tool.(map[string]interface{}); ok {
				name, _ := toolMap["name"].(string)
				description, _ := toolMap["description"].(string)
				inputSchema := toolMap["input_schema"]

				// 清理 Gemini 不支持的 schema 字段
				cleanedSchema := cleanGeminiSchemaC2G(inputSchema)

				functionDeclarations = append(functionDeclarations, map[string]interface{}{
					"name":        name,
					"description": description,
					"parameters":  cleanedSchema,
				})
			}
		}
		geminiReq["tools"] = []interface{}{
			map[string]interface{}{
				"functionDeclarations": functionDeclarations,
			},
		}
	}

	return geminiReq, nil
}

// cleanGeminiSchemaC2G 清理 Gemini 不支持的 schema 字段
func cleanGeminiSchemaC2G(schema interface{}) interface{} {
	if schemaMap, ok := schema.(map[string]interface{}); ok {
		cleaned := make(map[string]interface{})
		for key, value := range schemaMap {
			// 移除不支持的字段
			if key == "additionalProperties" || key == "default" || key == "$schema" {
				continue
			}
			// 递归清理嵌套对象
			if valueMap, ok := value.(map[string]interface{}); ok {
				cleaned[key] = cleanGeminiSchemaC2G(valueMap)
			} else if valueArray, ok := value.([]interface{}); ok {
				cleanedArray := make([]interface{}, len(valueArray))
				for i, item := range valueArray {
					cleanedArray[i] = cleanGeminiSchemaC2G(item)
				}
				cleaned[key] = cleanedArray
			} else {
				cleaned[key] = value
			}
		}
		return cleaned
	}
	return schema
}

// extractFunctionNameFromID 从 tool_use_id 提取函数名
func extractFunctionNameFromID(toolID string) string {
	// tool_use_id 格式通常是 "toolu_xxx" 或包含函数名
	// 这里简单返回 ID,实际使用时可能需要映射
	return toolID
}

// AdaptResponse 将 Gemini 响应转换为 Claude 响应
func (a *ClaudeToGeminiAdapter) AdaptResponse(respData map[string]interface{}) (map[string]interface{}, error) {
	claudeResp := make(map[string]interface{})

	// 基本字段
	claudeResp["id"] = fmt.Sprintf("msg_gemini_%d", time.Now().UnixNano())
	claudeResp["type"] = "message"
	claudeResp["role"] = "assistant"
	claudeResp["model"] = "claude-3-sonnet-20240229"

	// 提取内容
	contentBlocks := make([]interface{}, 0)
	stopReason := "end_turn"

	if candidates, ok := respData["candidates"].([]interface{}); ok && len(candidates) > 0 {
		if candidate, ok := candidates[0].(map[string]interface{}); ok {
			// 提取内容
			if content, ok := candidate["content"].(map[string]interface{}); ok {
				if parts, ok := content["parts"].([]interface{}); ok {
					for _, part := range parts {
						if partMap, ok := part.(map[string]interface{}); ok {
							// 文本内容
							if text, ok := partMap["text"].(string); ok {
								contentBlocks = append(contentBlocks, map[string]interface{}{
									"type": "text",
									"text": text,
								})
							}

							// 函数调用
							if functionCall, ok := partMap["functionCall"].(map[string]interface{}); ok {
								name, _ := functionCall["name"].(string)
								args := functionCall["args"]

								contentBlocks = append(contentBlocks, map[string]interface{}{
									"type":  "tool_use",
									"id":    fmt.Sprintf("toolu_%s_%d", name, time.Now().UnixNano()),
									"name":  name,
									"input": args,
								})
							}
						}
					}
				}
			}

			// 转换 finishReason
			if finishReason, ok := candidate["finishReason"].(string); ok {
				switch finishReason {
				case "STOP":
					stopReason = "end_turn"
				case "MAX_TOKENS":
					stopReason = "max_tokens"
				case "SAFETY", "RECITATION":
					stopReason = "end_turn"
				}
			}
		}
	}

	// 如果没有内容块,添加空文本块
	if len(contentBlocks) == 0 {
		contentBlocks = append(contentBlocks, map[string]interface{}{
			"type": "text",
			"text": "",
		})
	}

	claudeResp["content"] = contentBlocks
	claudeResp["stop_reason"] = stopReason
	claudeResp["stop_sequence"] = nil

	// 转换 usage
	if usageMetadata, ok := respData["usageMetadata"].(map[string]interface{}); ok {
		inputTokens := 0
		outputTokens := 0

		if pt, ok := usageMetadata["promptTokenCount"].(float64); ok {
			inputTokens = int(pt)
		}
		if ct, ok := usageMetadata["candidatesTokenCount"].(float64); ok {
			outputTokens = int(ct)
		}

		claudeResp["usage"] = map[string]interface{}{
			"input_tokens":  inputTokens,
			"output_tokens": outputTokens,
		}
	}

	return claudeResp, nil
}

// AdaptStreamChunk 转换流式响应块
func (a *ClaudeToGeminiAdapter) AdaptStreamChunk(chunk map[string]interface{}) (map[string]interface{}, error) {
	// 提取内容
	var textContent string

	if candidates, ok := chunk["candidates"].([]interface{}); ok && len(candidates) > 0 {
		if candidate, ok := candidates[0].(map[string]interface{}); ok {
			if content, ok := candidate["content"].(map[string]interface{}); ok {
				if parts, ok := content["parts"].([]interface{}); ok {
					for _, part := range parts {
						if partMap, ok := part.(map[string]interface{}); ok {
							if text, ok := partMap["text"].(string); ok {
								textContent += text
							}
						}
					}
				}
			}

			// 检查是否结束
			if finishReason, ok := candidate["finishReason"].(string); ok && finishReason != "" {
				return map[string]interface{}{
					"type": "message_stop",
				}, nil
			}
		}
	}

	if textContent != "" {
		return map[string]interface{}{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]interface{}{
				"type": "text_delta",
				"text": textContent,
			},
		}, nil
	}

	return nil, nil
}

// AdaptStreamStart 流式响应开始
func (a *ClaudeToGeminiAdapter) AdaptStreamStart(model string) []map[string]interface{} {
	var events []map[string]interface{}

	// message_start 事件
	messageStart := map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            fmt.Sprintf("msg_gemini_%d", time.Now().UnixNano()),
			"type":          "message",
			"role":          "assistant",
			"content":       []interface{}{},
			"model":         model,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]interface{}{
				"input_tokens":  0,
				"output_tokens": 0,
			},
		},
	}
	events = append(events, messageStart)

	// content_block_start 事件
	contentBlockStart := map[string]interface{}{
		"type":  "content_block_start",
		"index": 0,
		"content_block": map[string]interface{}{
			"type": "text",
			"text": "",
		},
	}
	events = append(events, contentBlockStart)

	return events
}

// AdaptStreamEnd 流式响应结束
func (a *ClaudeToGeminiAdapter) AdaptStreamEnd() []map[string]interface{} {
	var events []map[string]interface{}

	// content_block_stop 事件
	contentBlockStop := map[string]interface{}{
		"type":  "content_block_stop",
		"index": 0,
	}
	events = append(events, contentBlockStop)

	// message_delta 事件
	messageDelta := map[string]interface{}{
		"type": "message_delta",
		"delta": map[string]interface{}{
			"stop_reason":   "end_turn",
			"stop_sequence": nil,
		},
		"usage": map[string]interface{}{
			"output_tokens": 0,
		},
	}
	events = append(events, messageDelta)

	return events
}
