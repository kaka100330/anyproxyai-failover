package adapters

import (
	"encoding/json"
	"fmt"
	"time"
)

// OpenAIToGeminiAdapter 将 OpenAI 格式转换为 Gemini 格式
type OpenAIToGeminiAdapter struct{}

func init() {
	RegisterAdapter("openai-to-gemini", &OpenAIToGeminiAdapter{})
}

// AdaptRequest 将 OpenAI 请求转换为 Gemini 请求
func (a *OpenAIToGeminiAdapter) AdaptRequest(reqData map[string]interface{}, model string) (map[string]interface{}, error) {
	geminiReq := make(map[string]interface{})

	// 转换消息为 Gemini contents
	contents := make([]interface{}, 0)
	var systemInstruction interface{}

	if messages, ok := reqData["messages"].([]interface{}); ok {
		for _, msg := range messages {
			if msgMap, ok := msg.(map[string]interface{}); ok {
				role, _ := msgMap["role"].(string)
				content := msgMap["content"]

				// 处理 system 消息 - Gemini 使用 systemInstruction
				if role == "system" {
					if contentStr, ok := content.(string); ok {
						systemInstruction = map[string]interface{}{
							"parts": []interface{}{
								map[string]interface{}{"text": contentStr},
							},
						}
					}
					continue
				}

				// 转换角色
				geminiRole := role
				if role == "assistant" {
					geminiRole = "model"
				}

				// 处理 tool 消息 - 转换为 Gemini 的 functionResponse
				if role == "tool" {
					toolCallID, _ := msgMap["tool_call_id"].(string)
					contentStr := ""
					if cs, ok := content.(string); ok {
						contentStr = cs
					}

					// 从 tool_call_id 提取函数名
					functionName := extractFunctionName(toolCallID)

					contents = append(contents, map[string]interface{}{
						"role": "user",
						"parts": []interface{}{
							map[string]interface{}{
								"functionResponse": map[string]interface{}{
									"name": functionName,
									"response": map[string]interface{}{
										"result": contentStr,
									},
								},
							},
						},
					})
					continue
				}

				// 处理 assistant 消息
				if role == "assistant" {
					parts := make([]interface{}, 0)

					// 检查是否有 tool_calls
					if toolCalls, ok := msgMap["tool_calls"].([]interface{}); ok && len(toolCalls) > 0 {
						// 添加文本内容
						if contentStr, ok := content.(string); ok && contentStr != "" {
							parts = append(parts, map[string]interface{}{"text": contentStr})
						}

						// 转换 tool_calls 为 functionCall
						for _, tc := range toolCalls {
							if tcMap, ok := tc.(map[string]interface{}); ok {
								if function, ok := tcMap["function"].(map[string]interface{}); ok {
									name, _ := function["name"].(string)
									arguments, _ := function["arguments"].(string)

									var args map[string]interface{}
									if err := json.Unmarshal([]byte(arguments), &args); err != nil {
										args = map[string]interface{}{}
									}

									parts = append(parts, map[string]interface{}{
										"functionCall": map[string]interface{}{
											"name": name,
											"args": args,
										},
									})
								}
							}
						}
					} else {
						// 普通文本消息
						parts = a.convertContentToParts(content)
					}

					contents = append(contents, map[string]interface{}{
						"role":  geminiRole,
						"parts": parts,
					})
					continue
				}

				// 处理 user 消息
				if role == "user" {
					parts := a.convertContentToParts(content)
					contents = append(contents, map[string]interface{}{
						"role":  geminiRole,
						"parts": parts,
					})
				}
			}
		}
	}

	geminiReq["contents"] = contents

	// 设置 systemInstruction
	if systemInstruction != nil {
		geminiReq["systemInstruction"] = systemInstruction
	}

	// 转换 tools
	if tools, ok := reqData["tools"].([]interface{}); ok && len(tools) > 0 {
		functionDeclarations := make([]interface{}, 0, len(tools))
		for _, tool := range tools {
			if toolMap, ok := tool.(map[string]interface{}); ok {
				if function, ok := toolMap["function"].(map[string]interface{}); ok {
					name, _ := function["name"].(string)
					description, _ := function["description"].(string)
					parameters := function["parameters"]

					// 清理 Gemini 不支持的 schema 字段
					cleanedParams := cleanGeminiSchema(parameters)

					functionDeclarations = append(functionDeclarations, map[string]interface{}{
						"name":        name,
						"description": description,
						"parameters":  cleanedParams,
					})
				}
			}
		}
		geminiReq["tools"] = []interface{}{
			map[string]interface{}{
				"functionDeclarations": functionDeclarations,
			},
		}
	}

	// 转换生成配置
	generationConfig := make(map[string]interface{})

	if maxTokens, ok := reqData["max_tokens"]; ok {
		generationConfig["maxOutputTokens"] = maxTokens
	} else if maxCompletionTokens, ok := reqData["max_completion_tokens"]; ok {
		generationConfig["maxOutputTokens"] = maxCompletionTokens
	}

	if temperature, ok := reqData["temperature"]; ok {
		generationConfig["temperature"] = temperature
	}

	if topP, ok := reqData["top_p"]; ok {
		generationConfig["topP"] = topP
	}

	if stop, ok := reqData["stop"]; ok {
		generationConfig["stopSequences"] = stop
	}

	if len(generationConfig) > 0 {
		geminiReq["generationConfig"] = generationConfig
	}

	return geminiReq, nil
}

