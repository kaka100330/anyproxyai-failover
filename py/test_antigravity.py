#!/usr/bin/env python3
"""
Test script for Antigravity Proxy transformers.
"""

import json
from antigravity_proxy import (
    RequestTransformer,
    NonStreamingProcessor,
    StreamingProcessor,
    OpenAIConverter,
    OpenAIStreamingProcessor,
    Config,
    get_mapped_model,
    clean_json_schema,
    ClaudeUsage,
)


def test_config():
    """Test config loading."""
    print("=== Testing Config ===")
    config = Config()
    print(f"  Default host: {config.host}")
    print(f"  Default port: {config.port}")
    print(f"  Default api_key: {config.api_key}")


def test_model_mapping():
    """Test model mapping."""
    print("\n=== Testing Model Mapping ===")
    
    test_cases = [
        ("claude-sonnet-4-5", "claude-sonnet-4-5"),
        ("claude-3-5-sonnet-20241022", "claude-sonnet-4-5"),
        ("claude-opus-4-5", "claude-opus-4-5-thinking"),
        ("gemini-2.5-flash", "gemini-2.5-flash"),
        ("gemini-3-pro-preview", "gemini-3-pro-high"),
        ("gpt-4", "claude-sonnet-4-5"),
        ("gpt-3.5-turbo", "gemini-2.5-flash"),
        ("unknown-model", "claude-sonnet-4-5"),
    ]
    
    for input_model, expected in test_cases:
        result = get_mapped_model(input_model)
        status = "✓" if result == expected else "✗"
        print(f"  {status} {input_model} -> {result}")


def test_openai_to_claude_conversion():
    """Test OpenAI to Claude format conversion."""
    print("\n=== Testing OpenAI to Claude Conversion ===")
    
    openai_req = {
        "model": "gpt-4",
        "messages": [
            {"role": "system", "content": "You are a helpful assistant."},
            {"role": "user", "content": "Hello!"},
            {"role": "assistant", "content": "Hi there!"},
            {"role": "user", "content": "How are you?"},
        ],
        "max_tokens": 1024,
        "temperature": 0.7,
        "stream": False,
    }
    
    claude_req = OpenAIConverter.openai_to_claude(openai_req)
    
    print(f"  Model: {claude_req['model']}")
    print(f"  Messages count: {len(claude_req['messages'])}")
    print(f"  Has system: {'system' in claude_req}")
    print(f"  Max tokens: {claude_req['max_tokens']}")
    print(f"  Temperature: {claude_req.get('temperature')}")


def test_claude_to_openai_response():
    """Test Claude to OpenAI response conversion."""
    print("\n=== Testing Claude to OpenAI Response Conversion ===")
    
    claude_resp = {
        "id": "msg_123",
        "type": "message",
        "role": "assistant",
        "model": "claude-sonnet-4-5",
        "content": [
            {"type": "text", "text": "Hello! I'm doing well."}
        ],
        "stop_reason": "end_turn",
        "usage": {
            "input_tokens": 10,
            "output_tokens": 8,
        }
    }
    
    openai_resp = OpenAIConverter.claude_to_openai_response(claude_resp)
    
    print(f"  ID: {openai_resp['id']}")
    print(f"  Object: {openai_resp['object']}")
    print(f"  Model: {openai_resp['model']}")
    print(f"  Finish reason: {openai_resp['choices'][0]['finish_reason']}")
    print(f"  Content: {openai_resp['choices'][0]['message']['content'][:30]}...")


def test_openai_tool_conversion():
    """Test OpenAI tool conversion."""
    print("\n=== Testing OpenAI Tool Conversion ===")
    
    openai_req = {
        "model": "gpt-4",
        "messages": [{"role": "user", "content": "Search for Python"}],
        "tools": [
            {
                "type": "function",
                "function": {
                    "name": "search",
                    "description": "Search the web",
                    "parameters": {
                        "type": "object",
                        "properties": {
                            "query": {"type": "string"}
                        },
                        "required": ["query"]
                    }
                }
            }
        ]
    }
    
    claude_req = OpenAIConverter.openai_to_claude(openai_req)
    
    print(f"  Has tools: {'tools' in claude_req}")
    if claude_req.get('tools'):
        tool = claude_req['tools'][0]
        print(f"  Tool name: {tool['name']}")
        print(f"  Tool description: {tool['description']}")


def test_request_transformer():
    """Test request transformation."""
    print("\n=== Testing Request Transformer ===")
    
    transformer = RequestTransformer()
    
    claude_req = {
        "model": "claude-sonnet-4-5",
        "messages": [
            {"role": "user", "content": "Hello, how are you?"}
        ],
        "max_tokens": 1024,
    }
    
    result = transformer.transform(claude_req, "test-project", "claude-sonnet-4-5")
    
    print(f"  Project: {result['project']}")
    print(f"  Model: {result['model']}")
    print(f"  Request type: {result['requestType']}")
    print(f"  Contents count: {len(result['request']['contents'])}")


