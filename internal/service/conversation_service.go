package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"openai-router-go/internal/config"
)

// ConversationService handles conversation aggregation for different AI providers
type ConversationService struct {
	routeService  *RouteService
	proxyService  *ProxyService
	config        *config.Config
}

// NewConversationService creates a new conversation service
func NewConversationService(routeService *RouteService, proxyService *ProxyService, config *config.Config) *ConversationService {
	return &ConversationService{
		routeService: routeService,
		proxyService: proxyService,
		config:       config,
	}
}

// ConversationRequest represents a unified conversation request
type ConversationRequest struct {
	Provider    string                   `json:"provider"`    // "openai", "claude", or "gemini"
	Model       string                   `json:"model"`
	Messages    []map[string]interface{} `json:"messages"`
	Stream      bool                     `json:"stream,omitempty"`
	MaxTokens   int                      `json:"max_tokens,omitempty"`
	Temperature float64                  `json:"temperature,omitempty"`
}

// ConversationResponse represents a unified conversation response
type ConversationResponse struct {
	Provider  string      `json:"provider"`
	Model     string      `json:"model"`
	Content   string      `json:"content"`
	TokensUsed int         `json:"tokens_used,omitempty"`
	Error     string      `json:"error,omitempty"`
	RawResponse interface{} `json:"raw_response,omitempty"`
}

// SendConversation sends a conversation request to the specified provider
func (cs *ConversationService) SendConversation(req ConversationRequest) (*ConversationResponse, error) {
	switch strings.ToLower(req.Provider) {
	case "openai":
		return cs.sendOpenAIConversation(req)
	case "claude":
		return cs.sendClaudeConversation(req)
	case "gemini":
		return cs.sendGeminiConversation(req)
	default:
		return nil, fmt.Errorf("unsupported provider: %s", req.Provider)
	}
}

// sendOpenAIConversation sends a conversation using OpenAI format
func (cs *ConversationService) sendOpenAIConversation(req ConversationRequest) (*ConversationResponse, error) {
	// Construct OpenAI request
	openaiReq := map[string]interface{}{
		"model":    req.Model,
		"messages": req.Messages,
		"stream":   req.Stream,
	}

	if req.MaxTokens > 0 {
		openaiReq["max_tokens"] = req.MaxTokens
	}
	if req.Temperature > 0 {
		openaiReq["temperature"] = req.Temperature
	}

	// Convert to JSON
	reqBody, err := json.Marshal(openaiReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal OpenAI request: %v", err)
	}

	// Send request through proxy service
	headers := map[string]string{
		"Content-Type":  "application/json",
		"Authorization": fmt.Sprintf("Bearer %s", cs.config.LocalAPIKey),
	}

	respBody, statusCode, err := cs.proxyService.ProxyRequest(reqBody, headers)
	if err != nil {
		return &ConversationResponse{
			Provider: "openai",
			Model:    req.Model,
			Error:    err.Error(),
		}, err
	}

	if statusCode != http.StatusOK {
		return &ConversationResponse{
			Provider: "openai",
			Model:    req.Model,
			Error:    fmt.Sprintf("HTTP %d: %s", statusCode, string(respBody)),
		}, fmt.Errorf("OpenAI API returned status %d", statusCode)
	}

	// Parse OpenAI response
	var openaiResp map[string]interface{}
	if err := json.Unmarshal(respBody, &openaiResp); err != nil {
		return &ConversationResponse{
			Provider:  "openai",
			Model:     req.Model,
			Content:   string(respBody),
			Error:     "Failed to parse response",
			RawResponse: openaiResp,
		}, nil
	}

	// Extract content from OpenAI response
	content := ""
	tokensUsed := 0

	if choices, ok := openaiResp["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			if message, ok := choice["message"].(map[string]interface{}); ok {
				if contentStr, ok := message["content"].(string); ok {
					content = contentStr
				}
			}
		}
	}

	if usage, ok := openaiResp["usage"].(map[string]interface{}); ok {
		if totalTokens, ok := usage["total_tokens"].(float64); ok {
			tokensUsed = int(totalTokens)
		}
	}

	return &ConversationResponse{
		Provider:   "openai",
		Model:      req.Model,
		Content:    content,
		TokensUsed: tokensUsed,
		RawResponse: openaiResp,
	}, nil
}