// convertContentToParts 将内容转换为 Gemini parts
func (a *OpenAIToGeminiAdapter) convertContentToParts(content interface{}) []interface{} {
	parts := make([]interface{}, 0)

	switch c := content.(type) {
	case string:
		parts = append(parts, map[string]interface{}{"text": c})
	case []interface{}:
		for _, item := range c {
			if itemMap, ok := item.(map[string]interface{}); ok {
				itemType, _ := itemMap["type"].(string)

				switch itemType {
				case "text":
					if text, ok := itemMap["text"].(string); ok {
						parts = append(parts, map[string]interface{}{"text": text})
					}
				case "image_url":
					// 处理图片
					if imageURL, ok := itemMap["image_url"].(map[string]interface{}); ok {
						if url, ok := imageURL["url"].(string); ok {
							// 如果是 base64 data URL
							if len(url) > 5 && url[:5] == "data:" {
								// 解析 data URL
								parts = append(parts, map[string]interface{}{
									"text": fmt.Sprintf("[Image: %s...]", url[:50]),
								})
							} else {
								parts = append(parts, map[string]interface{}{
									"text": fmt.Sprintf("[Image URL: %s]", url),
								})
							}
						}
					}
				}
			}
		}
	default:
		parts = append(parts, map[string]interface{}{"text": fmt.Sprintf("%v", content)})
	}

	return parts
}

// cleanGeminiSchema 清理 Gemini 不支持的 schema 字段
func cleanGeminiSchema(schema interface{}) interface{} {
	if schemaMap, ok := schema.(map[string]interface{}); ok {
		cleaned := make(map[string]interface{})
		for key, value := range schemaMap {
			// 移除不支持的字段
			if key == "additionalProperties" || key == "default" || key == "$schema" {
				continue
			}
			// 递归清理嵌套对象
			if valueMap, ok := value.(map[string]interface{}); ok {
				cleaned[key] = cleanGeminiSchema(valueMap)
			} else if valueArray, ok := value.([]interface{}); ok {
				cleanedArray := make([]interface{}, len(valueArray))
				for i, item := range valueArray {
					cleanedArray[i] = cleanGeminiSchema(item)
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

// extractFunctionName 从 tool_call_id 提取函数名
func extractFunctionName(toolID string) string {
	// 如果 ID 格式是 call_xxx_functionName，提取函数名
	// 否则返回 ID 本身
	return toolID
}

// AdaptResponse 将 Gemini 响应转换为 OpenAI 响应
func (a *OpenAIToGeminiAdapter) AdaptResponse(respData map[string]interface{}) (map[string]interface{}, error) {
	openaiResp := make(map[string]interface{})

	// 基本字段
	openaiResp["id"] = fmt.Sprintf("chatcmpl-gemini-%d", time.Now().UnixNano())
	openaiResp["object"] = "chat.completion"
	openaiResp["created"] = time.Now().Unix()
	openaiResp["model"] = "gemini-pro"

	// 转换 candidates
	var textContent string
	var toolCalls []interface{}
	finishReason := "stop"

	if candidates, ok := respData["candidates"].([]interface{}); ok && len(candidates) > 0 {
		if candidate, ok := candidates[0].(map[string]interface{}); ok {
			// 提取内容
			if content, ok := candidate["content"].(map[string]interface{}); ok {
				if parts, ok := content["parts"].([]interface{}); ok {
					for _, part := range parts {
						if partMap, ok := part.(map[string]interface{}); ok {
							// 文本内容
							if text, ok := partMap["text"].(string); ok {
								textContent += text
							}
							// 函数调用
							if functionCall, ok := partMap["functionCall"].(map[string]interface{}); ok {
								name, _ := functionCall["name"].(string)
								args := functionCall["args"]

								var arguments string
								if argsBytes, err := json.Marshal(args); err == nil {
									arguments = string(argsBytes)
								}

								toolCalls = append(toolCalls, map[string]interface{}{
									"id":   fmt.Sprintf("call_%d_%s", time.Now().UnixNano(), name),
									"type": "function",
									"function": map[string]interface{}{
										"name":      name,
										"arguments": arguments,
									},
								})
							}
						}
					}
				}
			}

			// 转换 finishReason
			if fr, ok := candidate["finishReason"].(string); ok {
				switch fr {
				case "STOP":
					finishReason = "stop"
				case "MAX_TOKENS":
					finishReason = "length"
				case "SAFETY", "RECITATION":
					finishReason = "content_filter"
				default:
					finishReason = "stop"
				}
			}
		}
	}

	// 构建 message
	message := map[string]interface{}{
		"role":    "assistant",
		"content": textContent,
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
		finishReason = "tool_calls"
	}

	openaiResp["choices"] = []interface{}{
		map[string]interface{}{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		},
	}

	// 转换 usage
	if usageMetadata, ok := respData["usageMetadata"].(map[string]interface{}); ok {
		promptTokens := 0
		completionTokens := 0
		if pt, ok := usageMetadata["promptTokenCount"].(float64); ok {
			promptTokens = int(pt)
		}
		if ct, ok := usageMetadata["candidatesTokenCount"].(float64); ok {
			completionTokens = int(ct)
		}
		openaiResp["usage"] = map[string]interface{}{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
		}
	}

	return openaiResp, nil
}

// AdaptStreamChunk 转换流式响应块
func (a *OpenAIToGeminiAdapter) AdaptStreamChunk(chunk map[string]interface{}) (map[string]interface{}, error) {
	return nil, nil
}

// AdaptStreamStart 流式响应开始
func (a *OpenAIToGeminiAdapter) AdaptStreamStart(model string) []map[string]interface{} {
	return nil
}

// AdaptStreamEnd 流式响应结束
func (a *OpenAIToGeminiAdapter) AdaptStreamEnd() []map[string]interface{} {
	return nil
}