def test_response_transformer():
    """Test response transformation."""
    print("\n=== Testing Response Transformer ===")
    
    gemini_resp = {
        "candidates": [{
            "content": {
                "parts": [{"text": "Hello! I'm doing well."}]
            },
            "finishReason": "STOP"
        }],
        "usageMetadata": {
            "promptTokenCount": 10,
            "candidatesTokenCount": 15,
        }
    }
    
    processor = NonStreamingProcessor()
    claude_resp, usage = processor.process(gemini_resp, "test-123", "claude-sonnet-4-5")
    
    print(f"  Response ID: {claude_resp['id']}")
    print(f"  Content blocks: {len(claude_resp['content'])}")
    print(f"  Stop reason: {claude_resp['stop_reason']}")
    print(f"  Input tokens: {usage.input_tokens}")
    print(f"  Output tokens: {usage.output_tokens}")


def test_streaming_processor():
    """Test streaming processor."""
    print("\n=== Testing Streaming Processor ===")
    
    processor = StreamingProcessor("claude-sonnet-4-5")
    
    sse_lines = [
        'data: {"response": {"candidates": [{"content": {"parts": [{"text": "Hello"}]}}], "usageMetadata": {"promptTokenCount": 5, "candidatesTokenCount": 1}}, "responseId": "test-123"}',
        'data: {"response": {"candidates": [{"content": {"parts": [{"text": " world!"}]}}]}}',
        'data: {"response": {"candidates": [{"finishReason": "STOP"}]}}',
    ]
    
    all_events = []
    for line in sse_lines:
        events = processor.process_line(line)
        if events:
            all_events.append(events)
    
    final_events, usage = processor.finish()
    if final_events:
        all_events.append(final_events)
    
    print(f"  Generated {len(all_events)} event batches")
    print(f"  Final usage - Input: {usage.input_tokens}, Output: {usage.output_tokens}")


def test_openai_streaming_processor():
    """Test OpenAI streaming processor."""
    print("\n=== Testing OpenAI Streaming Processor ===")
    
    processor = OpenAIStreamingProcessor("gpt-4")
    
    # Simulate Claude events
    events = [
        ("message_start", {"message": {"id": "msg_123", "model": "gpt-4"}}),
        ("content_block_start", {"content_block": {"type": "text", "text": ""}}),
        ("content_block_delta", {"delta": {"type": "text_delta", "text": "Hello"}}),
        ("content_block_delta", {"delta": {"type": "text_delta", "text": " world!"}}),
        ("content_block_stop", {}),
        ("message_delta", {"delta": {"stop_reason": "end_turn"}}),
        ("message_stop", {}),
    ]
    
    all_chunks = []
    for event_type, data in events:
        chunk = processor.process_claude_event(event_type, data)
        if chunk:
            all_chunks.append(chunk)
    
    print(f"  Generated {len(all_chunks)} OpenAI chunks")
    
    # Check format
    if all_chunks:
        first_chunk = all_chunks[0]
        if "data:" in first_chunk:
            data_part = first_chunk.split("data:")[1].strip().split("\n")[0]
            parsed = json.loads(data_part)
            print(f"  First chunk object: {parsed.get('object')}")


def test_tool_use_response():
    """Test tool use response transformation."""
    print("\n=== Testing Tool Use Response ===")
    
    gemini_resp = {
        "candidates": [{
            "content": {
                "parts": [{
                    "functionCall": {
                        "name": "search",
                        "args": {"query": "Python tutorials"},
                        "id": "call_123"
                    }
                }]
            },
            "finishReason": "STOP"
        }],
        "usageMetadata": {"promptTokenCount": 20, "candidatesTokenCount": 10}
    }
    
    processor = NonStreamingProcessor()
    claude_resp, _ = processor.process(gemini_resp, "test-tool-123", "claude-sonnet-4-5")
    
    print(f"  Stop reason: {claude_resp['stop_reason']}")
    if claude_resp['content']:
        block = claude_resp['content'][0]
        print(f"  Block type: {block['type']}")
        print(f"  Tool name: {block.get('name')}")
    
    # Convert to OpenAI
    openai_resp = OpenAIConverter.claude_to_openai_response(claude_resp)
    print(f"  OpenAI finish_reason: {openai_resp['choices'][0]['finish_reason']}")
    if openai_resp['choices'][0]['message'].get('tool_calls'):
        print(f"  OpenAI tool_calls: {len(openai_resp['choices'][0]['message']['tool_calls'])}")


if __name__ == "__main__":
    test_config()
    test_model_mapping()
    test_openai_to_claude_conversion()
    test_claude_to_openai_response()
    test_openai_tool_conversion()
    test_request_transformer()
    test_response_transformer()
    test_streaming_processor()
    test_openai_streaming_processor()
    test_tool_use_response()
    
    print("\n=== All Tests Completed ===")