// sendClaudeConversation sends a conversation using Claude format
func (cs *ConversationService) sendClaudeConversation(req ConversationRequest) (*ConversationResponse, error) {
	// Convert OpenAI message format to Claude format
	claudeMessages := make([]map[string]interface{}, 0)
	for _, msg := range req.Messages {
		role, ok := msg["role"].(string)
		if !ok {
			continue
		}

		content, ok := msg["content"].(string)
		if !ok {
			continue
		}

		// Convert roles
		claudeRole := "user"
		if role == "assistant" {
			claudeRole = "assistant"
		} else if role == "system" {
			claudeRole = "user" // Claude expects system message as first user message
		}

		claudeMessages = append(claudeMessages, map[string]interface{}{
			"role":    claudeRole,
			"content": content,
		})
	}

	// Construct Claude request
	claudeReq := map[string]interface{}{
		"model":    req.Model,
		"messages": claudeMessages,
		"max_tokens": req.MaxTokens,
	}

	if req.Temperature > 0 {
		claudeReq["temperature"] = req.Temperature
	}

	// Convert to JSON
	reqBody, err := json.Marshal(claudeReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal Claude request: %v", err)
	}

	// Send request through anthropic adapter
	headers := map[string]string{
		"Content-Type":     "application/json",
		"anthropic-version": "2023-06-01",
		"x-api-key":         cs.config.LocalAPIKey,
	}

	respBody, statusCode, err := cs.proxyService.ProxyAnthropicRequest(reqBody, headers)
	if err != nil {
		return &ConversationResponse{
			Provider: "claude",
			Model:    req.Model,
			Error:    err.Error(),
		}, err
	}

	if statusCode != http.StatusOK {
		return &ConversationResponse{
			Provider: "claude",
			Model:    req.Model,
			Error:    fmt.Sprintf("HTTP %d: %s", statusCode, string(respBody)),
		}, fmt.Errorf("Claude API returned status %d", statusCode)
	}

	// Parse Claude response
	var claudeResp map[string]interface{}
	if err := json.Unmarshal(respBody, &claudeResp); err != nil {
		return &ConversationResponse{
			Provider:  "claude",
			Model:     req.Model,
			Content:   string(respBody),
			Error:     "Failed to parse response",
			RawResponse: claudeResp,
		}, nil
	}

	// Extract content from Claude response
	content := ""
	tokensUsed := 0

	if contentBlock, ok := claudeResp["content"].([]interface{}); ok && len(contentBlock) > 0 {
		if block, ok := contentBlock[0].(map[string]interface{}); ok {
			if text, ok := block["text"].(string); ok {
				content = text
			}
		}
	}

	if usage, ok := claudeResp["usage"].(map[string]interface{}); ok {
		if totalTokens, ok := usage["input_tokens"].(float64); ok {
			tokensUsed += int(totalTokens)
		}
		if outputTokens, ok := usage["output_tokens"].(float64); ok {
			tokensUsed += int(outputTokens)
		}
	}

	return &ConversationResponse{
		Provider:   "claude",
		Model:      req.Model,
		Content:    content,
		TokensUsed: tokensUsed,
		RawResponse: claudeResp,
	}, nil
}

// sendGeminiConversation sends a conversation using Gemini format
func (cs *ConversationService) sendGeminiConversation(req ConversationRequest) (*ConversationResponse, error) {
	// Convert OpenAI message format to Gemini format
	contents := make([]map[string]interface{}, 0)
	for _, msg := range req.Messages {
		role, ok := msg["role"].(string)
		if !ok {
			continue
		}

		content, ok := msg["content"].(string)
		if !ok {
			continue
		}

		// Convert roles
		geminiRole := "user"
		if role == "assistant" {
			geminiRole = "model"
		}

		contents = append(contents, map[string]interface{}{
			"role":    geminiRole,
			"parts": []map[string]interface{}{
				{"text": content},
			},
		})
	}

	// Construct Gemini request
	geminiReq := map[string]interface{}{
		"contents": contents,
	}

	if req.MaxTokens > 0 {
		geminiReq["maxOutputTokens"] = req.MaxTokens
	}
	if req.Temperature > 0 {
		geminiReq["temperature"] = req.Temperature
	}

	// Convert to JSON
	reqBody, err := json.Marshal(geminiReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal Gemini request: %v", err)
	}

	// Send request through proxy service
	headers := map[string]string{
		"Content-Type": "application/json",
	}

	respBody, statusCode, err := cs.proxyService.ProxyRequest(reqBody, headers)
	if err != nil {
		return &ConversationResponse{
			Provider: "gemini",
			Model:    req.Model,
			Error:    err.Error(),
		}, err
	}

	if statusCode != http.StatusOK {
		return &ConversationResponse{
			Provider: "gemini",
			Model:    req.Model,
			Error:    fmt.Sprintf("HTTP %d: %s", statusCode, string(respBody)),
		}, fmt.Errorf("Gemini API returned status %d", statusCode)
	}

	// Parse Gemini response
	var geminiResp map[string]interface{}
	if err := json.Unmarshal(respBody, &geminiResp); err != nil {
		return &ConversationResponse{
			Provider:  "gemini",
			Model:     req.Model,
			Content:   string(respBody),
			Error:     "Failed to parse response",
			RawResponse: geminiResp,
		}, nil
	}

	// Extract content from Gemini response
	content := ""
	tokensUsed := 0

	if candidates, ok := geminiResp["candidates"].([]interface{}); ok && len(candidates) > 0 {
		if candidate, ok := candidates[0].(map[string]interface{}); ok {
			if contentParts, ok := candidate["content"].(map[string]interface{}); ok {
				if parts, ok := contentParts["parts"].([]interface{}); ok && len(parts) > 0 {
					if part, ok := parts[0].(map[string]interface{}); ok {
						if text, ok := part["text"].(string); ok {
							content = text
						}
					}
				}
			}
		}
	}

	if usageMetadata, ok := geminiResp["usageMetadata"].(map[string]interface{}); ok {
		if totalTokens, ok := usageMetadata["totalTokenCount"].(float64); ok {
			tokensUsed = int(totalTokens)
		}
	}

	return &ConversationResponse{
		Provider:   "gemini",
		Model:      req.Model,
		Content:    content,
		TokensUsed: tokensUsed,
		RawResponse: geminiResp,
	}, nil
}

