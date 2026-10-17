#!/usr/bin/env python3
"""
Test script for Cursor2 Responses API endpoint.

Usage:
    python test_cursor2.py [--stream]
"""

import json
import sys
import requests

BASE_URL = "http://localhost:8080"
API_KEY = "sk-antigravity"

def test_non_streaming():
    """Test non-streaming Responses API request."""
    print("\n" + "="*60)
    print("Testing Cursor2 Responses API (non-streaming)")
    print("="*60)
    
    url = f"{BASE_URL}/cursor2/v1/responses"
    headers = {
        "Content-Type": "application/json",
        "Authorization": f"Bearer {API_KEY}"
    }
    
    # Responses API format request
    data = {
        "model": "claude-sonnet-4-5",
        "input": [
            {
                "type": "message",
                "role": "user",
                "content": [
                    {
                        "type": "input_text",
                        "text": "What is 2 + 2? Answer briefly."
                    }
                ]
            }
        ],
        "max_output_tokens": 1000,
        "stream": False
    }
    
    print(f"\nRequest URL: {url}")
    print(f"Request body: {json.dumps(data, indent=2)}")
    
    try:
        response = requests.post(url, headers=headers, json=data, timeout=60)
        print(f"\nStatus: {response.status_code}")
        
        if response.status_code == 200:
            result = response.json()
            print(f"\nResponse:")
            print(json.dumps(result, indent=2, ensure_ascii=False))
            
            # Check response structure
            assert "id" in result, "Missing 'id' in response"
            assert "object" in result and result["object"] == "response", "Invalid 'object' field"
            assert "output" in result, "Missing 'output' in response"
            assert "usage" in result, "Missing 'usage' in response"
            
            print("\n✓ Non-streaming test PASSED")
        else:
            print(f"\nError response: {response.text}")
            print("\n✗ Non-streaming test FAILED")
            
    except Exception as e:
        print(f"\nException: {e}")
        print("\n✗ Non-streaming test FAILED")


def test_streaming():
    """Test streaming Responses API request."""
    print("\n" + "="*60)
    print("Testing Cursor2 Responses API (streaming)")
    print("="*60)
    
    url = f"{BASE_URL}/cursor2/v1/responses"
    headers = {
        "Content-Type": "application/json",
        "Authorization": f"Bearer {API_KEY}"
    }
    
    data = {
        "model": "claude-sonnet-4-5",
        "input": [
            {
                "type": "message",
                "role": "user",
                "content": [
                    {
                        "type": "input_text",
                        "text": "Count from 1 to 5."
                    }
                ]
            }
        ],
        "max_output_tokens": 1000,
        "stream": True
    }
    
    print(f"\nRequest URL: {url}")
    print(f"Request body: {json.dumps(data, indent=2)}")
    
    try:
        response = requests.post(url, headers=headers, json=data, stream=True, timeout=60)
        print(f"\nStatus: {response.status_code}")
        
        if response.status_code == 200:
            print("\nStreaming events:")
            event_count = 0
            text_deltas = []
            
            for line in response.iter_lines():
                if line:
                    line = line.decode("utf-8")
                    if line.startswith("event:"):
                        event_type = line[6:].strip()
                        print(f"\n  Event: {event_type}")
                    elif line.startswith("data:"):
                        data_str = line[5:].strip()
                        if data_str and data_str != "[DONE]":
                            try:
                                event_data = json.loads(data_str)
                                event_count += 1
                                
                                # Extract text deltas
                                if event_data.get("type") == "response.output_text.delta":
                                    delta = event_data.get("delta", "")
                                    text_deltas.append(delta)
                                    print(f"    delta: '{delta}'")
                                elif event_data.get("type") == "response.completed":
                                    print(f"    status: {event_data.get('response', {}).get('status')}")
                                else:
                                    # Print abbreviated data for other events
                                    print(f"    type: {event_data.get('type')}")
                            except json.JSONDecodeError:
                                pass
            
            print(f"\n\nTotal events: {event_count}")
            print(f"Collected text: {''.join(text_deltas)}")
            print("\n✓ Streaming test PASSED")
        else:
            print(f"\nError response: {response.text}")
            print("\n✗ Streaming test FAILED")
            
    except Exception as e:
        print(f"\nException: {e}")
        print("\n✗ Streaming test FAILED")


def test_with_tools():
    """Test Responses API with function tools."""
    print("\n" + "="*60)
    print("Testing Cursor2 Responses API (with tools)")
    print("="*60)
    
    url = f"{BASE_URL}/cursor2/v1/responses"
    headers = {
        "Content-Type": "application/json",
        "Authorization": f"Bearer {API_KEY}"
    }
    
    data = {
        "model": "claude-sonnet-4-5",
        "input": [
            {
                "type": "message",
                "role": "user",
                "content": [
                    {
                        "type": "input_text",
                        "text": "What's the weather in Tokyo?"
                    }
                ]
            }
        ],
        "tools": [
            {
                "type": "function",
                "name": "get_weather",
                "description": "Get the current weather for a location",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "location": {
                            "type": "string",
                            "description": "The city name"
                        }
                    },
                    "required": ["location"]
                }
            }
        ],
        "max_output_tokens": 1000,
        "stream": False
    }
    
    print(f"\nRequest URL: {url}")
    
    try:
        response = requests.post(url, headers=headers, json=data, timeout=60)
        print(f"\nStatus: {response.status_code}")
        
        if response.status_code == 200:
            result = response.json()
            print(f"\nResponse:")
            print(json.dumps(result, indent=2, ensure_ascii=False))
            
            # Check for function_call in output
            output = result.get("output", [])
            has_function_call = any(item.get("type") == "function_call" for item in output)
            
            if has_function_call:
                print("\n✓ Tool call test PASSED (function_call found)")
            else:
                print("\n⚠ Tool call test: No function_call in output (model may have answered directly)")
        else:
            print(f"\nError response: {response.text}")
            print("\n✗ Tool call test FAILED")
            
    except Exception as e:
        print(f"\nException: {e}")
        print("\n✗ Tool call test FAILED")


if __name__ == "__main__":
    stream_only = "--stream" in sys.argv
    tools_only = "--tools" in sys.argv
    
    if stream_only:
        test_streaming()
    elif tools_only:
        test_with_tools()
    else:
        test_non_streaming()
        test_streaming()
        test_with_tools()
    
    print("\n" + "="*60)
    print("All tests completed!")
    print("="*60)