// GetSDKExamples returns SDK code examples for all providers
func (cs *ConversationService) GetSDKExamples() map[string]interface{} {
	baseURL := fmt.Sprintf("http://%s:%d", cs.config.Host, cs.config.Port)
	apiKey := cs.config.LocalAPIKey

	return map[string]interface{}{
		"openai": map[string]interface{}{
			"name":        "OpenAI",
			"description": "Standard OpenAI SDK and compatible libraries",
			"base_url":    baseURL,
			"endpoint":    fmt.Sprintf("%s/api/v1/chat/completions", baseURL),
			"api_key":     apiKey,
			"examples": map[string]interface{}{
				"python": fmt.Sprintf(`import openai

# Configure OpenAI client
client = openai.OpenAI(
    api_key="%s",
    base_url="%s/api/v1"
)

# Send chat completion request
response = client.chat.completions.create(
    model="gpt-3.5-turbo",
    messages=[
        {"role": "system", "content": "You are a helpful assistant."},
        {"role": "user", "content": "Hello, how are you?"}
    ],
    temperature=0.7,
    max_tokens=1000
)

print(response.choices[0].message.content)`, apiKey, baseURL),
				"javascript": fmt.Sprintf(`import OpenAI from 'openai';

// Configure OpenAI client
const openai = new OpenAI({
    apiKey: '%s',
    baseURL: '%s/api/v1',
    dangerouslyAllowBrowser: true // Only for frontend testing
});

// Send chat completion request
async function chatCompletion() {
    try {
        const response = await openai.chat.completions.create({
            model: 'gpt-3.5-turbo',
            messages: [
                { role: 'system', content: 'You are a helpful assistant.' },
                { role: 'user', content: 'Hello, how are you?' }
            ],
            temperature: 0.7,
            max_tokens: 1000
        });

        console.log(response.choices[0].message.content);
    } catch (error) {
        console.error('Error:', error);
    }
}

chatCompletion();`, apiKey, baseURL),
				"curl": fmt.Sprintf(`curl -X POST "%s/api/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer %s" \
  -d '{
    "model": "gpt-3.5-turbo",
    "messages": [
      {"role": "system", "content": "You are a helpful assistant."},
      {"role": "user", "content": "Hello, how are you?"}
    ],
    "temperature": 0.7,
    "max_tokens": 1000
  }'`, baseURL, apiKey),
			},
		},
		"claude": map[string]interface{}{
			"name":        "Claude",
			"description": "Anthropic Claude SDK and Claude Code compatible",
			"base_url":    baseURL,
			"endpoint":    fmt.Sprintf("%s/api/anthropic/v1/messages", baseURL),
			"api_key":     apiKey,
			"examples": map[string]interface{}{
				"python": fmt.Sprintf(`import anthropic

# Configure Claude client
client = anthropic.Anthropic(
    api_key="%s",
    base_url="%s/api/anthropic/v1"
)

# Send message request
message = client.messages.create(
    model="claude-3-haiku-20240307",
    max_tokens=1000,
    temperature=0.7,
    messages=[
        {"role": "user", "content": "Hello, how are you?"}
    ]
)

print(message.content[0].text)`, apiKey, baseURL),
				"javascript": fmt.Sprintf(`// For Claude Code or compatible Anthropic SDK
const claudeConfig = {
    apiKey: '%s',
    baseURL: '%s/api/anthropic/v1'
};

// Send message request (Claude Code format)
async function sendMessage() {
    try {
        const response = await fetch('%s/api/anthropic/v1/messages', {
            method: 'POST',
            headers: {
                'Content-Type': 'application/json',
                'x-api-key': '%s',
                'anthropic-version': '2023-06-01'
            },
            body: JSON.stringify({
                model: 'claude-3-haiku-20240307',
                max_tokens: 1000,
                temperature: 0.7,
                messages: [
                    { role: 'user', content: 'Hello, how are you?' }
                ]
            })
        });

        const data = await response.json();
        console.log(data.content[0].text);
    } catch (error) {
        console.error('Error:', error);
    }
}

sendMessage();`, apiKey, baseURL, baseURL, apiKey),
				"curl": fmt.Sprintf(`curl -X POST "%s/api/anthropic/v1/messages" \
  -H "Content-Type: application/json" \
  -H "x-api-key: %s" \
  -H "anthropic-version: 2023-06-01" \
  -d '{
    "model": "claude-3-haiku-20240307",
    "max_tokens": 1000,
    "temperature": 0.7,
    "messages": [
      {"role": "user", "content": "Hello, how are you?"}
    ]
  }'`, baseURL, apiKey),
			},
		},
		"gemini": map[string]interface{}{
			"name":        "Gemini",
			"description": "Google Gemini API compatible interface",
			"base_url":    baseURL,
			"endpoint":    fmt.Sprintf("%s/api/gemini/completions", baseURL),
			"api_key":     apiKey,
			"examples": map[string]interface{}{
				"python": fmt.Sprintf(`import requests
import json

# Configure Gemini request
base_url = "%s"
api_key = "%s"

# Send completion request
def send_gemini_request():
    headers = {
        "Content-Type": "application/json"
    }

    data = {
        "model": "gemini-pro",
        "contents": [
            {
                "role": "user",
                "parts": [{"text": "Hello, how are you?"}]
            }
        ],
        "temperature": 0.7,
        "maxOutputTokens": 1000
    }

    response = requests.post(
        f"{base_url}/api/gemini/completions",
        headers=headers,
        json=data
    )

    if response.status_code == 200:
        result = response.json()
        print(result["candidates"][0]["content"]["parts"][0]["text"])
    else:
        print(f"Error: {response.status_code} - {response.text}")

send_gemini_request()`, baseURL, apiKey),
				"javascript": fmt.Sprintf(`// Gemini API JavaScript example
const geminiConfig = {
    baseURL: '%s',
    apiKey: '%s'
};

// Send completion request
async function sendGeminiRequest() {
    try {
        const response = await fetch('%s/api/gemini/completions', {
            method: 'POST',
            headers: {
                'Content-Type': 'application/json'
            },
            body: JSON.stringify({
                model: 'gemini-pro',
                contents: [
                    {
                        role: 'user',
                        parts: [{ text: 'Hello, how are you?' }]
                    }
                ],
                temperature: 0.7,
                maxOutputTokens: 1000
            })
        });

        const data = await response.json();
        console.log(data.candidates[0].content.parts[0].text);
    } catch (error) {
        console.error('Error:', error);
    }
}

sendGeminiRequest();`, baseURL, apiKey, baseURL),
				"curl": fmt.Sprintf(`curl -X POST "%s/api/gemini/completions" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-pro",
    "contents": [
      {
        "role": "user",
        "parts": [{"text": "Hello, how are you?"}]
      }
    ],
    "temperature": 0.7,
    "maxOutputTokens": 1000
  }'`, baseURL),
			},
		},
	}
}

// GetAvailableModels returns available models for each provider
func (cs *ConversationService) GetAvailableModels() (map[string][]string, error) {
	// Get all routes from route service
	routes, err := cs.routeService.GetAllRoutes()
	if err != nil {
		return nil, err
	}

	// Group models by provider based on their format
	models := map[string][]string{
		"openai":  {},
		"claude":  {},
		"gemini":  {},
	}

	for _, route := range routes {
		if !route.Enabled {
			continue
		}

		switch route.Format {
		case "openai", "":
			models["openai"] = append(models["openai"], route.Model)
		case "anthropic":
			models["claude"] = append(models["claude"], route.Model)
		case "gemini":
			models["gemini"] = append(models["gemini"], route.Model)
		}
	}

	return models, nil
}