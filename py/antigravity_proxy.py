#!/usr/bin/env python3
"""
Antigravity API Proxy - Claude/OpenAI to Gemini Protocol Converter

支持 OpenAI �?Anthropic Claude 两种 API 接口格式�?
通过 config.json 配置端口�?API Key�?

Usage:
    python antigravity_proxy.py
"""

import json
import uuid
import hashlib
import random
import time
import asyncio
import os
import sys
from collections import deque
from dataclasses import dataclass, field
from typing import Any, Optional
from aiohttp import web, ClientSession, ClientTimeout

# ============ Constants ============

CONFIG_FILE = "config.json"

# Try to find config in multiple locations
def find_config_file() -> str:
    """Find config file in current dir, script dir, or py subdir."""
    candidates = [
        "config.json",
        os.path.join(os.path.dirname(__file__), "config.json"),
        "py/config.json",
    ]
    for path in candidates:
        if os.path.exists(path):
            return path
    return "config.json"


# ============ Rate Limiter ============

class RateLimiter:
    """Simple rate limiter using sliding window."""
    
    def __init__(self, max_requests: int = 10, window_seconds: float = 60.0, min_interval: float = 2.0):
        """
        Args:
            max_requests: Maximum requests allowed in the time window
            window_seconds: Time window in seconds
            min_interval: Minimum interval between requests in seconds
        """
        self.max_requests = max_requests
        self.window_seconds = window_seconds
        self.min_interval = min_interval
        self.request_times: deque = deque()
        self.last_request_time: float = 0
        self._lock = asyncio.Lock()
    
    async def acquire(self) -> float:
        """
        Wait until a request can be made.
        Returns the time waited in seconds.
        """
        async with self._lock:
            now = time.time()
            wait_time = 0.0
            
            # Clean old requests outside the window
            while self.request_times and self.request_times[0] < now - self.window_seconds:
                self.request_times.popleft()
            
            # Check minimum interval
            if self.last_request_time > 0:
                time_since_last = now - self.last_request_time
                if time_since_last < self.min_interval:
                    wait_time = self.min_interval - time_since_last
            
            # Check window limit
            if len(self.request_times) >= self.max_requests:
                oldest = self.request_times[0]
                wait_for_window = oldest + self.window_seconds - now
                if wait_for_window > wait_time:
                    wait_time = wait_for_window
            
            if wait_time > 0:
                debug_print(f"[RateLimiter] Waiting {wait_time:.1f}s before next request...")
                await asyncio.sleep(wait_time)
                now = time.time()
            
            self.request_times.append(now)
            self.last_request_time = now
            return wait_time


# Global rate limiter instance
_rate_limiter: Optional[RateLimiter] = None

def get_rate_limiter() -> RateLimiter:
    global _rate_limiter
    if _rate_limiter is None:
        _rate_limiter = RateLimiter(max_requests=10, window_seconds=60.0, min_interval=2.0)
    return _rate_limiter


# Antigravity API URLs (fallback order: sandbox -> daily -> prod)
BASE_URLS = [
    "https://daily-cloudcode-pa.sandbox.googleapis.com",
    "https://daily-cloudcode-pa.googleapis.com",
    "https://cloudcode-pa.googleapis.com",
]

# OAuth Constants
# Credentials are not redistributed with this fork; supply your own.
CLIENT_ID = os.environ.get("ANTIGRAVITY_CLIENT_ID", "")
CLIENT_SECRET = os.environ.get("ANTIGRAVITY_CLIENT_SECRET", "")
TOKEN_URL = "https://oauth2.googleapis.com/token"
USER_AGENT = "antigravity/1.104.0 darwin/arm64"

# Default stop sequences
DEFAULT_STOP_SEQUENCES = [
    "<|user|>", "<|endoftext|>", "<|end_of_turn|>", "[DONE]", "\n\nHuman:",
]

DUMMY_THOUGHT_SIGNATURE = "skip_thought_signature_validator"

# ============ Global Thought Signature Store ============
# Used to pass signature between streaming response and subsequent requests
# (Like Antigravity-Manager's global storage)

_global_thought_signature: Optional[str] = None

def global_thought_signature_store(sig: str):
    """Store thought signature globally for later use in tool calls."""
    global _global_thought_signature
    # Only store if new signature is longer (avoid short signatures overwriting valid ones)
    if _global_thought_signature is None or len(sig) > len(_global_thought_signature):
        debug_print(f"[ThoughtSig] Storing new signature (length: {len(sig)})")
        _global_thought_signature = sig

def global_thought_signature_get() -> Optional[str]:
    """Get the globally stored thought signature."""
    return _global_thought_signature

def global_thought_signature_clear():
    """Clear the globally stored thought signature."""
    global _global_thought_signature
    _global_thought_signature = None

# Antigravity identity prompt (MUST match Go version exactly - required by upstream)
ANTIGRAVITY_IDENTITY = """<identity>
You are Antigravity, a powerful agentic AI coding assistant designed by the Google Deepmind team working on Advanced Agentic Coding.
You are pair programming with a USER to solve their coding task. The task may require creating a new codebase, modifying or debugging an existing codebase, or simply answering a question.
The USER will send you requests, which you must always prioritize addressing. Along with each USER request, we will attach additional metadata about their current state, such as what files they have open and where their cursor is.
This information may or may not be relevant to the coding task, it is up for you to decide.
</identity>
<communication_style>
- **Proactiveness**. As an agent, you are allowed to be proactive, but only in the course of completing the user's task. For example, if the user asks you to add a new component, you can edit the code, verify build and test statuses, and take any other obvious follow-up actions, such as performing additional research. However, avoid surprising the user. For example, if the user asks HOW to approach something, you should answer their question and instead of jumping into editing a file.</communication_style>"""

# Supported models (for listing only, not for validation)
SUPPORTED_MODELS = {
    "claude-opus-4-5-thinking", "claude-sonnet-4-5", "claude-sonnet-4-5-thinking",
    "gemini-2.5-flash", "gemini-2.5-flash-lite", "gemini-2.5-flash-thinking",
    "gemini-3-flash", "gemini-3-pro-low", "gemini-3-pro-high", "gemini-3-pro-image",
}

# Model prefix mapping (only for common aliases, otherwise pass through)
PREFIX_MAPPING = [
    ("gemini-2.5-flash-image", "gemini-3-pro-image"),
    ("gemini-3-pro-image", "gemini-3-pro-image"),
    ("gemini-3-flash", "gemini-3-flash"),
    ("claude-3-5-sonnet", "claude-sonnet-4-5"),
    ("claude-sonnet-4-5", "claude-sonnet-4-5"),
    ("claude-haiku-4-5", "claude-sonnet-4-5"),
    ("claude-opus-4-5", "claude-opus-4-5-thinking"),
    ("claude-3-haiku", "claude-sonnet-4-5"),
    ("claude-sonnet-4", "claude-sonnet-4-5"),
    ("claude-haiku-4", "claude-sonnet-4-5"),
    ("claude-opus-4", "claude-opus-4-5-thinking"),
    ("gemini-3-pro", "gemini-3-pro-high"),
    ("gpt-4", "claude-sonnet-4-5"),
    ("gpt-3.5", "gemini-2.5-flash"),
]

# Minimum signature length for valid thinking blocks
MIN_SIGNATURE_LENGTH = 50

EXCLUDED_SCHEMA_KEYS = {
    "$schema", "$id", "$ref", "minLength", "maxLength", "pattern",
    "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf",
    "uniqueItems", "minItems", "maxItems", "oneOf", "anyOf", "allOf", "not",
    "if", "then", "else", "$defs", "definitions", "minProperties", "maxProperties",
    "patternProperties", "propertyNames", "dependencies", "dependentSchemas",
    "dependentRequired", "default", "const", "examples", "deprecated",
    "readOnly", "writeOnly", "contentMediaType", "contentEncoding", "strict",
}


# ============ Config ============

# 全局 debug 标志
_debug_enabled: bool = True

def debug_print(*args, **kwargs):
    """只在 debug 模式下打印"""
    if _debug_enabled:
        print(*args, **kwargs)

def set_debug_enabled(enabled: bool):
    """设置 debug 模式"""
    global _debug_enabled
    _debug_enabled = enabled

@dataclass
class Config:
    host: str = "0.0.0.0"
    port: int = 8080
    api_key: str = "sk-antigravity"
    refresh_token: str = ""
    project_id: str = ""
    # Rate limiting
    rate_limit_requests: int = 10  # Max requests per window
    rate_limit_window: float = 60.0  # Window in seconds
    rate_limit_interval: float = 2.0  # Min interval between requests
    # Thinking mode
    enable_thinking: bool = True  # Enable thinking by default
    thinking_budget: int = 10000  # Default thinking budget tokens
    # Debug mode
    debug: bool = True  # Enable debug logging by default
    
    @classmethod
    def load(cls, path: str = None) -> "Config":
        config = cls()
        if path is None:
            path = find_config_file()
        if os.path.exists(path):
            with open(path, "r", encoding="utf-8") as f:
                data = json.load(f)
            config.host = data.get("antigravity_host", data.get("host", "0.0.0.0"))
            config.port = data.get("antigravity_port", data.get("port", 8080))
            config.api_key = data.get("antigravity_api_key", data.get("local_api_key", "sk-antigravity"))
            config.refresh_token = data.get("antigravity_refresh_token", "")
            config.project_id = data.get("antigravity_project_id", "")
            # Rate limiting config
            config.rate_limit_requests = data.get("rate_limit_requests", 10)
            config.rate_limit_window = data.get("rate_limit_window", 60.0)
            config.rate_limit_interval = data.get("rate_limit_interval", 2.0)
            # Thinking config
            config.enable_thinking = data.get("enable_thinking", True)
            config.thinking_budget = data.get("thinking_budget", 10000)
            # Debug config
            config.debug = data.get("debug", True)
            # 设置全局 debug 标志
            set_debug_enabled(config.debug)
            if config.debug:
                print(f"Loading config from: {path}")
                print(f"Debug mode: enabled")
            else:
                print(f"Config loaded, debug mode: disabled")
        return config


# ============ Data Classes ============

@dataclass
class ClaudeUsage:
    input_tokens: int = 0
    output_tokens: int = 0
    cache_read_input_tokens: int = 0

    def to_dict(self) -> dict:
        d = {"input_tokens": self.input_tokens, "output_tokens": self.output_tokens}
        if self.cache_read_input_tokens:
            d["cache_read_input_tokens"] = self.cache_read_input_tokens
        return d
    
    def to_openai_dict(self) -> dict:
        return {
            "prompt_tokens": self.input_tokens + self.cache_read_input_tokens,
            "completion_tokens": self.output_tokens,
            "total_tokens": self.input_tokens + self.cache_read_input_tokens + self.output_tokens,
        }


# ============ Helper Functions ============

def get_mapped_model(requested_model: str) -> str:
    """Map requested model to supported model.
    
    Strategy (matching Antigravity-Manager):
    1. If model is in SUPPORTED_MODELS, use it directly
    2. Check prefix mappings for common aliases
    3. For gemini-* models, pass through as-is (allow any gemini model)
    4. For claude-* models, pass through as-is (allow any claude model)
    5. Default fallback to claude-sonnet-4-5
    """
    if requested_model in SUPPORTED_MODELS:
        return requested_model
    
    # Check prefix mappings
    for prefix, target in PREFIX_MAPPING:
        if requested_model.startswith(prefix):
            return target
    
    # Pass through gemini-* and claude-* models (self-adaptive)
    if requested_model.startswith("gemini-"):
        return requested_model
    if requested_model.startswith("claude-"):
        return requested_model
    
    # Default fallback
    return "claude-sonnet-4-5"


def model_supports_thinking(model: str) -> bool:
    """Check if a model supports thinking mode.
    
    Based on Antigravity-Manager logic:
    - Models with "-thinking" suffix support thinking
    - Gemini 3 Pro models support thinking (gemini-3-pro-*)
    - Claude models support thinking
    """
    model_lower = model.lower()
    
    # Explicit thinking models
    if "-thinking" in model_lower:
        return True
    
    # Gemini 3 Pro models support thinking
    if "gemini-3-pro" in model_lower:
        return True
    
    # Claude models support thinking
    if model_lower.startswith("claude-"):
        return True
    
    # Regular Gemini models (gemini-2.5-flash, gemini-3-flash) do NOT support thinking
    return False


def should_enable_thinking_by_default(model: str) -> bool:
    """Check if thinking should be enabled by default for a model.
    
    Based on Antigravity-Manager logic:
    - Opus 4.5 models enable thinking by default
    - Explicit -thinking models enable thinking by default
    """
    model_lower = model.lower()
    
    # Opus 4.5 variants
    if "opus-4-5" in model_lower or "opus-4.5" in model_lower:
        return True
    
    # Explicit thinking models
    if "-thinking" in model_lower:
        return True
    
    return False


def generate_random_id(length: int = 12) -> str:
    chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
    return "".join(random.choice(chars) for _ in range(length))


def generate_stable_session_id(contents: list) -> str:
    for content in contents:
        if content.get("role") == "user":
            parts = content.get("parts", [])
            if parts and parts[0].get("text"):
                h = hashlib.sha256(parts[0]["text"].encode()).digest()
                n = int.from_bytes(h[:8], "big") & 0x7FFFFFFFFFFFFFFF
                return f"-{n}"
    return f"-{random.randint(0, 9_000_000_000_000_000_000)}"


def clean_json_schema(schema: Optional[dict]) -> dict:
    """Clean JSON Schema for Gemini API compatibility.
    
    Gemini API has strict requirements:
    - type must be uppercase: STRING, NUMBER, INTEGER, BOOLEAN, ARRAY, OBJECT
    - Many JSON Schema keywords are not supported
    - Properties must have valid nested schemas
    """
    if not schema:
        return {"type": "OBJECT", "properties": {}}
    
    def clean_value(value: Any, depth: int = 0) -> Any:
        if depth > 10:  # Prevent infinite recursion
            return value
            
        if isinstance(value, dict):
            result = {}
            for k, v in value.items():
                if k in EXCLUDED_SCHEMA_KEYS:
                    continue
                if k == "type":
                    # Convert type to uppercase for Gemini
                    if isinstance(v, str):
                        v_upper = v.upper()
                        # Map common types
                        if v_upper in ("STRING", "NUMBER", "INTEGER", "BOOLEAN", "ARRAY", "OBJECT"):
                            result[k] = v_upper
                        elif v_upper == "NULL":
                            result[k] = "STRING"  # Gemini doesn't support null type
                        else:
                            result[k] = "STRING"  # Default to STRING for unknown types
                    elif isinstance(v, list):
                        # Handle union types like ["string", "null"] - take first non-null
                        for t in v:
                            if isinstance(t, str) and t.lower() != "null":
                                result[k] = t.upper()
                                break
                        if k not in result:
                            result[k] = "STRING"
                    else:
                        result[k] = "STRING"
                elif k == "format" and v in ("date-time", "date", "time"):
                    result[k] = v
                elif k == "additionalProperties":
                    result[k] = v if isinstance(v, bool) else False
                elif k == "properties" and isinstance(v, dict):
                    # Clean nested properties
                    cleaned_props = {}
                    for prop_name, prop_schema in v.items():
                        if isinstance(prop_schema, dict):
                            cleaned_props[prop_name] = clean_value(prop_schema, depth + 1)
                        elif isinstance(prop_schema, str):
                            # Handle shorthand like "string" -> {"type": "STRING"}
                            cleaned_props[prop_name] = {"type": prop_schema.upper()}
                        else:
                            cleaned_props[prop_name] = {"type": "STRING"}
                    result[k] = cleaned_props
                elif k == "items" and isinstance(v, dict):
                    # Clean array items schema
                    result[k] = clean_value(v, depth + 1)
                elif k in ("description", "enum", "required"):
                    # Keep these as-is
                    result[k] = v
                else:
                    result[k] = clean_value(v, depth + 1)
            return result
        elif isinstance(value, list):
            return [clean_value(item, depth + 1) for item in value]
        elif isinstance(value, str):
            return value
        return value
    
    cleaned = clean_value(schema)
    if "type" not in cleaned:
        cleaned["type"] = "OBJECT"
    if cleaned.get("type") == "OBJECT" and "properties" not in cleaned:
        cleaned["properties"] = {}
    return cleaned


# ============ Thinking Block Validation (matching Antigravity-Manager) ============

def has_valid_signature(block: dict) -> bool:
    """Check if a thinking block has a valid signature.
    
    Based on Antigravity-Manager logic:
    - Empty thinking + any signature = valid (trailing signature case)
    - Non-empty thinking + signature >= MIN_SIGNATURE_LENGTH = valid
    """
    if block.get("type") != "thinking":
        return True  # Non-thinking blocks are always valid
    
    thinking = block.get("thinking", "")
    signature = block.get("signature", "")
    
    # Empty thinking + any signature = valid
    if not thinking and signature:
        return True
    
    # Non-empty thinking + valid signature = valid
    if signature and len(signature) >= MIN_SIGNATURE_LENGTH:
        return True
    
    return False


def filter_invalid_thinking_blocks(messages: list) -> int:
    """Filter invalid thinking blocks from messages.
    
    Based on Antigravity-Manager logic:
    - Remove thinking blocks without valid signatures
    - Convert thinking content to text if signature is invalid but content exists
    - Only process assistant/model messages
    - NEW: Try to repair thinking blocks using global signature if available
    
    Returns the number of filtered blocks.
    """
    total_filtered = 0
    global_sig = global_thought_signature_get()
    
    for msg in messages:
        role = msg.get("role", "")
        if role not in ("assistant", "model"):
            continue
        
        content = msg.get("content")
        if not isinstance(content, list):
            continue
        
        new_blocks = []
        for block in content:
            if block.get("type") == "thinking":
                if has_valid_signature(block):
                    # Valid thinking block - keep it but remove cache_control
                    cleaned = {
                        "type": "thinking",
                        "thinking": block.get("thinking", ""),
                    }
                    if block.get("signature"):
                        cleaned["signature"] = block["signature"]
                    new_blocks.append(cleaned)
                elif global_sig and len(global_sig) >= MIN_SIGNATURE_LENGTH:
                    # Invalid signature but we have a global one - repair the block
                    thinking_text = block.get("thinking", "")
                    debug_print(f"[Thinking-Filter] Repairing thinking block with global signature (len={len(thinking_text)})")
                    new_blocks.append({
                        "type": "thinking",
                        "thinking": thinking_text,
                        "signature": global_sig
                    })
                else:
                    # Invalid signature - convert to text if has content
                    thinking_text = block.get("thinking", "")
                    if thinking_text.strip():
                        debug_print(f"[Thinking-Filter] Converting thinking block with invalid signature to text (len={len(thinking_text)})")
                        new_blocks.append({"type": "text", "text": thinking_text})
                    else:
                        debug_print(f"[Thinking-Filter] Dropping empty thinking block with invalid signature")
                    total_filtered += 1
            else:
                # Non-thinking block - keep as-is but remove cache_control
                cleaned_block = {k: v for k, v in block.items() if k != "cache_control"}
                new_blocks.append(cleaned_block)
        
        # Ensure message has at least one block
        if not new_blocks:
            new_blocks.append({"type": "text", "text": ""})
        
        msg["content"] = new_blocks
    
    if total_filtered > 0:
        debug_print(f"[Thinking-Filter] Filtered {total_filtered} invalid thinking block(s)")
    
    return total_filtered


def should_disable_thinking_due_to_history(messages: list) -> bool:
    """Check if thinking should be disabled due to incompatible history.
    
    Based on Antigravity-Manager logic:
    If the last assistant message has tool_use but no thinking block,
    it means this is a flow started by a non-thinking model.
    Forcing thinking would cause "final assistant message must start with thinking" error.
    """
    # Find last assistant message
    for msg in reversed(messages):
        if msg.get("role") in ("assistant", "model"):
            content = msg.get("content")
            if not isinstance(content, list):
                return False
            
            has_tool_use = any(b.get("type") == "tool_use" for b in content)
            has_thinking = any(b.get("type") == "thinking" for b in content)
            
            # Tool use without thinking = incompatible
            if has_tool_use and not has_thinking:
                debug_print(f"[Thinking-Mode] Detected ToolUse without Thinking in history. Disabling thinking.")
                return True
            
            # Only check the most recent assistant message
            return False
    
    return False


def has_valid_signature_for_function_calls(messages: list) -> bool:
    """Check if we have any valid signature available for function calls.
    
    Based on Antigravity-Manager logic:
    This prevents API rejection due to missing thought_signature.
    """
    # Check global store first
    global_sig = global_thought_signature_get()
    if global_sig and len(global_sig) >= MIN_SIGNATURE_LENGTH:
        return True
    
    # Check message history for valid signatures
    for msg in reversed(messages):
        if msg.get("role") in ("assistant", "model"):
            content = msg.get("content")
            if not isinstance(content, list):
                continue
            
            for block in content:
                if block.get("type") == "thinking":
                    sig = block.get("signature", "")
                    if sig and len(sig) >= MIN_SIGNATURE_LENGTH:
                        return True
    
    return False


# ============ OpenAI Format Converter ============

def is_valid_value(v) -> bool:
    """Check if a value is valid (not undefined/null string)."""
    if v is None:
        return False
    if isinstance(v, str) and v in ("[undefined]", "undefined", "null", "[null]", ""):
        return False
    return True

# ============ Gemini Format Converter ============

class GeminiConverter:
    """Convert between Gemini and Claude/internal formats.
    
    Gemini API format:
    - contents: [{role: "user"|"model", parts: [{text: "..."}, {functionCall: {...}}, ...]}]
    - systemInstruction: {role: "user", parts: [{text: "..."}]}
    - generationConfig: {maxOutputTokens, temperature, topP, thinkingConfig, ...}
    - tools: [{functionDeclarations: [{name, description, parameters}]}]
    """
    
    @staticmethod
    def _clean_undefined(value: Any) -> Any:
        """Recursively clean [undefined] and similar invalid values from request.
        
        Cherry Studio and other clients often send "[undefined]" strings.
        """
        if value is None:
            return None
        if isinstance(value, str):
            if value in ("[undefined]", "undefined", "[null]", "null", ""):
                return None
            return value
        if isinstance(value, dict):
            cleaned = {}
            for k, v in value.items():
                cleaned_v = GeminiConverter._clean_undefined(v)
                if cleaned_v is not None:
                    cleaned[k] = cleaned_v
            return cleaned
        if isinstance(value, list):
            cleaned = []
            for item in value:
                cleaned_item = GeminiConverter._clean_undefined(item)
                if cleaned_item is not None:
                    cleaned.append(cleaned_item)
            return cleaned
        return value
    
    @staticmethod
    def gemini_to_claude(gemini_req: dict, model: str) -> dict:
        """Convert Gemini request to Claude format for internal processing.
        
        This allows us to reuse the existing Claude->Gemini v1internal pipeline.
        """
        # Clean [undefined] values first
        gemini_req = GeminiConverter._clean_undefined(gemini_req) or {}
        
        messages = []
        system_content = None
        tool_id_to_name = {}
        
        # Extract system instruction
        sys_inst = gemini_req.get("systemInstruction")
        if sys_inst:
            parts = sys_inst.get("parts", [])
            texts = [p.get("text", "") for p in parts if p.get("text")]
            if texts:
                system_content = "\n".join(texts)
        
        # Convert contents to messages
        for content in gemini_req.get("contents", []):
            role = content.get("role", "user")
            parts = content.get("parts", [])
            
            # Map Gemini roles to Claude roles
            if role == "model":
                role = "assistant"
            
            claude_content = []
            
            for part in parts:
                # Text content
                if part.get("text") is not None:
                    text = part.get("text", "")
                    is_thought = part.get("thought", False)
                    signature = part.get("thoughtSignature", "")
                    
                    if is_thought:
                        # Thinking block
                        claude_content.append({
                            "type": "thinking",
                            "thinking": text,
                            "signature": signature
                        })
                    elif text.strip():
                        claude_content.append({"type": "text", "text": text})
                
                # Function call (tool_use)
                elif part.get("functionCall"):
                    fc = part["functionCall"]
                    tool_id = fc.get("id") or f"{fc.get('name', '')}-{generate_random_id()}"
                    tool_name = fc.get("name", "")
                    tool_id_to_name[tool_id] = tool_name
                    
                    block = {
                        "type": "tool_use",
                        "id": tool_id,
                        "name": tool_name,
                        "input": fc.get("args", {})
                    }
                    if part.get("thoughtSignature"):
                        block["signature"] = part["thoughtSignature"]
                    claude_content.append(block)
                
                # Function response (tool_result)
                elif part.get("functionResponse"):
                    fr = part["functionResponse"]
                    tool_id = fr.get("id", "")
                    result = fr.get("response", {}).get("result", "")
                    if isinstance(result, dict):
                        result = json.dumps(result)
                    
                    claude_content.append({
                        "type": "tool_result",
                        "tool_use_id": tool_id,
                        "content": str(result)
                    })
                
                # Inline data (image)
                elif part.get("inlineData"):
                    inline = part["inlineData"]
                    claude_content.append({
                        "type": "image",
                        "source": {
                            "type": "base64",
                            "media_type": inline.get("mimeType", "image/png"),
                            "data": inline.get("data", "")
                        }
                    })
            
            # Handle tool_result messages - they should be in user role
            has_tool_result = any(b.get("type") == "tool_result" for b in claude_content)
            if has_tool_result:
                role = "user"
            
            if claude_content:
                # For simple text content, use string format
                if len(claude_content) == 1 and claude_content[0].get("type") == "text":
                    messages.append({"role": role, "content": claude_content[0]["text"]})
                else:
                    messages.append({"role": role, "content": claude_content})
        
        # Build Claude request
        claude_req = {
            "model": model,
            "messages": messages,
            "max_tokens": 64000,
            "stream": False,
        }
        
        if system_content:
            claude_req["system"] = system_content
        
        # Convert generation config
        gen_config = gemini_req.get("generationConfig", {})
        if gen_config.get("maxOutputTokens"):
            claude_req["max_tokens"] = gen_config["maxOutputTokens"]
        if gen_config.get("temperature") is not None:
            claude_req["temperature"] = gen_config["temperature"]
        if gen_config.get("topP") is not None:
            claude_req["top_p"] = gen_config["topP"]
        
        # Convert thinking config
        thinking_config = gen_config.get("thinkingConfig")
        if thinking_config and thinking_config.get("includeThoughts"):
            claude_req["thinking"] = {
                "type": "enabled",
                "budget_tokens": thinking_config.get("thinkingBudget", 10000)
            }
        
        # Convert tools
        tools = gemini_req.get("tools", [])
        claude_tools = []
        for tool_group in tools:
            # Handle googleSearch tool
            if tool_group.get("googleSearch"):
                claude_tools.append({
                    "name": "web_search",
                    "description": "Search the web",
                    "input_schema": {"type": "object", "properties": {}}
                })
                continue
            
            # Handle function declarations
            func_decls = tool_group.get("functionDeclarations", [])
            for func in func_decls:
                claude_tools.append({
                    "name": func.get("name", ""),
                    "description": func.get("description", ""),
                    "input_schema": func.get("parameters", {})
                })
        
        if claude_tools:
            claude_req["tools"] = claude_tools
        
        return claude_req
    
    @staticmethod
    def claude_to_gemini_response(claude_resp: dict) -> dict:
        """Convert Claude response to Gemini format.
        
        Gemini response format:
        {
            "candidates": [{
                "content": {
                    "role": "model",
                    "parts": [{text: "..."}, {thought: true, text: "..."}, ...]
                },
                "finishReason": "STOP"|"MAX_TOKENS"|"TOOL_USE"
            }],
            "usageMetadata": {
                "promptTokenCount": N,
                "candidatesTokenCount": N,
                "totalTokenCount": N
            }
        }
        """
        parts = []
        
        for block in claude_resp.get("content", []):
            block_type = block.get("type", "")
            
            if block_type == "thinking":
                thinking_text = block.get("thinking", "")
                signature = block.get("signature", "")
                part = {"text": thinking_text, "thought": True}
                if signature:
                    part["thoughtSignature"] = signature
                parts.append(part)
            
            elif block_type == "text":
                text = block.get("text", "")
                if text:
                    parts.append({"text": text})
            
            elif block_type == "tool_use":
                fc = {
                    "name": block.get("name", ""),
                    "args": block.get("input", {}),
                    "id": block.get("id", "")
                }
                part = {"functionCall": fc}
                if block.get("signature"):
                    part["thoughtSignature"] = block["signature"]
                parts.append(part)
        
        # Map stop_reason to finishReason
        stop_reason = claude_resp.get("stop_reason", "end_turn")
        finish_reason = "STOP"
        if stop_reason == "tool_use":
            finish_reason = "TOOL_USE"
        elif stop_reason == "max_tokens":
            finish_reason = "MAX_TOKENS"
        
        # Build usage metadata
        usage = claude_resp.get("usage", {})
        usage_metadata = {
            "promptTokenCount": usage.get("input_tokens", 0) + usage.get("cache_read_input_tokens", 0),
            "candidatesTokenCount": usage.get("output_tokens", 0),
            "totalTokenCount": usage.get("input_tokens", 0) + usage.get("output_tokens", 0) + usage.get("cache_read_input_tokens", 0)
        }
        if usage.get("cache_read_input_tokens"):
            usage_metadata["cachedContentTokenCount"] = usage["cache_read_input_tokens"]
        
        return {
            "candidates": [{
                "content": {
                    "role": "model",
                    "parts": parts
                },
                "finishReason": finish_reason
            }],
            "usageMetadata": usage_metadata
        }
    
    @staticmethod
    def format_gemini_stream_chunk(gemini_resp: dict) -> str:
        """Format Gemini streaming chunk as SSE."""
        return f"data: {json.dumps(gemini_resp)}\n\n"


class OpenAIConverter:
    """Convert between OpenAI and Claude formats."""
    
    @staticmethod
    def openai_to_claude(openai_req: dict) -> dict:
        """Convert OpenAI chat completion request to Claude format.
        
        Key rules (matching Antigravity-Manager):
        1. System messages become Claude system prompt
        2. Assistant messages with tool_calls become assistant messages with tool_use blocks
        3. Tool role messages become user messages with tool_result blocks
        4. Tool results MUST follow their corresponding tool_use in the previous assistant message
        5. Consecutive messages of the same role should be merged
        """
        messages = []
        system_content = None
        pending_tool_results = []  # Collect tool results to merge with previous assistant
        
        for msg in openai_req.get("messages", []):
            role = msg.get("role", "user")
            content = msg.get("content", "")
            
            if role == "system":
                # Collect system content
                if is_valid_value(content):
                    if system_content:
                        system_content += "\n" + content
                    else:
                        system_content = content
                continue
            
            # Handle tool role messages - collect them for later
            if role == "tool":
                tool_content = msg.get("content", "")
                tool_call_id = msg.get("tool_call_id", "")
                if tool_call_id:
                    pending_tool_results.append({
                        "type": "tool_result",
                        "tool_use_id": tool_call_id,
                        "content": tool_content if is_valid_value(tool_content) else "",
                    })
                continue
            
            # Before processing non-tool messages, flush pending tool results
            if pending_tool_results:
                # Tool results go in a user message
                messages.append({
                    "role": "user",
                    "content": pending_tool_results
                })
                pending_tool_results = []
            
            if role == "assistant":
                claude_content = []
                
                # Handle reasoning_content (thinking) - MUST come first when thinking is enabled
                reasoning_content = msg.get("reasoning_content")
                if is_valid_value(reasoning_content):
                    # Get signature from global store
                    sig = global_thought_signature_get() or ""
                    claude_content.append({
                        "type": "thinking",
                        "thinking": reasoning_content,
                        "signature": sig
                    })
                
                # Handle text content
                if is_valid_value(content):
                    if isinstance(content, str) and content.strip():
                        claude_content.append({"type": "text", "text": content})
                    elif isinstance(content, list):
                        for item in content:
                            if item.get("type") == "text":
                                text = item.get("text", "")
                                if is_valid_value(text):
                                    claude_content.append({"type": "text", "text": text})
                
                # Handle tool_calls -> tool_use
                tool_calls = msg.get("tool_calls")
                if tool_calls and is_valid_value(tool_calls):
                    for tc in tool_calls:
                        if isinstance(tc, dict) and tc.get("type") == "function":
                            func = tc.get("function", {})
                            args = func.get("arguments", "{}")
                            try:
                                input_data = json.loads(args) if isinstance(args, str) else args
                            except:
                                input_data = {}
                            claude_content.append({
                                "type": "tool_use",
                                "id": tc.get("id", ""),
                                "name": func.get("name", ""),
                                "input": input_data,
                            })
                
                # Only add message if it has content
                if claude_content:
                    messages.append({"role": "assistant", "content": claude_content})
                continue
            
            # Handle user messages
            if role in ("user", "human"):
                role = "user"
            
            # Handle content formats
            if isinstance(content, str):
                if content.strip():
                    messages.append({"role": role, "content": content})
            elif isinstance(content, list):
                # Multi-modal content
                claude_content = []
                for item in content:
                    if item.get("type") == "text":
                        text = item.get("text", "")
                        if is_valid_value(text):
                            claude_content.append({"type": "text", "text": text})
                    elif item.get("type") == "image_url":
                        url = item.get("image_url", {}).get("url", "")
                        if url.startswith("data:"):
                            # Base64 image
                            parts = url.split(",", 1)
                            if len(parts) == 2:
                                media_type = parts[0].split(";")[0].replace("data:", "")
                                claude_content.append({
                                    "type": "image",
                                    "source": {
                                        "type": "base64",
                                        "media_type": media_type,
                                        "data": parts[1],
                                    }
                                })
                if claude_content:
                    messages.append({"role": role, "content": claude_content})
        
        # Flush any remaining tool results
        if pending_tool_results:
            messages.append({
                "role": "user",
                "content": pending_tool_results
            })
        
        # Merge consecutive messages of the same role (Claude requires alternating roles)
        merged_messages = []
        for msg in messages:
            if merged_messages and merged_messages[-1]["role"] == msg["role"]:
                # Merge with previous message
                prev_content = merged_messages[-1]["content"]
                curr_content = msg["content"]
                
                # Convert to list format if needed
                if isinstance(prev_content, str):
                    prev_content = [{"type": "text", "text": prev_content}]
                if isinstance(curr_content, str):
                    curr_content = [{"type": "text", "text": curr_content}]
                
                # Merge content lists
                merged_messages[-1]["content"] = prev_content + curr_content
            else:
                merged_messages.append(msg)
        
        messages = merged_messages
        
        # Get max_tokens, filter invalid values
        max_tokens = openai_req.get("max_tokens")
        if not is_valid_value(max_tokens) or not isinstance(max_tokens, (int, float)):
            max_tokens = 4096
        
        claude_req = {
            "model": openai_req.get("model", "claude-sonnet-4-5"),
            "messages": messages,
            "max_tokens": int(max_tokens),
            "stream": openai_req.get("stream", False),
        }
        
        if system_content and is_valid_value(system_content):
            claude_req["system"] = system_content
        
        temperature = openai_req.get("temperature")
        if is_valid_value(temperature) and isinstance(temperature, (int, float)):
            claude_req["temperature"] = temperature
        
        top_p = openai_req.get("top_p")
        if is_valid_value(top_p) and isinstance(top_p, (int, float)):
            claude_req["top_p"] = top_p
        
        # Convert tools - support multiple formats
        tools = openai_req.get("tools")
        if tools and is_valid_value(tools) and isinstance(tools, list):
            claude_tools = []
            debug_print(f"[OpenAI->Claude] Converting {len(tools)} tools")
            for i, tool in enumerate(tools):
                tool_type = tool.get("type", "")
                debug_print(f"  [{i}] type={tool_type}, keys={list(tool.keys())}")
                
                if tool_type == "function":
                    # Standard OpenAI format: {"type": "function", "function": {...}}
                    func = tool.get("function", {})
                    input_schema = clean_json_schema(func.get("parameters", {}))
                    claude_tools.append({
                        "name": func.get("name", ""),
                        "description": func.get("description", ""),
                        "input_schema": input_schema,
                    })
                elif "name" in tool:
                    # Direct format (Cursor/Claude style): {"name": "...", "description": "...", ...}
                    input_schema = clean_json_schema(tool.get("input_schema") or tool.get("parameters", {}))
                    claude_tools.append({
                        "name": tool.get("name", ""),
                        "description": tool.get("description", ""),
                        "input_schema": input_schema,
                    })
                elif "function" in tool and not tool_type:
                    # OpenAI format without explicit type
                    func = tool.get("function", {})
                    input_schema = clean_json_schema(func.get("parameters", {}))
                    claude_tools.append({
                        "name": func.get("name", ""),
                        "description": func.get("description", ""),
                        "input_schema": input_schema,
                    })
            
            debug_print(f"[OpenAI->Claude] Converted to {len(claude_tools)} Claude tools (schemas cleaned)")
            if claude_tools:
                claude_req["tools"] = claude_tools
        
        return claude_req
    
    @staticmethod
    def claude_to_openai_response(claude_resp: dict, stream: bool = False) -> dict:
        """Convert Claude response to OpenAI format.
        
        Key behavior (matching Antigravity-Manager):
        - Thinking content goes to reasoning_content field only
        - Regular content goes to content field
        - thoughtSignature is stored globally for later use
        """
        content = ""
        reasoning_content = ""
        tool_calls = []
        
        for block in claude_resp.get("content", []):
            if block.get("type") == "thinking":
                # Collect thinking content
                thinking_text = block.get("thinking", "")
                if thinking_text:
                    reasoning_content += thinking_text
                # Store signature globally for later use in tool calls
                sig = block.get("signature", "")
                if sig:
                    global_thought_signature_store(sig)
            elif block.get("type") == "text":
                content += block.get("text", "")
            elif block.get("type") == "tool_use":
                tool_calls.append({
                    "id": block.get("id", ""),
                    "type": "function",
                    "function": {
                        "name": block.get("name", ""),
                        "arguments": json.dumps(block.get("input", {})),
                    }
                })
        
        message = {"role": "assistant", "content": content if content else None}
        # Add reasoning_content for OpenAI format (like Antigravity-Manager)
        # Do NOT wrap in <thinking> tags - just use the field
        if reasoning_content:
            message["reasoning_content"] = reasoning_content
        if tool_calls:
            message["tool_calls"] = tool_calls
        
        finish_reason = "stop"
        if claude_resp.get("stop_reason") == "tool_use":
            finish_reason = "tool_calls"
        elif claude_resp.get("stop_reason") == "max_tokens":
            finish_reason = "length"
        
        usage = claude_resp.get("usage", {})
        
        return {
            "id": f"chatcmpl-{claude_resp.get('id', generate_random_id())}",
            "object": "chat.completion",
            "created": int(time.time()),
            "model": claude_resp.get("model", "claude-sonnet-4-5"),
            "choices": [{
                "index": 0,
                "message": message,
                "finish_reason": finish_reason,
            }],
            "usage": {
                "prompt_tokens": usage.get("input_tokens", 0),
                "completion_tokens": usage.get("output_tokens", 0),
                "total_tokens": usage.get("input_tokens", 0) + usage.get("output_tokens", 0),
            }
        }
    
    @staticmethod
    def format_openai_stream_chunk(
        chunk_id: str,
        model: str,
        delta: dict,
        finish_reason: Optional[str] = None
    ) -> str:
        """Format OpenAI streaming chunk."""
        chunk = {
            "id": chunk_id,
            "object": "chat.completion.chunk",
            "created": int(time.time()),
            "model": model,
            "choices": [{
                "index": 0,
                "delta": delta,
                "finish_reason": finish_reason,
            }]
        }
        return f"data: {json.dumps(chunk)}\n\n"


# ============ Request Transformer ============

class RequestTransformer:
    """Transform Claude requests to Gemini v1internal format."""
    
    def __init__(self, enable_identity_patch: bool = True):
        self.enable_identity_patch = enable_identity_patch
    
    def transform(self, claude_req: dict, project_id: str, mapped_model: str) -> dict:
        tool_id_to_name = {}
        
        # Step 1: Filter invalid thinking blocks from history (like Antigravity-Manager)
        messages = claude_req.get("messages", [])
        filter_invalid_thinking_blocks(messages)
        
        # Step 2: Determine if thinking should be enabled
        # Check if thinking is explicitly requested
        thinking_config = claude_req.get("thinking", {})
        is_thinking_requested = thinking_config.get("type") == "enabled"
        
        # Check if model supports thinking
        target_supports_thinking = model_supports_thinking(mapped_model)
        
        # Check if history is compatible with thinking
        history_compatible = not should_disable_thinking_due_to_history(messages)
        
        # Check if we have valid signatures for function calls
        has_function_calls = any(
            any(b.get("type") == "tool_use" for b in msg.get("content", []) if isinstance(msg.get("content"), list))
            for msg in messages
        )
        has_valid_sig = has_valid_signature_for_function_calls(messages) if has_function_calls else True
        
        # Final decision on thinking mode
        is_thinking = is_thinking_requested and target_supports_thinking and history_compatible
        
        # If thinking is requested but conditions not met, log why
        if is_thinking_requested and not is_thinking:
            reasons = []
            if not target_supports_thinking:
                reasons.append(f"model '{mapped_model}' does not support thinking")
            if not history_compatible:
                reasons.append("history has tool_use without thinking")
            if has_function_calls and not has_valid_sig:
                reasons.append("no valid signature for function calls")
            debug_print(f"[Transform] Thinking DISABLED: {', '.join(reasons)}")
        
        # Only Gemini models accept dummy thought signatures
        # Claude models require valid signatures from upstream
        allow_dummy = mapped_model.startswith("gemini-")
        
        contents, stripped = self._build_contents(messages, tool_id_to_name, is_thinking, allow_dummy)
        
        # If thinking blocks were stripped due to missing signatures, disable thinking mode
        if stripped:
            debug_print(f"[Transform] Thinking blocks stripped, disabling thinking mode")
            is_thinking = False
        
        system_instruction = self._build_system_instruction(claude_req.get("system"), mapped_model)
        generation_config = self._build_generation_config(claude_req, is_thinking)
        tools = self._build_tools(claude_req.get("tools", []))
        
        inner_request = {
            "contents": contents,
            "toolConfig": {"functionCallingConfig": {"mode": "VALIDATED"}},
            "sessionId": generate_stable_session_id(contents),
        }
        
        if system_instruction:
            inner_request["systemInstruction"] = system_instruction
        if generation_config:
            inner_request["generationConfig"] = generation_config
        if tools:
            inner_request["tools"] = tools
        
        result = {
            "project": project_id,
            "requestId": f"agent-{uuid.uuid4()}",
            "userAgent": "antigravity",
            "requestType": "agent",
            "model": mapped_model,
            "request": inner_request,
        }
        
        # Debug: print if identity patch is included
        has_identity = False
        if system_instruction and system_instruction.get("parts"):
            for part in system_instruction["parts"]:
                if "You are Antigravity" in part.get("text", ""):
                    has_identity = True
                    break
        debug_print(f"[Transform] model={mapped_model} thinking={is_thinking} has_identity={has_identity}")
        
        return result
    
    def _build_system_instruction(self, system: Any, model_name: str) -> Optional[dict]:
        parts = []
        user_has_identity = False
        user_parts = []
        
        if system:
            if isinstance(system, str):
                if system.strip():
                    user_parts.append({"text": system})
                    if "You are Antigravity" in system:
                        user_has_identity = True
            elif isinstance(system, list):
                for block in system:
                    if block.get("type") == "text" and block.get("text", "").strip():
                        user_parts.append({"text": block["text"]})
                        if "You are Antigravity" in block["text"]:
                            user_has_identity = True
        
        # CRITICAL: Always inject identity patch if not present (required by Antigravity upstream)
        if self.enable_identity_patch and not user_has_identity:
            parts.append({"text": ANTIGRAVITY_IDENTITY})
        parts.extend(user_parts)
        
        # Always return systemInstruction with identity patch, even if no user system prompt
        if parts:
            return {"role": "user", "parts": parts}
        # Fallback: inject identity even when no system prompt provided
        if self.enable_identity_patch:
            return {"role": "user", "parts": [{"text": ANTIGRAVITY_IDENTITY}]}
        return None
    
    def _build_contents(self, messages: list, tool_id_to_name: dict, is_thinking: bool, allow_dummy: bool) -> tuple:
        contents = []
        stripped = False
        
        for i, msg in enumerate(messages):
            role = "model" if msg.get("role") == "assistant" else msg.get("role", "user")
            parts, s = self._build_parts(msg.get("content"), tool_id_to_name, is_thinking, allow_dummy)
            if s:
                stripped = True
            
            # When thinking is enabled, ALL model messages must start with a thinking block
            if allow_dummy and role == "model" and is_thinking:
                has_thought = any(p.get("thought") for p in parts)
                if not has_thought and parts:
                    # Insert dummy thinking block at the beginning
                    debug_print(f"[_build_contents] Adding dummy thinking to model message {i}")
                    parts.insert(0, {"text": "Thinking...", "thought": True, "thoughtSignature": DUMMY_THOUGHT_SIGNATURE})
            
            if parts:
                contents.append({"role": role, "parts": parts})
        
        return contents, stripped
    
    def _build_parts(self, content: Any, tool_id_to_name: dict, is_thinking: bool, allow_dummy: bool) -> tuple:
        """Build Gemini parts from Claude content blocks.
        
        Based on Antigravity-Manager logic:
        - Thinking blocks MUST be first in the message
        - If thinking is disabled, convert thinking blocks to text
        - Empty thinking blocks cause errors, downgrade to text
        """
        parts = []
        stripped = False
        
        if isinstance(content, str):
            if content.strip() and content != "(no content)":
                parts.append({"text": content.strip()})
            return parts, False
        
        if not isinstance(content, list):
            return parts, False
        
        for block in content:
            block_type = block.get("type", "")
            
            if block_type == "text":
                text = block.get("text", "")
                if text.strip() and text != "(no content)":
                    parts.append({"text": text})
            
            elif block_type == "thinking":
                thinking_text = block.get("thinking", "")
                signature = block.get("signature", "")
                
                # [CRITICAL] Thinking block MUST be first (Gemini protocol)
                if parts:
                    debug_print(f"[_build_parts] Thinking block at non-zero index, downgrading to text")
                    if thinking_text.strip():
                        parts.append({"text": thinking_text})
                    continue
                
                # If thinking is disabled, convert to text
                if not is_thinking:
                    debug_print(f"[_build_parts] Thinking disabled, downgrading to text")
                    if thinking_text.strip():
                        parts.append({"text": thinking_text})
                    continue
                
                # Empty thinking blocks cause errors
                if not thinking_text.strip():
                    debug_print(f"[_build_parts] Empty thinking block, downgrading to placeholder")
                    parts.append({"text": "..."})
                    continue
                
                # Build thinking part
                part = {"text": thinking_text, "thought": True}
                
                if signature and len(signature) >= MIN_SIGNATURE_LENGTH:
                    part["thoughtSignature"] = signature
                elif not allow_dummy:
                    # No valid signature and can't use dummy - downgrade to text
                    debug_print(f"[_build_parts] No valid signature, downgrading to text")
                    parts.append({"text": thinking_text})
                    stripped = True
                    continue
                else:
                    part["thoughtSignature"] = DUMMY_THOUGHT_SIGNATURE
                
                parts.append(part)
            
            elif block_type == "image":
                source = block.get("source", {})
                if source.get("type") == "base64":
                    parts.append({
                        "inlineData": {
                            "mimeType": source.get("media_type", ""),
                            "data": source.get("data", ""),
                        }
                    })
            
            elif block_type == "tool_use":
                tool_id = block.get("id", "")
                tool_name = block.get("name", "")
                if tool_id and tool_name:
                    tool_id_to_name[tool_id] = tool_name
                
                part = {"functionCall": {"name": tool_name, "args": block.get("input"), "id": tool_id}}
                
                # Signature resolution (Priority: Block -> Global Store -> Dummy)
                block_sig = block.get("signature", "")
                global_sig = global_thought_signature_get()
                
                if block_sig and len(block_sig) >= MIN_SIGNATURE_LENGTH:
                    part["thoughtSignature"] = block_sig
                elif global_sig and len(global_sig) >= MIN_SIGNATURE_LENGTH:
                    debug_print(f"[_build_parts] Using global signature for tool_use (len={len(global_sig)})")
                    part["thoughtSignature"] = global_sig
                elif allow_dummy:
                    part["thoughtSignature"] = DUMMY_THOUGHT_SIGNATURE
                
                parts.append(part)
            
            elif block_type == "tool_result":
                tool_use_id = block.get("tool_use_id", "")
                func_name = block.get("name") or tool_id_to_name.get(tool_use_id, tool_use_id)
                result = self._parse_tool_result(block.get("content"), block.get("is_error", False))
                parts.append({"functionResponse": {"name": func_name, "response": {"result": result}, "id": tool_use_id}})
        
        return parts, stripped
    
    def _parse_tool_result(self, content: Any, is_error: bool) -> str:
        if not content:
            return "Tool execution failed." if is_error else "Success."
        if isinstance(content, str):
            return content if content.strip() else ("Failed." if is_error else "Success.")
        if isinstance(content, list):
            texts = [item.get("text", "") for item in content if isinstance(item, dict)]
            return "\n".join(texts) or ("Failed." if is_error else "Success.")
        return json.dumps(content)
    
    def _build_generation_config(self, req: dict, is_thinking: bool) -> dict:
        config = {"maxOutputTokens": 64000, "stopSequences": DEFAULT_STOP_SEQUENCES}
        
        debug_print(f"[GenerationConfig] is_thinking={is_thinking}")
        if is_thinking:
            thinking_config = req.get("thinking", {})
            budget = thinking_config.get("budget_tokens", 0)
            if "gemini-2.5-flash" in req.get("model", "") and budget > 24576:
                budget = 24576
            config["thinkingConfig"] = {"includeThoughts": True}
            if budget > 0:
                config["thinkingConfig"]["thinkingBudget"] = budget
            debug_print(f"[GenerationConfig] thinking enabled, budget={budget}")
        else:
            debug_print(f"[GenerationConfig] thinking NOT enabled")
        
        if req.get("temperature") is not None:
            config["temperature"] = req["temperature"]
        if req.get("top_p") is not None:
            config["topP"] = req["top_p"]
        
        return config
    
    def _build_tools(self, tools: list) -> list:
        if not tools:
            return []
        
        if any(t.get("name") == "web_search" for t in tools):
            return [{"googleSearch": {"enhancedContent": {"imageSearch": {"maxResultCount": 5}}}}]
        
        func_decls = []
        for tool in tools:
            name = tool.get("name", "").strip()
            if not name:
                continue
            if tool.get("type") == "custom" and tool.get("custom"):
                custom = tool["custom"]
                desc = custom.get("description", "")
                schema = custom.get("input_schema")
            else:
                desc = tool.get("description", "")
                schema = tool.get("input_schema")
            func_decls.append({"name": name, "description": desc, "parameters": clean_json_schema(schema)})
        
        return [{"functionDeclarations": func_decls}] if func_decls else []


# ============ Response Processors ============

class NonStreamingProcessor:
    """Process non-streaming Gemini response."""
    
    def __init__(self):
        self.content_blocks = []
        self.text_builder = ""
        self.thinking_builder = ""
        self.thinking_signature = ""
        self.trailing_signature = ""
        self.has_tool_call = False
    
    def process(self, gemini_resp: dict, response_id: str, original_model: str) -> tuple[dict, ClaudeUsage]:
        parts = []
        candidates = gemini_resp.get("candidates", [])
        if candidates and candidates[0].get("content"):
            parts = candidates[0]["content"].get("parts", [])
        
        for part in parts:
            self._process_part(part)
        
        self._flush_thinking()
        self._flush_text()
        
        if self.trailing_signature:
            self.content_blocks.append({"type": "thinking", "thinking": "", "signature": self.trailing_signature})
        
        return self._build_response(gemini_resp, response_id, original_model)
    
    def _process_part(self, part: dict):
        signature = part.get("thoughtSignature", "")
        
        if part.get("functionCall"):
            self._flush_thinking()
            self._flush_text()
            if self.trailing_signature:
                self.content_blocks.append({"type": "thinking", "thinking": "", "signature": self.trailing_signature})
                self.trailing_signature = ""
            
            self.has_tool_call = True
            fc = part["functionCall"]
            tool_id = fc.get("id") or f"{fc.get('name', '')}-{generate_random_id()}"
            item = {"type": "tool_use", "id": tool_id, "name": fc.get("name", ""), "input": fc.get("args")}
            if signature:
                item["signature"] = signature
            self.content_blocks.append(item)
            return
        
        text = part.get("text", "")
        is_thought = part.get("thought", False)
        
        if text or is_thought:
            if is_thought:
                self._flush_text()
                if self.trailing_signature:
                    self._flush_thinking()
                    self.content_blocks.append({"type": "thinking", "thinking": "", "signature": self.trailing_signature})
                    self.trailing_signature = ""
                self.thinking_builder += text
                if signature:
                    self.thinking_signature = signature
            else:
                if not text:
                    if signature:
                        self.trailing_signature = signature
                    return
                self._flush_thinking()
                if self.trailing_signature:
                    self._flush_text()
                    self.content_blocks.append({"type": "thinking", "thinking": "", "signature": self.trailing_signature})
                    self.trailing_signature = ""
                self.text_builder += text
                if signature:
                    self._flush_text()
                    self.content_blocks.append({"type": "thinking", "thinking": "", "signature": signature})
        
        if part.get("inlineData") and part["inlineData"].get("data"):
            self._flush_thinking()
            inline = part["inlineData"]
            self.text_builder += f"![image](data:{inline.get('mimeType', '')};base64,{inline['data']})"
            self._flush_text()
    
    def _flush_text(self):
        if self.text_builder:
            self.content_blocks.append({"type": "text", "text": self.text_builder})
            self.text_builder = ""
    
    def _flush_thinking(self):
        if self.thinking_builder or self.thinking_signature:
            self.content_blocks.append({"type": "thinking", "thinking": self.thinking_builder, "signature": self.thinking_signature})
            self.thinking_builder = ""
            self.thinking_signature = ""
    
    def _build_response(self, gemini_resp: dict, response_id: str, original_model: str) -> tuple[dict, ClaudeUsage]:
        finish_reason = ""
        candidates = gemini_resp.get("candidates", [])
        if candidates:
            finish_reason = candidates[0].get("finishReason", "")
        
        stop_reason = "end_turn"
        if self.has_tool_call:
            stop_reason = "tool_use"
        elif finish_reason == "MAX_TOKENS":
            stop_reason = "max_tokens"
        
        usage = ClaudeUsage()
        usage_meta = gemini_resp.get("usageMetadata", {})
        if usage_meta:
            cached = usage_meta.get("cachedContentTokenCount", 0)
            usage.input_tokens = usage_meta.get("promptTokenCount", 0) - cached
            usage.output_tokens = usage_meta.get("candidatesTokenCount", 0)
            usage.cache_read_input_tokens = cached
        
        return {
            "id": response_id,
            "type": "message",
            "role": "assistant",
            "model": original_model,
            "content": self.content_blocks,
            "stop_reason": stop_reason,
            "stop_sequence": None,
            "usage": usage.to_dict(),
        }, usage


class StreamingProcessor:
    """Process streaming Gemini response."""
    
    BLOCK_NONE, BLOCK_TEXT, BLOCK_THINKING, BLOCK_FUNCTION = 0, 1, 2, 3
    
    def __init__(self, original_model: str):
        self.original_model = original_model
        self.block_type = self.BLOCK_NONE
        self.block_index = 0
        self.message_start_sent = False
        self.message_stop_sent = False
        self.used_tool = False
        self.pending_signature = ""
        self.trailing_signature = ""
        self.input_tokens = 0
        self.output_tokens = 0
        self.cache_read_tokens = 0
        self.response_id = f"msg_{generate_random_id()}"
    
    def process_line(self, line: str) -> str:
        line = line.strip()
        if not line or not line.startswith("data:"):
            return ""
        
        data = line[5:].strip()
        if not data or data == "[DONE]":
            return ""
        
        try:
            v1_resp = json.loads(data)
        except json.JSONDecodeError:
            return ""
        
        gemini_resp = v1_resp.get("response", v1_resp)
        self.response_id = v1_resp.get("responseId") or gemini_resp.get("responseId") or self.response_id
        
        result = []
        
        if not self.message_start_sent:
            result.append(self._emit_message_start(v1_resp))
        
        usage_meta = gemini_resp.get("usageMetadata", {})
        if usage_meta:
            cached = usage_meta.get("cachedContentTokenCount", 0)
            self.input_tokens = usage_meta.get("promptTokenCount", 0) - cached
            self.output_tokens = usage_meta.get("candidatesTokenCount", 0)
            self.cache_read_tokens = cached
        
        candidates = gemini_resp.get("candidates", [])
        if candidates and candidates[0].get("content"):
            for part in candidates[0]["content"].get("parts", []):
                # Debug: print raw part to see what we're getting
                if part.get("thought"):
                    debug_print(f"[process_line] RAW PART WITH THOUGHT: {part}")
                result.append(self._process_part(part))
        
        if candidates:
            finish_reason = candidates[0].get("finishReason", "")
            if finish_reason:
                result.append(self._emit_finish(finish_reason))
        
        return "".join(result)
    
    def finish(self) -> tuple[str, ClaudeUsage]:
        result = "" if self.message_stop_sent else self._emit_finish("")
        return result, ClaudeUsage(self.input_tokens, self.output_tokens, self.cache_read_tokens)
    
    def _emit_message_start(self, v1_resp: dict) -> str:
        if self.message_start_sent:
            return ""
        
        gemini_resp = v1_resp.get("response", v1_resp)
        usage_meta = gemini_resp.get("usageMetadata", {})
        cached = usage_meta.get("cachedContentTokenCount", 0)
        
        usage = {"input_tokens": usage_meta.get("promptTokenCount", 0) - cached, "output_tokens": usage_meta.get("candidatesTokenCount", 0)}
        if cached:
            usage["cache_read_input_tokens"] = cached
        
        message = {
            "id": self.response_id,
            "type": "message",
            "role": "assistant",
            "content": [],
            "model": self.original_model,
            "stop_reason": None,
            "stop_sequence": None,
            "usage": usage,
        }
        
        self.message_start_sent = True
        return self._format_sse("message_start", {"type": "message_start", "message": message})
    
    def _process_part(self, part: dict) -> str:
        result = []
        signature = part.get("thoughtSignature", "")
        
        # Debug: log each part to see if thought is present
        is_thought = part.get("thought", False)
        text_preview = part.get("text", "")[:100] if part.get("text") else ""
        if is_thought:
            debug_print(f"[StreamingProcessor] *** THINKING PART FOUND *** thought={is_thought}, signature={bool(signature)}, text='{text_preview[:50]}...'")
        else:
            debug_print(f"[StreamingProcessor] part keys: {list(part.keys())}, thought={is_thought}, text_preview='{text_preview[:30]}...'")
        
        if part.get("functionCall"):
            if self.trailing_signature:
                result.append(self._end_block())
                result.append(self._emit_empty_thinking(self.trailing_signature))
                self.trailing_signature = ""
            result.append(self._process_function_call(part["functionCall"], signature))
            return "".join(result)
        
        text = part.get("text", "")
        is_thought = part.get("thought", False)
        
        if text or is_thought:
            if is_thought:
                result.append(self._process_thinking(text, signature))
            else:
                result.append(self._process_text(text, signature))
        
        if part.get("inlineData") and part["inlineData"].get("data"):
            inline = part["inlineData"]
            result.append(self._process_text(f"![image](data:{inline.get('mimeType', '')};base64,{inline['data']})", ""))
        
        return "".join(result)
    
    def _process_thinking(self, text: str, signature: str) -> str:
        result = []
        if self.trailing_signature:
            result.append(self._end_block())
            result.append(self._emit_empty_thinking(self.trailing_signature))
            self.trailing_signature = ""
        
        if self.block_type != self.BLOCK_THINKING:
            result.append(self._start_block(self.BLOCK_THINKING, {"type": "thinking", "thinking": ""}))
        
        if text:
            result.append(self._emit_delta("thinking_delta", {"thinking": text}))
        if signature:
            self.pending_signature = signature
        
        return "".join(result)
    
    def _process_text(self, text: str, signature: str) -> str:
        result = []
        
        if not text:
            if signature:
                self.trailing_signature = signature
            return ""
        
        if self.trailing_signature:
            result.append(self._end_block())
            result.append(self._emit_empty_thinking(self.trailing_signature))
            self.trailing_signature = ""
        
        if signature:
            result.append(self._start_block(self.BLOCK_TEXT, {"type": "text", "text": ""}))
            result.append(self._emit_delta("text_delta", {"text": text}))
            result.append(self._end_block())
            result.append(self._emit_empty_thinking(signature))
            return "".join(result)
        
        if self.block_type != self.BLOCK_TEXT:
            result.append(self._start_block(self.BLOCK_TEXT, {"type": "text", "text": ""}))
        result.append(self._emit_delta("text_delta", {"text": text}))
        return "".join(result)
    
    def _process_function_call(self, fc: dict, signature: str) -> str:
        result = []
        self.used_tool = True
        tool_id = fc.get("id") or f"{fc.get('name', '')}-{generate_random_id()}"
        tool_use = {"type": "tool_use", "id": tool_id, "name": fc.get("name", ""), "input": {}}
        if signature:
            tool_use["signature"] = signature
        
        result.append(self._start_block(self.BLOCK_FUNCTION, tool_use))
        if fc.get("args"):
            result.append(self._emit_delta("input_json_delta", {"partial_json": json.dumps(fc["args"])}))
        result.append(self._end_block())
        return "".join(result)
    
    def _start_block(self, block_type: int, content_block: dict) -> str:
        result = []
        if self.block_type != self.BLOCK_NONE:
            result.append(self._end_block())
        result.append(self._format_sse("content_block_start", {"type": "content_block_start", "index": self.block_index, "content_block": content_block}))
        self.block_type = block_type
        return "".join(result)
    
    def _end_block(self) -> str:
        if self.block_type == self.BLOCK_NONE:
            return ""
        result = []
        if self.block_type == self.BLOCK_THINKING and self.pending_signature:
            result.append(self._emit_delta("signature_delta", {"signature": self.pending_signature}))
            self.pending_signature = ""
        result.append(self._format_sse("content_block_stop", {"type": "content_block_stop", "index": self.block_index}))
        self.block_index += 1
        self.block_type = self.BLOCK_NONE
        return "".join(result)
    
    def _emit_delta(self, delta_type: str, delta_content: dict) -> str:
        return self._format_sse("content_block_delta", {"type": "content_block_delta", "index": self.block_index, "delta": {"type": delta_type, **delta_content}})
    
    def _emit_empty_thinking(self, signature: str) -> str:
        result = []
        result.append(self._start_block(self.BLOCK_THINKING, {"type": "thinking", "thinking": ""}))
        result.append(self._emit_delta("thinking_delta", {"thinking": ""}))
        result.append(self._emit_delta("signature_delta", {"signature": signature}))
        result.append(self._end_block())
        return "".join(result)
    
    def _emit_finish(self, finish_reason: str) -> str:
        result = []
        result.append(self._end_block())
        
        if self.trailing_signature:
            result.append(self._emit_empty_thinking(self.trailing_signature))
            self.trailing_signature = ""
        
        stop_reason = "tool_use" if self.used_tool else ("max_tokens" if finish_reason == "MAX_TOKENS" else "end_turn")
        usage = {"input_tokens": self.input_tokens, "output_tokens": self.output_tokens}
        if self.cache_read_tokens:
            usage["cache_read_input_tokens"] = self.cache_read_tokens
        
        result.append(self._format_sse("message_delta", {"type": "message_delta", "delta": {"stop_reason": stop_reason, "stop_sequence": None}, "usage": usage}))
        
        if not self.message_stop_sent:
            result.append(self._format_sse("message_stop", {"type": "message_stop"}))
            self.message_stop_sent = True
        
        return "".join(result)
    
    def _format_sse(self, event_type: str, data: dict) -> str:
        return f"event: {event_type}\ndata: {json.dumps(data)}\n\n"


# ============ OpenAI Streaming Processor ============

class OpenAIStreamingProcessor:
    """Process streaming for OpenAI format output.
    
    Key behavior (matching Antigravity-Manager):
    - Thinking content is sent via delta.reasoning_content field
    - Regular content is sent via delta.content field
    - thoughtSignature is stored globally for later use in tool calls
    """
    
    def __init__(self, original_model: str):
        self.original_model = original_model
        self.chunk_id = f"chatcmpl-{generate_random_id()}"
        self.created_ts = int(time.time())
        self.started = False
        self.tool_calls = []
        self.current_tool_index = -1
        self.finished = False
        self.in_thinking_block = False
    
    def _format_chunk(self, delta: dict, finish_reason: Optional[str] = None) -> str:
        """Format a single OpenAI streaming chunk."""
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
    
    def process_claude_event(self, event_type: str, data: dict) -> str:
        """Process Claude SSE event and return OpenAI format."""
        result = []
        
        if event_type == "message_start":
            if not self.started:
                self.started = True
                result.append(self._format_chunk({"role": "assistant", "content": ""}))
        
        elif event_type == "content_block_start":
            block = data.get("content_block", {})
            block_type = block.get("type", "")
            debug_print(f"[OpenAI Processor] content_block_start: block_type={block_type}")
            if block_type == "tool_use":
                self.current_tool_index += 1
                self.tool_calls.append({
                    "index": self.current_tool_index,
                    "id": block.get("id", ""),
                    "type": "function",
                    "function": {"name": block.get("name", ""), "arguments": ""}
                })
                result.append(self._format_chunk({
                    "tool_calls": [{
                        "index": self.current_tool_index,
                        "id": block.get("id", ""),
                        "type": "function",
                        "function": {"name": block.get("name", ""), "arguments": ""}
                    }]
                }))
            elif block_type == "thinking":
                self.in_thinking_block = True
        
        elif event_type == "content_block_stop":
            if self.in_thinking_block:
                self.in_thinking_block = False
        
        elif event_type == "content_block_delta":
            delta = data.get("delta", {})
            delta_type = delta.get("type", "")
            debug_print(f"[OpenAI Processor] content_block_delta: delta_type={delta_type}")
            
            if delta_type == "text_delta":
                text = delta.get("text", "")
                if text:
                    result.append(self._format_chunk({"content": text}))
            
            elif delta_type == "thinking_delta":
                # Send thinking content via reasoning_content field (like Antigravity-Manager)
                thinking = delta.get("thinking", "")
                if thinking:
                    result.append(self._format_chunk({
                        "role": "assistant",
                        "content": None,
                        "reasoning_content": thinking
                    }))
            
            elif delta_type == "signature_delta":
                # Store signature globally for later use in tool calls
                sig = delta.get("signature", "")
                if sig:
                    global_thought_signature_store(sig)
            
            elif delta_type == "input_json_delta":
                partial = delta.get("partial_json", "")
                if partial and self.current_tool_index >= 0:
                    result.append(self._format_chunk({
                        "tool_calls": [{"index": self.current_tool_index, "function": {"arguments": partial}}]
                    }))
        
        elif event_type == "message_delta":
            delta = data.get("delta", {})
            stop_reason = delta.get("stop_reason", "")
            finish_reason = "stop"
            if stop_reason == "tool_use":
                finish_reason = "tool_calls"
            elif stop_reason == "max_tokens":
                finish_reason = "length"
            
            if not self.finished:
                result.append(self._format_chunk({}, finish_reason))
                self.finished = True
        
        elif event_type == "message_stop":
            if not self.finished:
                result.append(self._format_chunk({}, "stop"))
                self.finished = True
            result.append("data: [DONE]\n\n")
        
        return "".join(result)
    
    def finish(self) -> str:
        if not self.finished:
            result = self._format_chunk({}, "stop")
            return result + "data: [DONE]\n\n"
        return "data: [DONE]\n\n"


# ============ OpenAI Responses API Converter ============

class ResponsesAPIConverter:
    """Convert between OpenAI Responses API format and Claude format.
    
    OpenAI Responses API format (v1/responses):
    - input: [{type: "message", role: "user", content: [{type: "input_text", text: "..."}]}]
    - output: [{type: "message", role: "assistant", content: [{type: "output_text", text: "..."}]}]
    - Streaming events: response.created, response.output_text.delta, response.function_call_arguments.delta, etc.
    """
    
    @staticmethod
    def responses_to_claude(responses_req: dict) -> dict:
        """Convert OpenAI Responses API request to Claude format.
        
        Responses API input format:
        {
            "model": "gpt-5",
            "input": [
                {
                    "type": "message",
                    "role": "user",
                    "content": [{"type": "input_text", "text": "Hello"}]
                }
            ],
            "tools": [...],
            "max_output_tokens": 4096
        }
        """
        messages = []
        system_content = None
        
        # Handle input - can be string or array of items
        input_data = responses_req.get("input", [])
        
        if isinstance(input_data, str):
            # Simple string input
            messages.append({"role": "user", "content": input_data})
        elif isinstance(input_data, list):
            for item in input_data:
                item_type = item.get("type", "")
                
                if item_type == "message":
                    role = item.get("role", "user")
                    content = item.get("content", [])
                    
                    # Handle system messages
                    if role == "system":
                        texts = []
                        if isinstance(content, str):
                            texts.append(content)
                        elif isinstance(content, list):
                            for c in content:
                                if c.get("type") == "input_text":
                                    texts.append(c.get("text", ""))
                        if texts:
                            system_content = "\n".join(texts)
                        continue
                    
                    # Convert content blocks
                    claude_content = []
                    if isinstance(content, str):
                        claude_content.append({"type": "text", "text": content})
                    elif isinstance(content, list):
                        for c in content:
                            c_type = c.get("type", "")
                            if c_type == "input_text":
                                text = c.get("text", "")
                                if text:
                                    claude_content.append({"type": "text", "text": text})
                            elif c_type == "input_image":
                                # Handle image input
                                image_url = c.get("image_url", "")
                                if image_url.startswith("data:"):
                                    parts = image_url.split(",", 1)
                                    if len(parts) == 2:
                                        media_type = parts[0].split(";")[0].replace("data:", "")
                                        claude_content.append({
                                            "type": "image",
                                            "source": {
                                                "type": "base64",
                                                "media_type": media_type,
                                                "data": parts[1]
                                            }
                                        })
                            elif c_type == "output_text":
                                # Assistant output from history
                                text = c.get("text", "")
                                if text:
                                    claude_content.append({"type": "text", "text": text})
                            elif c_type == "tool_result":
                                # Tool result
                                claude_content.append({
                                    "type": "tool_result",
                                    "tool_use_id": c.get("call_id", ""),
                                    "content": c.get("output_text", "") or json.dumps(c.get("output", {}))
                                })
                    
                    if claude_content:
                        # Map role
                        if role == "assistant":
                            messages.append({"role": "assistant", "content": claude_content})
                        else:
                            messages.append({"role": "user", "content": claude_content})
                
                elif item_type == "function_call":
                    # Function call from history (assistant made this call)
                    messages.append({
                        "role": "assistant",
                        "content": [{
                            "type": "tool_use",
                            "id": item.get("call_id", item.get("id", "")),
                            "name": item.get("name", ""),
                            "input": json.loads(item.get("arguments", "{}")) if isinstance(item.get("arguments"), str) else item.get("arguments", {})
                        }]
                    })
                
                elif item_type == "function_call_output":
                    # Function call output (tool result)
                    messages.append({
                        "role": "user",
                        "content": [{
                            "type": "tool_result",
                            "tool_use_id": item.get("call_id", ""),
                            "content": item.get("output", "")
                        }]
                    })
                
                elif item_type == "reasoning":
                    # Reasoning/thinking from history - skip or convert to text
                    pass
        
        # Build Claude request
        claude_req = {
            "model": responses_req.get("model", "claude-sonnet-4-5"),
            "messages": messages,
            "max_tokens": responses_req.get("max_output_tokens", 4096),
            "stream": responses_req.get("stream", False),
        }
        
        if system_content:
            claude_req["system"] = system_content
        
        # Handle instructions (system prompt)
        instructions = responses_req.get("instructions")
        if instructions:
            if system_content:
                claude_req["system"] = instructions + "\n\n" + system_content
            else:
                claude_req["system"] = instructions
        
        # Convert tools
        tools = responses_req.get("tools", [])
        if tools:
            claude_tools = []
            for tool in tools:
                tool_type = tool.get("type", "")
                if tool_type == "function":
                    claude_tools.append({
                        "name": tool.get("name", ""),
                        "description": tool.get("description", ""),
                        "input_schema": clean_json_schema(tool.get("parameters", {}))
                    })
                elif tool_type == "web_search":
                    claude_tools.append({
                        "name": "web_search",
                        "description": "Search the web",
                        "input_schema": {"type": "object", "properties": {}}
                    })
            if claude_tools:
                claude_req["tools"] = claude_tools
        
        # Temperature
        if responses_req.get("temperature") is not None:
            claude_req["temperature"] = responses_req["temperature"]
        
        return claude_req
    
    @staticmethod
    def claude_to_responses_response(claude_resp: dict) -> dict:
        """Convert Claude response to OpenAI Responses API format.
        
        Responses API output format:
        {
            "id": "resp_xxx",
            "object": "response",
            "status": "completed",
            "output": [
                {"type": "reasoning", "summary": [...]},
                {"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "..."}]}
            ],
            "usage": {...}
        }
        """
        output = []
        
        # Process content blocks
        message_content = []
        for block in claude_resp.get("content", []):
            block_type = block.get("type", "")
            
            if block_type == "thinking":
                # Add reasoning output item
                thinking_text = block.get("thinking", "")
                if thinking_text:
                    output.append({
                        "id": f"rs_{generate_random_id()}",
                        "type": "reasoning",
                        "summary": [{"type": "summary_text", "text": thinking_text[:500] + "..." if len(thinking_text) > 500 else thinking_text}]
                    })
            
            elif block_type == "text":
                text = block.get("text", "")
                if text:
                    message_content.append({
                        "type": "output_text",
                        "text": text,
                        "annotations": []
                    })
            
            elif block_type == "tool_use":
                # Add function_call output item
                output.append({
                    "id": block.get("id", f"call_{generate_random_id()}"),
                    "type": "function_call",
                    "name": block.get("name", ""),
                    "arguments": json.dumps(block.get("input", {})),
                    "call_id": block.get("id", ""),
                    "status": "completed"
                })
        
        # Add message output item if there's content
        if message_content:
            output.append({
                "id": f"msg_{generate_random_id()}",
                "type": "message",
                "role": "assistant",
                "status": "completed",
                "content": message_content
            })
        
        # Determine status
        stop_reason = claude_resp.get("stop_reason", "end_turn")
        status = "completed"
        if stop_reason == "max_tokens":
            status = "incomplete"
        
        # Build usage
        usage = claude_resp.get("usage", {})
        
        return {
            "id": f"resp_{claude_resp.get('id', generate_random_id())}",
            "object": "response",
            "created_at": int(time.time()),
            "status": status,
            "model": claude_resp.get("model", ""),
            "output": output,
            "usage": {
                "input_tokens": usage.get("input_tokens", 0),
                "output_tokens": usage.get("output_tokens", 0),
                "total_tokens": usage.get("input_tokens", 0) + usage.get("output_tokens", 0)
            }
        }


# ============ OpenAI Responses API Streaming Processor ============

class ResponsesStreamingProcessor:
    """Process streaming for OpenAI Responses API format.
    
    Directly processes Gemini v1internal SSE stream to Responses API events.
    
    Event types:
    - response.created / response.in_progress / response.completed
    - response.output_item.added / response.output_item.done
    - response.content_part.added / response.content_part.done
    - response.output_text.delta / response.output_text.done
    - response.function_call_arguments.delta / response.function_call_arguments.done
    """
    
    def __init__(self, original_model: str):
        self.original_model = original_model
        self.response_id = f"resp_{generate_random_id()}"
        self.created_at = int(time.time())
        self.started = False
        self.finished = False
        self.sequence_number = 0
        
        # Output tracking
        self.output_items = []
        self.current_output_index = -1
        self.current_content_index = -1
        
        # State tracking
        self.in_reasoning = False
        self.in_message = False
        self.in_function_call = False
        self.current_text = ""
        self.current_reasoning = ""
        self.current_function_name = ""
        self.current_function_args = ""
        self.current_function_id = ""
        
        # Usage
        self.input_tokens = 0
        self.output_tokens = 0
        
        debug_print(f"[ResponsesStream] Initialized processor for model={original_model}")
    
    def _next_seq(self) -> int:
        self.sequence_number += 1
        return self.sequence_number
    
    def _format_event(self, event_type: str, data: dict) -> str:
        """Format a Responses API SSE event."""
        data["sequence_number"] = self._next_seq()
        return f"event: {event_type}\ndata: {json.dumps(data)}\n\n"
    
    def _emit_response_created(self) -> str:
        """Emit response.created event."""
        return self._format_event("response.created", {
            "type": "response.created",
            "response": {
                "id": self.response_id,
                "object": "response",
                "created_at": self.created_at,
                "status": "in_progress",
                "model": self.original_model,
                "output": [],
                "usage": None
            }
        })
    
    def _emit_output_item_added(self, item: dict) -> str:
        """Emit response.output_item.added event."""
        self.current_output_index += 1
        return self._format_event("response.output_item.added", {
            "type": "response.output_item.added",
            "output_index": self.current_output_index,
            "item": item
        })
    
    def _emit_content_part_added(self, part: dict) -> str:
        """Emit response.content_part.added event."""
        self.current_content_index += 1
        return self._format_event("response.content_part.added", {
            "type": "response.content_part.added",
            "item_id": f"msg_{self.current_output_index}",
            "output_index": self.current_output_index,
            "content_index": self.current_content_index,
            "part": part
        })
    
    def _emit_text_delta(self, delta: str) -> str:
        """Emit response.output_text.delta event."""
        return self._format_event("response.output_text.delta", {
            "type": "response.output_text.delta",
            "item_id": f"msg_{self.current_output_index}",
            "output_index": self.current_output_index,
            "content_index": self.current_content_index,
            "delta": delta
        })
    
    def _emit_function_call_args_delta(self, delta: str) -> str:
        """Emit response.function_call_arguments.delta event."""
        return self._format_event("response.function_call_arguments.delta", {
            "type": "response.function_call_arguments.delta",
            "item_id": self.current_function_id,
            "output_index": self.current_output_index,
            "call_id": self.current_function_id,
            "delta": delta
        })
    
    def process_line(self, line: str) -> str:
        """Process a single SSE line from Gemini v1internal response."""
        line = line.strip()
        if not line or not line.startswith("data:"):
            return ""
        
        data = line[5:].strip()
        if not data or data == "[DONE]":
            return ""
        
        try:
            v1_resp = json.loads(data)
        except json.JSONDecodeError:
            return ""
        
        gemini_resp = v1_resp.get("response", v1_resp)
        result = []
        
        # Emit response.created on first data
        if not self.started:
            self.started = True
            result.append(self._emit_response_created())
        
        # Update usage
        usage_meta = gemini_resp.get("usageMetadata", {})
        if usage_meta:
            cached = usage_meta.get("cachedContentTokenCount", 0)
            self.input_tokens = usage_meta.get("promptTokenCount", 0) - cached
            self.output_tokens = usage_meta.get("candidatesTokenCount", 0)
        
        # Process candidates
        candidates = gemini_resp.get("candidates", [])
        if candidates and candidates[0].get("content"):
            for part in candidates[0]["content"].get("parts", []):
                result.append(self._process_part(part))
        
        # Check for finish
        if candidates:
            finish_reason = candidates[0].get("finishReason", "")
            if finish_reason and not self.finished:
                result.append(self._emit_finish(finish_reason))
        
        return "".join(result)
    
    def _process_part(self, part: dict) -> str:
        """Process a single part from Gemini response."""
        result = []
        
        is_thought = part.get("thought", False)
        text = part.get("text", "")
        signature = part.get("thoughtSignature", "")
        
        # Store signature globally
        if signature:
            global_thought_signature_store(signature)
        
        # Handle thinking/reasoning content
        if is_thought:
            if not self.in_reasoning:
                # Start new reasoning output item
                self.in_reasoning = True
                self.current_reasoning = ""
                item = {
                    "id": f"rs_{generate_random_id()}",
                    "type": "reasoning",
                    "status": "in_progress",
                    "summary": []
                }
                result.append(self._emit_output_item_added(item))
            
            if text:
                self.current_reasoning += text
                # For reasoning, we don't stream deltas, just accumulate
            return "".join(result)
        
        # Handle function call
        if part.get("functionCall"):
            fc = part["functionCall"]
            self.current_function_id = fc.get("id") or f"call_{generate_random_id()}"
            self.current_function_name = fc.get("name", "")
            self.current_function_args = json.dumps(fc.get("args", {}))
            
            # Close any open reasoning
            if self.in_reasoning:
                self.in_reasoning = False
                result.append(self._format_event("response.output_item.done", {
                    "type": "response.output_item.done",
                    "output_index": self.current_output_index,
                    "item": {
                        "id": f"rs_{self.current_output_index}",
                        "type": "reasoning",
                        "status": "completed",
                        "summary": [{"type": "summary_text", "text": self.current_reasoning[:500]}] if self.current_reasoning else []
                    }
                }))
            
            # Close any open message
            if self.in_message:
                self.in_message = False
                result.append(self._format_event("response.content_part.done", {
                    "type": "response.content_part.done",
                    "item_id": f"msg_{self.current_output_index}",
                    "output_index": self.current_output_index,
                    "content_index": self.current_content_index,
                    "part": {"type": "output_text", "text": self.current_text, "annotations": []}
                }))
                result.append(self._format_event("response.output_item.done", {
                    "type": "response.output_item.done",
                    "output_index": self.current_output_index,
                    "item": {
                        "id": f"msg_{self.current_output_index}",
                        "type": "message",
                        "role": "assistant",
                        "status": "completed",
                        "content": [{"type": "output_text", "text": self.current_text, "annotations": []}]
                    }
                }))
            
            # Emit function_call output item
            item = {
                "id": self.current_function_id,
                "type": "function_call",
                "name": self.current_function_name,
                "call_id": self.current_function_id,
                "arguments": "",
                "status": "in_progress"
            }
            result.append(self._emit_output_item_added(item))
            
            # Emit arguments delta
            result.append(self._emit_function_call_args_delta(self.current_function_args))
            
            # Emit function_call_arguments.done
            result.append(self._format_event("response.function_call_arguments.done", {
                "type": "response.function_call_arguments.done",
                "item_id": self.current_function_id,
                "output_index": self.current_output_index,
                "call_id": self.current_function_id,
                "arguments": self.current_function_args
            }))
            
            # Emit output_item.done for function call
            result.append(self._format_event("response.output_item.done", {
                "type": "response.output_item.done",
                "output_index": self.current_output_index,
                "item": {
                    "id": self.current_function_id,
                    "type": "function_call",
                    "name": self.current_function_name,
                    "call_id": self.current_function_id,
                    "arguments": self.current_function_args,
                    "status": "completed"
                }
            }))
            
            return "".join(result)
        
        # Handle regular text
        if text:
            # Close any open reasoning first
            if self.in_reasoning:
                self.in_reasoning = False
                result.append(self._format_event("response.output_item.done", {
                    "type": "response.output_item.done",
                    "output_index": self.current_output_index,
                    "item": {
                        "id": f"rs_{self.current_output_index}",
                        "type": "reasoning",
                        "status": "completed",
                        "summary": [{"type": "summary_text", "text": self.current_reasoning[:500]}] if self.current_reasoning else []
                    }
                }))
            
            if not self.in_message:
                # Start new message output item
                self.in_message = True
                self.current_text = ""
                self.current_content_index = -1
                
                item = {
                    "id": f"msg_{generate_random_id()}",
                    "type": "message",
                    "role": "assistant",
                    "status": "in_progress",
                    "content": []
                }
                result.append(self._emit_output_item_added(item))
                
                # Add content part
                result.append(self._emit_content_part_added({
                    "type": "output_text",
                    "text": "",
                    "annotations": []
                }))
            
            self.current_text += text
            result.append(self._emit_text_delta(text))
        
        return "".join(result)
    
    def _emit_finish(self, finish_reason: str) -> str:
        """Emit completion events."""
        result = []
        
        # Close any open message
        if self.in_message:
            self.in_message = False
            result.append(self._format_event("response.output_text.done", {
                "type": "response.output_text.done",
                "item_id": f"msg_{self.current_output_index}",
                "output_index": self.current_output_index,
                "content_index": self.current_content_index,
                "text": self.current_text
            }))
            result.append(self._format_event("response.content_part.done", {
                "type": "response.content_part.done",
                "item_id": f"msg_{self.current_output_index}",
                "output_index": self.current_output_index,
                "content_index": self.current_content_index,
                "part": {"type": "output_text", "text": self.current_text, "annotations": []}
            }))
            result.append(self._format_event("response.output_item.done", {
                "type": "response.output_item.done",
                "output_index": self.current_output_index,
                "item": {
                    "id": f"msg_{self.current_output_index}",
                    "type": "message",
                    "role": "assistant",
                    "status": "completed",
                    "content": [{"type": "output_text", "text": self.current_text, "annotations": []}]
                }
            }))
        
        # Close any open reasoning
        if self.in_reasoning:
            self.in_reasoning = False
            result.append(self._format_event("response.output_item.done", {
                "type": "response.output_item.done",
                "output_index": self.current_output_index,
                "item": {
                    "id": f"rs_{self.current_output_index}",
                    "type": "reasoning",
                    "status": "completed",
                    "summary": [{"type": "summary_text", "text": self.current_reasoning[:500]}] if self.current_reasoning else []
                }
            }))
        
        # Determine status
        status = "completed"
        if finish_reason == "MAX_TOKENS":
            status = "incomplete"
        
        # Emit response.completed
        result.append(self._format_event("response.completed", {
            "type": "response.completed",
            "response": {
                "id": self.response_id,
                "object": "response",
                "created_at": self.created_at,
                "status": status,
                "model": self.original_model,
                "output": [],  # Full output would be here in real impl
                "usage": {
                    "input_tokens": self.input_tokens,
                    "output_tokens": self.output_tokens,
                    "total_tokens": self.input_tokens + self.output_tokens
                }
            }
        }))
        
        self.finished = True
        return "".join(result)
    
    def finish(self) -> str:
        """Finish processing and return final events."""
        if not self.finished:
            return self._emit_finish("STOP")
        return ""


# ============ Cursor Streaming Processor ============

class CursorStreamingProcessor:
    """Process streaming for Cursor format output.
    
    Directly processes Gemini v1internal SSE stream to OpenAI format.
    Optimized for Cursor tool_calls handling.
    """
    
    def __init__(self, original_model: str):
        self.original_model = original_model
        self.chunk_id = f"chatcmpl-{generate_random_id()}"
        self.created_ts = int(time.time())
        self.started = False
        self.tool_calls = []
        self.current_tool_index = -1
        self.finished = False
        self.in_thinking = False
        self.response_id = ""
        self.text_count = 0
        self.thinking_count = 0
        self.function_count = 0
        debug_print(f"[CursorStream] Initialized processor for model={original_model}")
    
    def _format_chunk(self, delta: dict, finish_reason: Optional[str] = None) -> str:
        """Format a single OpenAI streaming chunk."""
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
    
    def process_line(self, line: str) -> str:
        """Process a single SSE line from Gemini v1internal response."""
        line = line.strip()
        if not line or not line.startswith("data:"):
            return ""
        
        data = line[5:].strip()
        if not data or data == "[DONE]":
            return ""
        
        try:
            v1_resp = json.loads(data)
        except json.JSONDecodeError:
            debug_print(f"[CursorStream] JSON decode error: {data[:100]}...")
            return ""
        
        gemini_resp = v1_resp.get("response", v1_resp)
        self.response_id = v1_resp.get("responseId") or gemini_resp.get("responseId") or self.response_id
        
        result = []
        
        # Send initial message start
        if not self.started:
            self.started = True
            debug_print(f"[CursorStream] Starting stream, response_id={self.response_id}")
            result.append(self._format_chunk({"role": "assistant", "content": ""}))
        
        # Process candidates
        candidates = gemini_resp.get("candidates", [])
        if candidates and candidates[0].get("content"):
            for part in candidates[0]["content"].get("parts", []):
                result.append(self._process_part(part))
        
        # Check for finish
        if candidates:
            finish_reason = candidates[0].get("finishReason", "")
            if finish_reason and not self.finished:
                openai_finish = "stop"
                if self.tool_calls:
                    openai_finish = "tool_calls"
                elif finish_reason == "MAX_TOKENS":
                    openai_finish = "length"
                debug_print(f"[CursorStream] Finish: gemini={finish_reason}, openai={openai_finish}")
                result.append(self._format_chunk({}, openai_finish))
                self.finished = True
        
        return "".join(result)
    
    def _process_part(self, part: dict) -> str:
        """Process a single part from Gemini response."""
        result = []
        
        is_thought = part.get("thought", False)
        text = part.get("text", "")
        signature = part.get("thoughtSignature", "")
        
        # Handle thinking content
        if is_thought:
            self.thinking_count += 1
            if self.thinking_count <= 3:  # Only log first few
                debug_print(f"[CursorStream] Thinking part #{self.thinking_count}: '{text[:50]}...' sig={bool(signature)}")
            if text:
                result.append(self._format_chunk({
                    "role": "assistant",
                    "content": None,
                    "reasoning_content": text
                }))
            if signature:
                debug_print(f"[CursorStream] Storing signature (len={len(signature)})")
                global_thought_signature_store(signature)
            return "".join(result)
        
        # Handle function call (tool_use)
        if part.get("functionCall"):
            fc = part["functionCall"]
            tool_id = fc.get("id") or f"{fc.get('name', '')}-{generate_random_id()}"
            tool_name = fc.get("name", "")
            tool_args = fc.get("args", {})
            
            self.function_count += 1
            self.current_tool_index += 1
            self.tool_calls.append({
                "index": self.current_tool_index,
                "id": tool_id,
                "type": "function",
                "function": {"name": tool_name, "arguments": json.dumps(tool_args)}
            })
            
            debug_print(f"[CursorStream] *** FUNCTION CALL #{self.function_count} ***")
            debug_print(f"  name: {tool_name}")
            debug_print(f"  id: {tool_id}")
            debug_print(f"  args: {json.dumps(tool_args)[:200]}...")
            
            # Send tool_call chunk
            result.append(self._format_chunk({
                "tool_calls": [{
                    "index": self.current_tool_index,
                    "id": tool_id,
                    "type": "function",
                    "function": {"name": tool_name, "arguments": json.dumps(tool_args)}
                }]
            }))
            
            # Store signature if present
            if signature:
                global_thought_signature_store(signature)
            
            return "".join(result)
        
        # Handle regular text
        if text:
            self.text_count += 1
            if self.text_count <= 5:  # Only log first few
                debug_print(f"[CursorStream] Text part #{self.text_count}: '{text[:50]}...'")
            result.append(self._format_chunk({"content": text}))
        
        return "".join(result)
    
    def finish(self) -> str:
        """Finish processing and return final events."""
        result = []
        
        debug_print(f"[CursorStream] Stream finished: text={self.text_count}, thinking={self.thinking_count}, functions={self.function_count}, tool_calls={len(self.tool_calls)}")
        
        if not self.finished:
            finish_reason = "tool_calls" if self.tool_calls else "stop"
            debug_print(f"[CursorStream] Final finish_reason={finish_reason}")
            result.append(self._format_chunk({}, finish_reason))
            self.finished = True
        
        result.append("data: [DONE]\n\n")
        return "".join(result)


# ============ Antigravity Client ============

class AntigravityClient:
    """Antigravity API client with OAuth support."""
    
    def __init__(self, refresh_token: str, project_id: str = ""):
        self.refresh_token = refresh_token
        self.project_id = project_id
        self.access_token = ""
        self.expires_at = 0
        self._session: Optional[ClientSession] = None
        self._url_availability = {url: 0 for url in BASE_URLS}
    
    async def _get_session(self) -> ClientSession:
        if self._session is None or self._session.closed:
            self._session = ClientSession(timeout=ClientTimeout(total=120))
        return self._session
    
    async def close(self):
        if self._session and not self._session.closed:
            await self._session.close()
    
    async def refresh_access_token(self) -> str:
        session = await self._get_session()
        data = {
            "client_id": CLIENT_ID,
            "client_secret": CLIENT_SECRET,
            "refresh_token": self.refresh_token,
            "grant_type": "refresh_token",
        }
        async with session.post(TOKEN_URL, data=data) as resp:
            if resp.status != 200:
                text = await resp.text()
                raise Exception(f"Token refresh failed ({resp.status}): {text}")
            result = await resp.json()
            self.access_token = result["access_token"]
            self.expires_at = time.time() + result.get("expires_in", 3600) - 60
            return self.access_token
    
    async def get_access_token(self) -> str:
        if not self.access_token or time.time() >= self.expires_at:
            await self.refresh_access_token()
        return self.access_token
    
    def _get_available_urls(self) -> list:
        now = time.time()
        return [url for url in BASE_URLS if self._url_availability.get(url, 0) <= now]
    
    def _mark_unavailable(self, url: str, ttl: float = 300):
        self._url_availability[url] = time.time() + ttl
    
    async def forward_request(self, gemini_body: dict, stream: bool = False) -> tuple[int, dict, Any]:
        """Forward request to upstream API.
        
        Returns:
            For non-streaming: (status, headers, bytes)
            For streaming: (status, headers, response_object) - caller must handle streaming
        """
        # Apply rate limiting before making request
        rate_limiter = get_rate_limiter()
        await rate_limiter.acquire()
        
        session = await self._get_session()
        access_token = await self.get_access_token()
        
        available_urls = self._get_available_urls() or BASE_URLS
        action = "streamGenerateContent"  # Always use streaming for better quota
        
        last_error = None
        for url_idx, base_url in enumerate(available_urls):
            api_url = f"{base_url}/v1internal:{action}?alt=sse"
            headers = {
                "Content-Type": "application/json",
                "Authorization": f"Bearer {access_token}",
                "User-Agent": USER_AGENT,
                "Accept": "text/event-stream",
            }
            
            try:
                resp = await session.post(api_url, json=gemini_body, headers=headers)
                
                if resp.status == 429:
                    # Rate limited - wait and retry
                    debug_print(f"[RateLimiter] Got 429, waiting before retry...")
                    await asyncio.sleep(5)
                    if url_idx < len(available_urls) - 1:
                        self._mark_unavailable(base_url)
                        await resp.release()
                        continue
                
                # Return response object - caller handles streaming or reading
                return resp.status, dict(resp.headers), resp
                
            except Exception as e:
                last_error = e
                if url_idx < len(available_urls) - 1:
                    self._mark_unavailable(base_url)
                    continue
                raise
        
        raise last_error or Exception("All URLs failed")
    
    async def forward_request_streaming(self, gemini_body: dict):
        """Forward request and yield SSE lines as they arrive.
        
        Yields:
            (status, headers) on first yield
            Then yields each SSE line as it arrives
        """
        rate_limiter = get_rate_limiter()
        await rate_limiter.acquire()
        
        session = await self._get_session()
        access_token = await self.get_access_token()
        
        available_urls = self._get_available_urls() or BASE_URLS
        action = "streamGenerateContent"
        
        last_error = None
        for url_idx, base_url in enumerate(available_urls):
            api_url = f"{base_url}/v1internal:{action}?alt=sse"
            headers = {
                "Content-Type": "application/json",
                "Authorization": f"Bearer {access_token}",
                "User-Agent": USER_AGENT,
                "Accept": "text/event-stream",
            }
            
            try:
                async with session.post(api_url, json=gemini_body, headers=headers) as resp:
                    if resp.status == 429:
                        debug_print(f"[RateLimiter] Got 429, waiting before retry...")
                        await asyncio.sleep(5)
                        if url_idx < len(available_urls) - 1:
                            self._mark_unavailable(base_url)
                            continue
                    
                    # Yield status and headers first
                    yield resp.status, dict(resp.headers)
                    
                    if resp.status >= 400:
                        # For errors, yield the error body
                        error_body = await resp.text()
                        yield error_body
                        return
                    
                    # Stream the response line by line
                    buffer = ""
                    async for chunk in resp.content.iter_any():
                        if chunk:
                            buffer += chunk.decode("utf-8", errors="ignore")
                            while "\n" in buffer:
                                line, buffer = buffer.split("\n", 1)
                                line = line.strip()
                                if line:
                                    yield line
                    
                    # Yield any remaining buffer
                    if buffer.strip():
                        yield buffer.strip()
                    
                    return
                    
            except Exception as e:
                last_error = e
                if url_idx < len(available_urls) - 1:
                    self._mark_unavailable(base_url)
                    continue
                raise
        
        raise last_error or Exception("All URLs failed")
    
    async def load_code_assist(self) -> dict:
        session = await self._get_session()
        access_token = await self.get_access_token()
        
        available_urls = self._get_available_urls() or BASE_URLS
        body = {"metadata": {"ideType": "ANTIGRAVITY"}}
        
        for url_idx, base_url in enumerate(available_urls):
            api_url = f"{base_url}/v1internal:loadCodeAssist"
            headers = {
                "Content-Type": "application/json",
                "Authorization": f"Bearer {access_token}",
                "User-Agent": USER_AGENT,
            }
            
            try:
                async with session.post(api_url, json=body, headers=headers) as resp:
                    if resp.status == 429 and url_idx < len(available_urls) - 1:
                        self._mark_unavailable(base_url)
                        continue
                    if resp.status != 200:
                        text = await resp.text()
                        raise Exception(f"loadCodeAssist failed ({resp.status}): {text}")
                    result = await resp.json()
                    if result.get("cloudaicompanionProject"):
                        self.project_id = result["cloudaicompanionProject"]
                    return result
            except Exception as e:
                if url_idx < len(available_urls) - 1:
                    self._mark_unavailable(base_url)
                    continue
                raise


# ============ HTTP Server ============

class AntigravityProxy:
    """HTTP proxy server for Antigravity API."""
    
    def __init__(self, config: Config):
        self.config = config
        self.client = AntigravityClient(config.refresh_token, config.project_id)
        self.transformer = RequestTransformer()
    
    def _check_auth(self, request: web.Request) -> Optional[web.Response]:
        """Check API key authentication.
        
        Supports multiple auth methods:
        - Authorization: Bearer <key>
        - x-api-key: <key>
        - x-goog-api-key: <key> (Gemini/Google style)
        """
        auth = request.headers.get("Authorization", "")
        api_key = request.headers.get("x-api-key", "")
        goog_api_key = request.headers.get("x-goog-api-key", "")
        
        if auth.startswith("Bearer "):
            api_key = auth[7:]
        
        # Also accept x-goog-api-key (used by Gemini clients like Cherry Studio)
        if not api_key and goog_api_key:
            api_key = goog_api_key
        
        if api_key != self.config.api_key:
            return web.json_response(
                {"error": {"type": "authentication_error", "message": "Invalid API key"}},
                status=401
            )
        return None
    
    async def _ensure_project(self) -> Optional[web.Response]:
        """Ensure project_id is loaded."""
        if not self.client.project_id:
            try:
                await self.client.load_code_assist()
            except Exception as e:
                return web.json_response(
                    {"error": {"type": "api_error", "message": f"Failed to load project: {e}"}},
                    status=500
                )
        return None
    
    # ============ Anthropic Claude API ============
    
    async def handle_claude_messages(self, request: web.Request) -> web.StreamResponse:
        """Handle /v1/messages endpoint (Anthropic Claude API)."""
        auth_error = self._check_auth(request)
        if auth_error:
            return auth_error
        
        try:
            body = await request.json()
        except json.JSONDecodeError:
            return web.json_response({"error": {"type": "invalid_request", "message": "Invalid JSON"}}, status=400)
        
        project_error = await self._ensure_project()
        if project_error:
            return project_error
        
        original_model = body.get("model", "")
        mapped_model = get_mapped_model(original_model)
        is_stream = body.get("stream", False)
        
        # Smart thinking injection (matching Antigravity-Manager logic)
        existing_thinking = body.get("thinking", {})
        thinking_already_enabled = existing_thinking.get("type") == "enabled"
        target_supports_thinking = model_supports_thinking(mapped_model)
        
        if self.config.enable_thinking and not thinking_already_enabled:
            if target_supports_thinking:
                body["thinking"] = {
                    "type": "enabled",
                    "budget_tokens": self.config.thinking_budget
                }
                debug_print(f"[Claude Messages] Thinking enabled, budget={self.config.thinking_budget}")
            else:
                debug_print(f"[Claude Messages] Thinking NOT enabled: model '{mapped_model}' does not support thinking")
        elif thinking_already_enabled and not target_supports_thinking:
            debug_print(f"[Claude Messages] WARNING: Thinking requested but model '{mapped_model}' does not support it. Disabling.")
            del body["thinking"]
        
        gemini_body = self.transformer.transform(body, self.client.project_id, mapped_model)
        
        try:
            status, headers, resp = await self.client.forward_request(gemini_body, stream=True)
            
            if status >= 400:
                error_text = await resp.text()
                await resp.release()
                return web.json_response(
                    {"error": {"type": "api_error", "message": f"Upstream error ({status}): {error_text[:500]}"}},
                    status=status
                )
            
            if is_stream:
                return await self._handle_claude_streaming_real(request, resp, original_model)
            else:
                return await self._handle_claude_non_streaming_real(resp, original_model)
                
        except Exception as e:
            import traceback
            traceback.print_exc()
            return web.json_response({"error": {"type": "api_error", "message": str(e)}}, status=500)
    
    async def _handle_claude_streaming_real(self, request: web.Request, resp, original_model: str) -> web.StreamResponse:
        """Handle real-time streaming for Claude format."""
        response = web.StreamResponse(status=200, headers={
            "Content-Type": "text/event-stream",
            "Cache-Control": "no-cache",
            "Connection": "keep-alive",
            "X-Accel-Buffering": "no",
        })
        await response.prepare(request)
        
        processor = StreamingProcessor(original_model)
        client_disconnected = False
        
        try:
            async for chunk in resp.content.iter_any():
                if client_disconnected:
                    break
                if chunk:
                    text = chunk.decode("utf-8", errors="ignore")
                    for line in text.split("\n"):
                        events = processor.process_line(line)
                        if events:
                            if not await self._safe_write(response, events.encode("utf-8")):
                                client_disconnected = True
                                break
            
            if not client_disconnected:
                final_events, _ = processor.finish()
                if final_events:
                    await self._safe_write(response, final_events.encode("utf-8"))
        finally:
            await resp.release()
        
        if not client_disconnected:
            try:
                await response.write_eof()
            except Exception:
                pass
        return response
    
    async def _handle_claude_non_streaming_real(self, resp, original_model: str) -> web.Response:
        """Handle non-streaming response for Claude format."""
        collected_parts = []
        usage_meta = {}
        response_id = f"msg_{generate_random_id()}"
        
        try:
            async for chunk in resp.content.iter_any():
                if chunk:
                    text = chunk.decode("utf-8", errors="ignore")
                    for line in text.split("\n"):
                        line = line.strip()
                        if line.startswith("data:"):
                            data = line[5:].strip()
                            if data and data != "[DONE]":
                                try:
                                    v1_resp = json.loads(data)
                                    gemini_resp = v1_resp.get("response", v1_resp)
                                    response_id = v1_resp.get("responseId") or gemini_resp.get("responseId") or response_id
                                    candidates = gemini_resp.get("candidates", [])
                                    if candidates and candidates[0].get("content"):
                                        collected_parts.extend(candidates[0]["content"].get("parts", []))
                                    if gemini_resp.get("usageMetadata"):
                                        usage_meta = gemini_resp["usageMetadata"]
                                except json.JSONDecodeError:
                                    pass
        finally:
            await resp.release()
        
        gemini_resp = {
            "candidates": [{"content": {"parts": collected_parts}}],
            "usageMetadata": usage_meta,
        }
        
        processor = NonStreamingProcessor()
        claude_resp, _ = processor.process(gemini_resp, response_id, original_model)
        return web.json_response(claude_resp)
    
    # ============ OpenAI API ============
    
    async def handle_openai_chat(self, request: web.Request) -> web.StreamResponse:
        """Handle /v1/chat/completions endpoint (OpenAI API).
        
        Supports real-time streaming for better user experience.
        """
        auth_error = self._check_auth(request)
        if auth_error:
            return auth_error
        
        try:
            body = await request.json()
        except json.JSONDecodeError:
            return web.json_response({"error": {"message": "Invalid JSON", "type": "invalid_request_error"}}, status=400)
        
        project_error = await self._ensure_project()
        if project_error:
            return project_error
        
        original_model = body.get("model", "")
        mapped_model = get_mapped_model(original_model)
        is_stream = body.get("stream", False)
        
        # Convert OpenAI to Claude format
        claude_req = OpenAIConverter.openai_to_claude(body)
        
        # Smart thinking injection (matching Antigravity-Manager logic)
        target_supports_thinking = model_supports_thinking(mapped_model)
        existing_thinking = claude_req.get("thinking", {})
        thinking_already_enabled = existing_thinking.get("type") == "enabled"
        messages = claude_req.get("messages", [])
        history_compatible = not should_disable_thinking_due_to_history(messages)
        
        can_enable_thinking = (
            self.config.enable_thinking and 
            target_supports_thinking and 
            history_compatible
        )
        
        if can_enable_thinking and not thinking_already_enabled:
            claude_req["thinking"] = {
                "type": "enabled",
                "budget_tokens": self.config.thinking_budget
            }
            debug_print(f"[OpenAI Chat] Thinking enabled, budget={self.config.thinking_budget}")
        elif self.config.enable_thinking and not can_enable_thinking:
            reasons = []
            if not target_supports_thinking:
                reasons.append(f"model '{mapped_model}' does not support thinking")
            if not history_compatible:
                reasons.append("history has tool_use without thinking")
            debug_print(f"[OpenAI Chat] Thinking DISABLED: {', '.join(reasons)}")
            if "thinking" in claude_req:
                del claude_req["thinking"]
        
        debug_print(f"[OpenAI Chat] original_model={original_model}, mapped_model={mapped_model}, is_stream={is_stream}")
        
        gemini_body = self.transformer.transform(claude_req, self.client.project_id, mapped_model)
        
        # Debug: log thinkingConfig in request
        gen_config = gemini_body.get("request", {}).get("generationConfig", {})
        thinking_config = gen_config.get("thinkingConfig")
        debug_print(f"[OpenAI Debug] Request thinkingConfig: {thinking_config}")
        debug_print(f"[OpenAI Debug] Model in request: {gemini_body.get('model')}")
        
        try:
            status, headers, resp = await self.client.forward_request(gemini_body, stream=True)
            
            if status >= 400:
                error_text = await resp.text()
                await resp.release()
                return web.json_response(
                    {"error": {"message": f"Upstream error ({status}): {error_text[:500]}", "type": "api_error"}},
                    status=status
                )
            
            if is_stream:
                return await self._handle_openai_streaming_real(request, resp, original_model)
            else:
                return await self._handle_openai_non_streaming_real(resp, original_model)
                
        except Exception as e:
            import traceback
            traceback.print_exc()
            return web.json_response({"error": {"message": str(e), "type": "api_error"}}, status=500)
    
    async def _safe_write(self, response: web.StreamResponse, data: bytes) -> bool:
        """Safely write to response, return False if client disconnected."""
        try:
            await response.write(data)
            return True
        except (ConnectionResetError, BrokenPipeError, ConnectionAbortedError):
            debug_print("[Stream] Client disconnected (connection reset)")
            return False
        except Exception as e:
            # Catch aiohttp specific errors
            error_name = type(e).__name__
            if "ConnectionReset" in error_name or "closing transport" in str(e).lower():
                debug_print(f"[Stream] Client disconnected ({error_name})")
                return False
            raise
    
    async def _handle_openai_streaming_real(self, request: web.Request, resp, original_model: str) -> web.StreamResponse:
        """Handle real-time streaming for OpenAI format.
        
        Converts Gemini SSE stream to OpenAI SSE format in real-time.
        """
        response = web.StreamResponse(status=200, headers={
            "Content-Type": "text/event-stream",
            "Cache-Control": "no-cache",
            "Connection": "keep-alive",
            "X-Accel-Buffering": "no",
        })
        await response.prepare(request)
        
        claude_processor = StreamingProcessor(original_model)
        openai_processor = OpenAIStreamingProcessor(original_model)
        client_disconnected = False
        
        try:
            buffer = ""
            async for chunk in resp.content.iter_any():
                if client_disconnected:
                    break
                if chunk:
                    buffer += chunk.decode("utf-8", errors="ignore")
                    
                    # Process complete lines
                    while "\n" in buffer:
                        line, buffer = buffer.split("\n", 1)
                        line = line.strip()
                        if not line:
                            continue
                        
                        # Process through Claude processor first
                        claude_events = claude_processor.process_line(line)
                        if claude_events:
                            # Parse Claude events and convert to OpenAI
                            for event_line in claude_events.split("\n\n"):
                                if not event_line.strip():
                                    continue
                                lines = event_line.strip().split("\n")
                                event_type = ""
                                event_data = {}
                                for l in lines:
                                    if l.startswith("event:"):
                                        event_type = l[6:].strip()
                                    elif l.startswith("data:"):
                                        try:
                                            event_data = json.loads(l[5:].strip())
                                        except:
                                            pass
                                if event_type:
                                    openai_events = openai_processor.process_claude_event(event_type, event_data)
                                    if openai_events:
                                        if not await self._safe_write(response, openai_events.encode("utf-8")):
                                            client_disconnected = True
                                            break
            
            # Process any remaining buffer (only if client still connected)
            if not client_disconnected and buffer.strip():
                claude_events = claude_processor.process_line(buffer.strip())
                if claude_events:
                    for event_line in claude_events.split("\n\n"):
                        if client_disconnected:
                            break
                        if not event_line.strip():
                            continue
                        lines = event_line.strip().split("\n")
                        event_type = ""
                        event_data = {}
                        for l in lines:
                            if l.startswith("event:"):
                                event_type = l[6:].strip()
                            elif l.startswith("data:"):
                                try:
                                    event_data = json.loads(l[5:].strip())
                                except:
                                    pass
                        if event_type:
                            openai_events = openai_processor.process_claude_event(event_type, event_data)
                            if openai_events:
                                if not await self._safe_write(response, openai_events.encode("utf-8")):
                                    client_disconnected = True
                                    break
            
            # Finish processing (only if client still connected)
            if not client_disconnected:
                final_claude, _ = claude_processor.finish()
                if final_claude:
                    for event_line in final_claude.split("\n\n"):
                        if client_disconnected:
                            break
                        if not event_line.strip():
                            continue
                        lines = event_line.strip().split("\n")
                        event_type = ""
                        event_data = {}
                        for l in lines:
                            if l.startswith("event:"):
                                event_type = l[6:].strip()
                            elif l.startswith("data:"):
                                try:
                                    event_data = json.loads(l[5:].strip())
                                except:
                                    pass
                        if event_type:
                            openai_events = openai_processor.process_claude_event(event_type, event_data)
                            if openai_events:
                                if not await self._safe_write(response, openai_events.encode("utf-8")):
                                    client_disconnected = True
                                    break
            
            if not client_disconnected:
                final_openai = openai_processor.finish()
                if final_openai:
                    await self._safe_write(response, final_openai.encode("utf-8"))
        finally:
            await resp.release()
        
        if not client_disconnected:
            try:
                await response.write_eof()
            except Exception:
                pass  # Client already disconnected
        return response
    
    async def _handle_openai_non_streaming_real(self, resp, original_model: str) -> web.Response:
        """Handle non-streaming response for OpenAI format."""
        collected_parts = []
        usage_meta = {}
        response_id = f"msg_{generate_random_id()}"
        thought_count = 0
        text_count = 0
        
        try:
            async for chunk in resp.content.iter_any():
                if chunk:
                    text = chunk.decode("utf-8", errors="ignore")
                    for line in text.split("\n"):
                        line = line.strip()
                        if line.startswith("data:"):
                            data = line[5:].strip()
                            if data and data != "[DONE]":
                                try:
                                    v1_resp = json.loads(data)
                                    gemini_resp = v1_resp.get("response", v1_resp)
                                    response_id = v1_resp.get("responseId") or gemini_resp.get("responseId") or response_id
                                    candidates = gemini_resp.get("candidates", [])
                                    if candidates and candidates[0].get("content"):
                                        parts = candidates[0]["content"].get("parts", [])
                                        for p in parts:
                                            if p.get("thought"):
                                                thought_count += 1
                                            elif p.get("text"):
                                                text_count += 1
                                        collected_parts.extend(parts)
                                    if gemini_resp.get("usageMetadata"):
                                        usage_meta = gemini_resp["usageMetadata"]
                                except json.JSONDecodeError:
                                    pass
        finally:
            await resp.release()
        
        debug_print(f"[NonStreaming] Collected {len(collected_parts)} parts: {thought_count} thought, {text_count} text")
        
        gemini_resp = {
            "candidates": [{"content": {"parts": collected_parts}}],
            "usageMetadata": usage_meta,
        }
        
        processor = NonStreamingProcessor()
        claude_resp, _ = processor.process(gemini_resp, response_id, original_model)
        
        # Debug: check claude_resp content
        thinking_blocks = [b for b in claude_resp.get("content", []) if b.get("type") == "thinking"]
        text_blocks = [b for b in claude_resp.get("content", []) if b.get("type") == "text"]
        debug_print(f"[NonStreaming] Claude response: {len(thinking_blocks)} thinking blocks, {len(text_blocks)} text blocks")
        
        # Convert to OpenAI format
        openai_resp = OpenAIConverter.claude_to_openai_response(claude_resp)
        return web.json_response(openai_resp)
    
    # ============ Legacy methods (kept for reference, not used) ============
    # The _handle_*_streaming and _handle_*_non_streaming methods below are
    # legacy implementations that buffer the entire response before processing.
    # They have been replaced by _handle_*_streaming_real and _handle_*_non_streaming_real
    # which provide true real-time streaming.
    
    # ============ Legacy Completions API ============
    
    async def handle_completions(self, request: web.Request) -> web.StreamResponse:
        """Handle /v1/completions endpoint (Legacy OpenAI Completions API).
        
        Converts legacy prompt-based requests to chat format.
        """
        auth_error = self._check_auth(request)
        if auth_error:
            return auth_error
        
        try:
            body = await request.json()
        except json.JSONDecodeError:
            return web.json_response({"error": {"message": "Invalid JSON", "type": "invalid_request_error"}}, status=400)
        
        # Convert legacy completions to chat format
        prompt = body.get("prompt", "")
        if isinstance(prompt, list):
            prompt = "\n".join(str(p) for p in prompt)
        
        chat_body = {
            "model": body.get("model", "claude-sonnet-4-5"),
            "messages": [{"role": "user", "content": prompt}],
            "stream": body.get("stream", False),
            "max_tokens": body.get("max_tokens", 4096),
        }
        
        if body.get("temperature") is not None:
            chat_body["temperature"] = body["temperature"]
        if body.get("top_p") is not None:
            chat_body["top_p"] = body["top_p"]
        if body.get("stop") is not None:
            chat_body["stop"] = body["stop"]
        
        # Reuse chat completions handler
        # Create a mock request with the converted body
        request._payload = None
        request._read_bytes = json.dumps(chat_body).encode()
        
        # Process through chat completions
        return await self._handle_completions_internal(request, chat_body)
    
    async def _handle_completions_internal(self, request: web.Request, body: dict) -> web.StreamResponse:
        """Internal handler for completions that works with pre-parsed body."""
        project_error = await self._ensure_project()
        if project_error:
            return project_error
        
        original_model = body.get("model", "")
        mapped_model = get_mapped_model(original_model)
        is_stream = body.get("stream", False)
        
        # Convert to Claude format
        claude_req = OpenAIConverter.openai_to_claude(body)
        
        # Disable thinking for legacy completions (simpler output)
        if "thinking" in claude_req:
            del claude_req["thinking"]
        
        gemini_body = self.transformer.transform(claude_req, self.client.project_id, mapped_model)
        
        try:
            status, headers, resp = await self.client.forward_request(gemini_body, stream=True)
            
            if status >= 400:
                error_text = await resp.text()
                await resp.release()
                return web.json_response(
                    {"error": {"message": f"Upstream error ({status}): {error_text[:500]}", "type": "api_error"}},
                    status=status
                )
            
            if is_stream:
                return await self._handle_completions_streaming(request, resp, original_model)
            else:
                return await self._handle_completions_non_streaming(resp, original_model)
                
        except Exception as e:
            import traceback
            traceback.print_exc()
            return web.json_response({"error": {"message": str(e), "type": "api_error"}}, status=500)
    
    async def _handle_completions_streaming(self, request: web.Request, resp, original_model: str) -> web.StreamResponse:
        """Handle streaming for legacy completions format."""
        response = web.StreamResponse(status=200, headers={
            "Content-Type": "text/event-stream",
            "Cache-Control": "no-cache",
            "Connection": "keep-alive",
        })
        await response.prepare(request)
        
        chunk_id = f"cmpl-{generate_random_id()}"
        created_ts = int(time.time())
        client_disconnected = False
        
        try:
            async for chunk in resp.content.iter_any():
                if client_disconnected:
                    break
                if chunk:
                    text = chunk.decode("utf-8", errors="ignore")
                    for line in text.split("\n"):
                        if client_disconnected:
                            break
                        line = line.strip()
                        if line.startswith("data:"):
                            data = line[5:].strip()
                            if data and data != "[DONE]":
                                try:
                                    v1_resp = json.loads(data)
                                    gemini_resp = v1_resp.get("response", v1_resp)
                                    candidates = gemini_resp.get("candidates", [])
                                    if candidates and candidates[0].get("content"):
                                        for part in candidates[0]["content"].get("parts", []):
                                            text_content = part.get("text", "")
                                            if text_content and not part.get("thought"):
                                                chunk_data = {
                                                    "id": chunk_id,
                                                    "object": "text_completion",
                                                    "created": created_ts,
                                                    "model": original_model,
                                                    "choices": [{
                                                        "text": text_content,
                                                        "index": 0,
                                                        "logprobs": None,
                                                        "finish_reason": None
                                                    }]
                                                }
                                                if not await self._safe_write(response, f"data: {json.dumps(chunk_data)}\n\n".encode()):
                                                    client_disconnected = True
                                                    break
                                except json.JSONDecodeError:
                                    pass
            
            # Send final chunk (only if client still connected)
            if not client_disconnected:
                final_chunk = {
                    "id": chunk_id,
                    "object": "text_completion",
                    "created": created_ts,
                    "model": original_model,
                    "choices": [{
                        "text": "",
                        "index": 0,
                        "logprobs": None,
                        "finish_reason": "stop"
                    }]
                }
                await self._safe_write(response, f"data: {json.dumps(final_chunk)}\n\n".encode())
                await self._safe_write(response, b"data: [DONE]\n\n")
        finally:
            await resp.release()
        
        if not client_disconnected:
            try:
                await response.write_eof()
            except Exception:
                pass
        return response
    
    async def _handle_completions_non_streaming(self, resp, original_model: str) -> web.Response:
        """Handle non-streaming for legacy completions format."""
        collected_text = ""
        usage_meta = {}
        
        try:
            async for chunk in resp.content.iter_any():
                if chunk:
                    text = chunk.decode("utf-8", errors="ignore")
                    for line in text.split("\n"):
                        line = line.strip()
                        if line.startswith("data:"):
                            data = line[5:].strip()
                            if data and data != "[DONE]":
                                try:
                                    v1_resp = json.loads(data)
                                    gemini_resp = v1_resp.get("response", v1_resp)
                                    candidates = gemini_resp.get("candidates", [])
                                    if candidates and candidates[0].get("content"):
                                        for part in candidates[0]["content"].get("parts", []):
                                            text_content = part.get("text", "")
                                            if text_content and not part.get("thought"):
                                                collected_text += text_content
                                    if gemini_resp.get("usageMetadata"):
                                        usage_meta = gemini_resp["usageMetadata"]
                                except json.JSONDecodeError:
                                    pass
        finally:
            await resp.release()
        
        cached = usage_meta.get("cachedContentTokenCount", 0)
        response_data = {
            "id": f"cmpl-{generate_random_id()}",
            "object": "text_completion",
            "created": int(time.time()),
            "model": original_model,
            "choices": [{
                "text": collected_text,
                "index": 0,
                "logprobs": None,
                "finish_reason": "stop"
            }],
            "usage": {
                "prompt_tokens": usage_meta.get("promptTokenCount", 0) - cached,
                "completion_tokens": usage_meta.get("candidatesTokenCount", 0),
                "total_tokens": usage_meta.get("totalTokenCount", 0),
            }
        }
        return web.json_response(response_data)
    
    # ============ Codex Responses API ============
    
    async def handle_responses(self, request: web.Request) -> web.StreamResponse:
        """Handle /v1/responses endpoint (Codex-style API).
        
        This is similar to chat completions but with a different request/response format.
        """
        # For now, redirect to chat completions with format conversion
        return await self.handle_openai_chat(request)
    
    # ============ Cursor API ============
    
    async def handle_cursor_chat(self, request: web.Request) -> web.StreamResponse:
        """Handle /cursor/v1/chat/completions endpoint (Cursor Editor API).
        
        Cursor-specific OpenAI-compatible endpoint with proper tool_calls support.
        Key differences from standard OpenAI endpoint:
        - Optimized for Cursor tool calling patterns
        - Better handling of streaming tool_calls
        - Supports Cursor-specific headers and parameters
        """
        debug_print("\n" + "="*60)
        debug_print("[Cursor] ========== NEW REQUEST ==========")
        
        auth_error = self._check_auth(request)
        if auth_error:
            debug_print("[Cursor] Auth failed")
            return auth_error
        
        try:
            body = await request.json()
        except json.JSONDecodeError:
            debug_print("[Cursor] Invalid JSON in request body")
            return web.json_response({"error": {"message": "Invalid JSON", "type": "invalid_request_error"}}, status=400)
        
        # Detect request format: Anthropic or OpenAI
        anthropic_beta = request.headers.get("Anthropic-Beta", "")
        is_anthropic_format = bool(anthropic_beta) or "content" in str(body.get("messages", [{}])[0].get("content", ""))
        
        # Log request details
        debug_print(f"[Cursor] Request Headers:")
        for key, value in request.headers.items():
            if key.lower() not in ('authorization', 'x-api-key', 'x-goog-api-key'):
                debug_print(f"  {key}: {value}")
        
        debug_print(f"\n[Cursor] Detected format: {'Anthropic' if is_anthropic_format else 'OpenAI'}")
        debug_print(f"\n[Cursor] Request Body:")
        debug_print(f"  model: {body.get('model', 'N/A')}")
        debug_print(f"  stream: {body.get('stream', False)}")
        debug_print(f"  temperature: {body.get('temperature', 'N/A')}")
        debug_print(f"  max_tokens: {body.get('max_tokens', 'N/A')}")
        
        # Log messages summary
        messages = body.get("messages", [])
        debug_print(f"\n[Cursor] Messages ({len(messages)} total):")
        for i, msg in enumerate(messages):
            role = msg.get("role", "unknown")
            content = msg.get("content", "")
            if isinstance(content, str):
                content_preview = content[:100] + "..." if len(content) > 100 else content
            elif isinstance(content, list):
                # Anthropic format: content is a list of blocks
                block_types = [b.get("type", "unknown") for b in content]
                content_preview = f"[{len(content)} blocks: {block_types}]"
            else:
                content_preview = f"[unknown format]"
            tool_calls = msg.get("tool_calls", [])
            tool_call_id = msg.get("tool_call_id", "")
            
            debug_print(f"  [{i}] role={role}", end="")
            if tool_calls:
                debug_print(f", tool_calls={[tc.get('function', {}).get('name', 'unknown') for tc in tool_calls]}", end="")
            if tool_call_id:
                debug_print(f", tool_call_id={tool_call_id}", end="")
            debug_print(f", content='{content_preview}'")
        
        # Log tools - handle both OpenAI and Anthropic formats
        tools = body.get("tools", [])
        if tools:
            debug_print(f"\n[Cursor] Tools ({len(tools)} total):")
            for i, tool in enumerate(tools[:5]):  # Only show first 5
                if tool.get("type") == "function":
                    # OpenAI format
                    func = tool.get("function", {})
                    debug_print(f"  [{i}] (OpenAI) {func.get('name', 'unknown')}")
                elif tool.get("name"):
                    # Anthropic format
                    debug_print(f"  [{i}] (Anthropic) {tool.get('name', 'unknown')}")
                else:
                    debug_print(f"  [{i}] (Unknown) keys={list(tool.keys())}")
            if len(tools) > 5:
                debug_print(f"  ... and {len(tools) - 5} more")
        else:
            debug_print(f"\n[Cursor] Tools: None")
        
        project_error = await self._ensure_project()
        if project_error:
            debug_print("[Cursor] Project error")
            return project_error
        
        original_model = body.get("model", "")
        mapped_model = get_mapped_model(original_model)
        is_stream = body.get("stream", False)
        
        debug_print(f"\n[Cursor] Model mapping: {original_model} -> {mapped_model}")
        
        # Convert to Claude format based on detected format
        if is_anthropic_format:
            # Already in Anthropic/Claude format, use directly
            debug_print(f"[Cursor] Using Anthropic format directly (no conversion needed)")
            claude_req = body.copy()
            # Ensure tools are in correct format and clean schemas
            if tools:
                claude_tools = []
                for tool in tools:
                    if tool.get("name"):
                        # Already Anthropic format - clean the schema
                        input_schema = tool.get("input_schema", {})
                        cleaned_schema = clean_json_schema(input_schema)
                        claude_tools.append({
                            "name": tool.get("name", ""),
                            "description": tool.get("description", ""),
                            "input_schema": cleaned_schema,
                        })
                    elif tool.get("type") == "function":
                        # OpenAI format embedded - clean the schema
                        func = tool.get("function", {})
                        input_schema = func.get("parameters", {})
                        cleaned_schema = clean_json_schema(input_schema)
                        claude_tools.append({
                            "name": func.get("name", ""),
                            "description": func.get("description", ""),
                            "input_schema": cleaned_schema,
                        })
                if claude_tools:
                    claude_req["tools"] = claude_tools
                    debug_print(f"[Cursor] Converted {len(claude_tools)} tools to Claude format (schemas cleaned)")
        else:
            # OpenAI format, need conversion
            debug_print(f"[Cursor] Converting from OpenAI format")
            claude_req = OpenAIConverter.openai_to_claude(body)
        
        # Log converted Claude request
        debug_print(f"\n[Cursor] Claude request:")
        debug_print(f"  messages: {len(claude_req.get('messages', []))} messages")
        debug_print(f"  system: {'yes' if claude_req.get('system') else 'no'}")
        debug_print(f"  tools: {len(claude_req.get('tools', []))} tools")
        if claude_req.get('tools'):
            for i, t in enumerate(claude_req['tools'][:3]):
                debug_print(f"    [{i}] {t.get('name', 'unknown')}")
            if len(claude_req['tools']) > 3:
                debug_print(f"    ... and {len(claude_req['tools']) - 3} more")
        
        # Check if request has tools - if so, be more careful with thinking mode
        has_tools = bool(claude_req.get("tools"))
        
        # Smart thinking injection
        target_supports_thinking = model_supports_thinking(mapped_model)
        existing_thinking = claude_req.get("thinking", {})
        thinking_already_enabled = existing_thinking.get("type") == "enabled"
        messages = claude_req.get("messages", [])
        history_compatible = not should_disable_thinking_due_to_history(messages)
        
        # For Cursor with tools, we need to be more careful about thinking mode
        # If there are tools and no valid signature, thinking might cause issues
        has_valid_sig = has_valid_signature_for_function_calls(messages)
        
        can_enable_thinking = (
            self.config.enable_thinking and 
            target_supports_thinking and 
            history_compatible and
            (not has_tools or has_valid_sig)  # Extra check for tools
        )
        
        debug_print(f"\n[Cursor] Thinking mode check:")
        debug_print(f"  config.enable_thinking: {self.config.enable_thinking}")
        debug_print(f"  target_supports_thinking: {target_supports_thinking}")
        debug_print(f"  history_compatible: {history_compatible}")
        debug_print(f"  has_tools: {has_tools}")
        debug_print(f"  has_valid_sig: {has_valid_sig}")
        debug_print(f"  can_enable_thinking: {can_enable_thinking}")
        
        if can_enable_thinking and not thinking_already_enabled:
            claude_req["thinking"] = {
                "type": "enabled",
                "budget_tokens": self.config.thinking_budget
            }
            debug_print(f"[Cursor] Thinking ENABLED, budget={self.config.thinking_budget}")
        elif self.config.enable_thinking and not can_enable_thinking:
            reasons = []
            if not target_supports_thinking:
                reasons.append(f"model '{mapped_model}' does not support thinking")
            if not history_compatible:
                reasons.append("history has tool_use without thinking")
            if has_tools and not has_valid_sig:
                reasons.append("has tools but no valid signature")
            debug_print(f"[Cursor] Thinking DISABLED: {', '.join(reasons)}")
            if "thinking" in claude_req:
                del claude_req["thinking"]
        
        debug_print(f"\n[Cursor] Final: model={mapped_model}, stream={is_stream}, has_tools={has_tools}")
        
        gemini_body = self.transformer.transform(claude_req, self.client.project_id, mapped_model)
        
        # Log Gemini request summary
        debug_print(f"\n[Cursor] Gemini request:")
        debug_print(f"  model: {gemini_body.get('model')}")
        debug_print(f"  requestType: {gemini_body.get('requestType')}")
        inner_req = gemini_body.get("request", {})
        debug_print(f"  contents: {len(inner_req.get('contents', []))} items")
        gen_config = inner_req.get("generationConfig", {})
        debug_print(f"  thinkingConfig: {gen_config.get('thinkingConfig')}")
        tools = inner_req.get("tools", [])
        if tools:
            for tool in tools:
                if tool.get("functionDeclarations"):
                    debug_print(f"  functionDeclarations: {len(tool['functionDeclarations'])} functions")
                if tool.get("googleSearch"):
                    debug_print(f"  googleSearch: enabled")
        
        debug_print("="*60 + "\n")
        
        try:
            status, headers, resp = await self.client.forward_request(gemini_body, stream=True)
            
            debug_print(f"[Cursor] Upstream response: status={status}")
            
            if status >= 400:
                error_text = await resp.text()
                await resp.release()
                debug_print(f"[Cursor] Upstream error: {error_text[:500]}")
                return web.json_response(
                    {"error": {"message": f"Upstream error ({status}): {error_text[:500]}", "type": "api_error"}},
                    status=status
                )
            
            if is_stream:
                return await self._handle_cursor_streaming(request, resp, original_model)
            else:
                return await self._handle_cursor_non_streaming(resp, original_model)
                
        except Exception as e:
            import traceback
            traceback.print_exc()
            debug_print(f"[Cursor] Exception: {e}")
            return web.json_response({"error": {"message": str(e), "type": "api_error"}}, status=500)
    
    async def _handle_cursor_streaming(self, request: web.Request, resp, original_model: str) -> web.StreamResponse:
        """Handle streaming for Cursor format.
        
        Optimized for Cursor tool_calls handling.
        """
        response = web.StreamResponse(status=200, headers={
            "Content-Type": "text/event-stream",
            "Cache-Control": "no-cache",
            "Connection": "keep-alive",
            "X-Accel-Buffering": "no",
        })
        await response.prepare(request)
        
        processor = CursorStreamingProcessor(original_model)
        client_disconnected = False
        
        try:
            buffer = ""
            async for chunk in resp.content.iter_any():
                if client_disconnected:
                    break
                if chunk:
                    buffer += chunk.decode("utf-8", errors="ignore")
                    
                    while "\n" in buffer:
                        line, buffer = buffer.split("\n", 1)
                        line = line.strip()
                        if not line:
                            continue
                        
                        events = processor.process_line(line)
                        if events:
                            if not await self._safe_write(response, events.encode("utf-8")):
                                client_disconnected = True
                                break
            
            # Process remaining buffer (only if client still connected)
            if not client_disconnected and buffer.strip():
                events = processor.process_line(buffer.strip())
                if events:
                    await self._safe_write(response, events.encode("utf-8"))
            
            # Finish
            if not client_disconnected:
                final_events = processor.finish()
                if final_events:
                    await self._safe_write(response, final_events.encode("utf-8"))
        finally:
            await resp.release()
        
        if not client_disconnected:
            try:
                await response.write_eof()
            except Exception:
                pass
        return response
    
    async def _handle_cursor_non_streaming(self, resp, original_model: str) -> web.Response:
        """Handle non-streaming for Cursor format."""
        collected_parts = []
        usage_meta = {}
        response_id = f"chatcmpl-{generate_random_id()}"
        
        try:
            async for chunk in resp.content.iter_any():
                if chunk:
                    text = chunk.decode("utf-8", errors="ignore")
                    for line in text.split("\n"):
                        line = line.strip()
                        if line.startswith("data:"):
                            data = line[5:].strip()
                            if data and data != "[DONE]":
                                try:
                                    v1_resp = json.loads(data)
                                    gemini_resp = v1_resp.get("response", v1_resp)
                                    response_id = v1_resp.get("responseId") or response_id
                                    candidates = gemini_resp.get("candidates", [])
                                    if candidates and candidates[0].get("content"):
                                        collected_parts.extend(candidates[0]["content"].get("parts", []))
                                    if gemini_resp.get("usageMetadata"):
                                        usage_meta = gemini_resp["usageMetadata"]
                                except json.JSONDecodeError:
                                    pass
        finally:
            await resp.release()
        
        # Build OpenAI response from collected parts
        content = ""
        reasoning_content = ""
        tool_calls = []
        
        for part in collected_parts:
            if part.get("thought"):
                thinking_text = part.get("text", "")
                if thinking_text:
                    reasoning_content += thinking_text
                sig = part.get("thoughtSignature", "")
                if sig:
                    global_thought_signature_store(sig)
            elif part.get("functionCall"):
                fc = part["functionCall"]
                tool_id = fc.get("id") or f"{fc.get('name', '')}-{generate_random_id()}"
                tool_calls.append({
                    "id": tool_id,
                    "type": "function",
                    "function": {
                        "name": fc.get("name", ""),
                        "arguments": json.dumps(fc.get("args", {}))
                    }
                })
            elif part.get("text"):
                content += part["text"]
        
        message = {"role": "assistant", "content": content if content else None}
        if reasoning_content:
            message["reasoning_content"] = reasoning_content
        if tool_calls:
            message["tool_calls"] = tool_calls
        
        finish_reason = "stop"
        if tool_calls:
            finish_reason = "tool_calls"
        
        cached = usage_meta.get("cachedContentTokenCount", 0)
        response_data = {
            "id": response_id,
            "object": "chat.completion",
            "created": int(time.time()),
            "model": original_model,
            "choices": [{
                "index": 0,
                "message": message,
                "finish_reason": finish_reason
            }],
            "usage": {
                "prompt_tokens": usage_meta.get("promptTokenCount", 0) - cached,
                "completion_tokens": usage_meta.get("candidatesTokenCount", 0),
                "total_tokens": usage_meta.get("totalTokenCount", 0)
            }
        }
        
        return web.json_response(response_data)
    
    # ============ Cursor2 Responses API ============
    
    async def handle_cursor2_responses(self, request: web.Request) -> web.StreamResponse:
        """Handle /cursor2/v1/responses endpoint (OpenAI Responses API format).
        
        This endpoint implements the OpenAI Responses API format for Cursor and other
        clients that support the newer API format.
        
        Key differences from Chat Completions:
        - Uses 'input' instead of 'messages'
        - Uses 'max_output_tokens' instead of 'max_tokens'
        - Streaming uses semantic events (response.output_text.delta, etc.)
        - Output includes 'reasoning' items for thinking content
        """
        debug_print("\n" + "="*60)
        debug_print("[Cursor2] ========== NEW RESPONSES API REQUEST ==========")
        
        auth_error = self._check_auth(request)
        if auth_error:
            debug_print("[Cursor2] Auth failed")
            return auth_error
        
        try:
            body = await request.json()
        except json.JSONDecodeError:
            debug_print("[Cursor2] Invalid JSON in request body")
            return web.json_response({
                "error": {
                    "message": "Invalid JSON",
                    "type": "invalid_request_error",
                    "code": "invalid_json"
                }
            }, status=400)
        
        # Log request details
        debug_print(f"[Cursor2] Request Headers:")
        for key, value in request.headers.items():
            if key.lower() not in ('authorization', 'x-api-key', 'x-goog-api-key'):
                debug_print(f"  {key}: {value}")
        
        debug_print(f"\n[Cursor2] Request Body:")
        debug_print(f"  model: {body.get('model', 'N/A')}")
        debug_print(f"  stream: {body.get('stream', False)}")
        debug_print(f"  max_output_tokens: {body.get('max_output_tokens', 'N/A')}")
        
        # Log input summary
        input_data = body.get("input", [])
        if isinstance(input_data, str):
            debug_print(f"  input: (string) '{input_data[:100]}...'")
        elif isinstance(input_data, list):
            debug_print(f"  input: ({len(input_data)} items)")
            for i, item in enumerate(input_data[:5]):
                item_type = item.get("type", "unknown")
                role = item.get("role", "")
                debug_print(f"    [{i}] type={item_type}, role={role}")
            if len(input_data) > 5:
                debug_print(f"    ... and {len(input_data) - 5} more")
        
        # Log tools
        tools = body.get("tools", [])
        if tools:
            debug_print(f"\n[Cursor2] Tools ({len(tools)} total):")
            for i, tool in enumerate(tools[:5]):
                tool_type = tool.get("type", "unknown")
                name = tool.get("name", "")
                debug_print(f"  [{i}] type={tool_type}, name={name}")
            if len(tools) > 5:
                debug_print(f"  ... and {len(tools) - 5} more")
        
        project_error = await self._ensure_project()
        if project_error:
            debug_print("[Cursor2] Project error")
            return project_error
        
        original_model = body.get("model", "")
        mapped_model = get_mapped_model(original_model)
        is_stream = body.get("stream", False)
        
        debug_print(f"\n[Cursor2] Model mapping: {original_model} -> {mapped_model}")
        
        # Convert Responses API format to Claude format
        claude_req = ResponsesAPIConverter.responses_to_claude(body)
        
        debug_print(f"\n[Cursor2] Claude request:")
        debug_print(f"  messages: {len(claude_req.get('messages', []))} messages")
        debug_print(f"  system: {'yes' if claude_req.get('system') else 'no'}")
        debug_print(f"  tools: {len(claude_req.get('tools', []))} tools")
        
        # Check if request has tools
        has_tools = bool(claude_req.get("tools"))
        
        # Smart thinking injection
        target_supports_thinking = model_supports_thinking(mapped_model)
        messages = claude_req.get("messages", [])
        history_compatible = not should_disable_thinking_due_to_history(messages)
        has_valid_sig = has_valid_signature_for_function_calls(messages)
        
        can_enable_thinking = (
            self.config.enable_thinking and 
            target_supports_thinking and 
            history_compatible and
            (not has_tools or has_valid_sig)
        )
        
        debug_print(f"\n[Cursor2] Thinking mode check:")
        debug_print(f"  can_enable_thinking: {can_enable_thinking}")
        
        if can_enable_thinking:
            claude_req["thinking"] = {
                "type": "enabled",
                "budget_tokens": self.config.thinking_budget
            }
            debug_print(f"[Cursor2] Thinking ENABLED, budget={self.config.thinking_budget}")
        
        debug_print(f"\n[Cursor2] Final: model={mapped_model}, stream={is_stream}, has_tools={has_tools}")
        
        gemini_body = self.transformer.transform(claude_req, self.client.project_id, mapped_model)
        
        debug_print("="*60 + "\n")
        
        try:
            status, headers, resp = await self.client.forward_request(gemini_body, stream=True)
            
            debug_print(f"[Cursor2] Upstream response: status={status}")
            
            if status >= 400:
                error_text = await resp.text()
                await resp.release()
                debug_print(f"[Cursor2] Upstream error: {error_text[:500]}")
                return web.json_response({
                    "error": {
                        "message": f"Upstream error ({status}): {error_text[:500]}",
                        "type": "api_error",
                        "code": str(status)
                    }
                }, status=status)
            
            if is_stream:
                return await self._handle_cursor2_streaming(request, resp, original_model)
            else:
                return await self._handle_cursor2_non_streaming(resp, original_model)
                
        except Exception as e:
            import traceback
            traceback.print_exc()
            debug_print(f"[Cursor2] Exception: {e}")
            return web.json_response({
                "error": {
                    "message": str(e),
                    "type": "api_error",
                    "code": "internal_error"
                }
            }, status=500)
    
    async def _handle_cursor2_streaming(self, request: web.Request, resp, original_model: str) -> web.StreamResponse:
        """Handle streaming for Cursor2 Responses API format.
        
        Uses ResponsesStreamingProcessor to emit proper Responses API events.
        """
        response = web.StreamResponse(status=200, headers={
            "Content-Type": "text/event-stream",
            "Cache-Control": "no-cache",
            "Connection": "keep-alive",
            "X-Accel-Buffering": "no",
        })
        await response.prepare(request)
        
        processor = ResponsesStreamingProcessor(original_model)
        client_disconnected = False
        
        try:
            buffer = ""
            async for chunk in resp.content.iter_any():
                if client_disconnected:
                    break
                if chunk:
                    buffer += chunk.decode("utf-8", errors="ignore")
                    
                    while "\n" in buffer:
                        line, buffer = buffer.split("\n", 1)
                        line = line.strip()
                        if not line:
                            continue
                        
                        events = processor.process_line(line)
                        if events:
                            if not await self._safe_write(response, events.encode("utf-8")):
                                client_disconnected = True
                                break
            
            # Process remaining buffer
            if not client_disconnected and buffer.strip():
                events = processor.process_line(buffer.strip())
                if events:
                    await self._safe_write(response, events.encode("utf-8"))
            
            # Finish
            if not client_disconnected:
                final_events = processor.finish()
                if final_events:
                    await self._safe_write(response, final_events.encode("utf-8"))
        finally:
            await resp.release()
        
        if not client_disconnected:
            try:
                await response.write_eof()
            except Exception:
                pass
        return response
    
    async def _handle_cursor2_non_streaming(self, resp, original_model: str) -> web.Response:
        """Handle non-streaming for Cursor2 Responses API format."""
        collected_parts = []
        usage_meta = {}
        response_id = f"resp_{generate_random_id()}"
        
        try:
            async for chunk in resp.content.iter_any():
                if chunk:
                    text = chunk.decode("utf-8", errors="ignore")
                    for line in text.split("\n"):
                        line = line.strip()
                        if line.startswith("data:"):
                            data = line[5:].strip()
                            if data and data != "[DONE]":
                                try:
                                    v1_resp = json.loads(data)
                                    gemini_resp = v1_resp.get("response", v1_resp)
                                    response_id = v1_resp.get("responseId") or response_id
                                    candidates = gemini_resp.get("candidates", [])
                                    if candidates and candidates[0].get("content"):
                                        collected_parts.extend(candidates[0]["content"].get("parts", []))
                                    if gemini_resp.get("usageMetadata"):
                                        usage_meta = gemini_resp["usageMetadata"]
                                except json.JSONDecodeError:
                                    pass
        finally:
            await resp.release()
        
        # Build Responses API output from collected parts
        output = []
        message_content = []
        reasoning_text = ""
        
        for part in collected_parts:
            if part.get("thought"):
                thinking_text = part.get("text", "")
                if thinking_text:
                    reasoning_text += thinking_text
                sig = part.get("thoughtSignature", "")
                if sig:
                    global_thought_signature_store(sig)
            elif part.get("functionCall"):
                fc = part["functionCall"]
                tool_id = fc.get("id") or f"call_{generate_random_id()}"
                output.append({
                    "id": tool_id,
                    "type": "function_call",
                    "name": fc.get("name", ""),
                    "call_id": tool_id,
                    "arguments": json.dumps(fc.get("args", {})),
                    "status": "completed"
                })
            elif part.get("text"):
                message_content.append({
                    "type": "output_text",
                    "text": part["text"],
                    "annotations": []
                })
        
        # Add reasoning output item if present
        if reasoning_text:
            output.insert(0, {
                "id": f"rs_{generate_random_id()}",
                "type": "reasoning",
                "summary": [{"type": "summary_text", "text": reasoning_text[:500] + "..." if len(reasoning_text) > 500 else reasoning_text}]
            })
        
        # Add message output item if there's content
        if message_content:
            output.append({
                "id": f"msg_{generate_random_id()}",
                "type": "message",
                "role": "assistant",
                "status": "completed",
                "content": message_content
            })
        
        cached = usage_meta.get("cachedContentTokenCount", 0)
        response_data = {
            "id": response_id,
            "object": "response",
            "created_at": int(time.time()),
            "status": "completed",
            "model": original_model,
            "output": output,
            "usage": {
                "input_tokens": usage_meta.get("promptTokenCount", 0) - cached,
                "output_tokens": usage_meta.get("candidatesTokenCount", 0),
                "total_tokens": usage_meta.get("totalTokenCount", 0)
            }
        }
        
        return web.json_response(response_data)
    
    # ============ Gemini API ============
    
    async def handle_gemini_generate(self, request: web.Request) -> web.StreamResponse:
        """Handle /v1beta/models/{model}:generateContent endpoint (Gemini API).
        
        Non-streaming Gemini protocol with thinking support.
        """
        auth_error = self._check_auth(request)
        if auth_error:
            return auth_error
        
        # Extract model from path (e.g., "gemini-2.5-flash:generateContent" -> "gemini-2.5-flash")
        model_action = request.match_info.get("model_action", "")
        if ":" in model_action:
            original_model = model_action.rsplit(":", 1)[0]
        else:
            original_model = model_action
        
        try:
            body = await request.json()
        except json.JSONDecodeError:
            return web.json_response({"error": {"message": "Invalid JSON", "code": 400}}, status=400)
        
        project_error = await self._ensure_project()
        if project_error:
            return project_error
        
        mapped_model = get_mapped_model(original_model)
        
        # Convert Gemini request to Claude format for internal processing
        claude_req = GeminiConverter.gemini_to_claude(body, mapped_model)
        
        # Smart thinking injection
        target_supports_thinking = model_supports_thinking(mapped_model)
        existing_thinking = claude_req.get("thinking", {})
        thinking_already_enabled = existing_thinking.get("type") == "enabled"
        messages = claude_req.get("messages", [])
        history_compatible = not should_disable_thinking_due_to_history(messages)
        
        can_enable_thinking = (
            self.config.enable_thinking and 
            target_supports_thinking and 
            history_compatible
        )
        
        if can_enable_thinking and not thinking_already_enabled:
            claude_req["thinking"] = {
                "type": "enabled",
                "budget_tokens": self.config.thinking_budget
            }
            debug_print(f"[Gemini Generate] Thinking enabled, budget={self.config.thinking_budget}")
        elif self.config.enable_thinking and not can_enable_thinking:
            reasons = []
            if not target_supports_thinking:
                reasons.append(f"model '{mapped_model}' does not support thinking")
            if not history_compatible:
                reasons.append("history has tool_use without thinking")
            debug_print(f"[Gemini Generate] Thinking DISABLED: {', '.join(reasons)}")
            if "thinking" in claude_req:
                del claude_req["thinking"]
        
        debug_print(f"[Gemini Generate] original_model={original_model}, mapped_model={mapped_model}")
        
        gemini_body = self.transformer.transform(claude_req, self.client.project_id, mapped_model)
        
        try:
            status, headers, resp = await self.client.forward_request(gemini_body, stream=True)
            
            if status >= 400:
                error_text = await resp.text()
                await resp.release()
                return web.json_response(
                    {"error": {"message": f"Upstream error ({status}): {error_text[:500]}", "code": status}},
                    status=status
                )
            
            return await self._handle_gemini_non_streaming(resp, original_model)
                
        except Exception as e:
            import traceback
            traceback.print_exc()
            return web.json_response({"error": {"message": str(e), "code": 500}}, status=500)
    
    async def handle_gemini_stream_generate(self, request: web.Request) -> web.StreamResponse:
        """Handle /v1beta/models/{model}:streamGenerateContent endpoint (Gemini API).
        
        Streaming Gemini protocol with thinking support.
        """
        auth_error = self._check_auth(request)
        if auth_error:
            return auth_error
        
        # Extract model from path
        model_action = request.match_info.get("model_action", "")
        if ":" in model_action:
            original_model = model_action.rsplit(":", 1)[0]
        else:
            original_model = model_action
        
        try:
            body = await request.json()
        except json.JSONDecodeError:
            return web.json_response({"error": {"message": "Invalid JSON", "code": 400}}, status=400)
        
        project_error = await self._ensure_project()
        if project_error:
            return project_error
        
        mapped_model = get_mapped_model(original_model)
        
        # Convert Gemini request to Claude format
        claude_req = GeminiConverter.gemini_to_claude(body, mapped_model)
        
        # Smart thinking injection
        target_supports_thinking = model_supports_thinking(mapped_model)
        existing_thinking = claude_req.get("thinking", {})
        thinking_already_enabled = existing_thinking.get("type") == "enabled"
        messages = claude_req.get("messages", [])
        history_compatible = not should_disable_thinking_due_to_history(messages)
        
        can_enable_thinking = (
            self.config.enable_thinking and 
            target_supports_thinking and 
            history_compatible
        )
        
        if can_enable_thinking and not thinking_already_enabled:
            claude_req["thinking"] = {
                "type": "enabled",
                "budget_tokens": self.config.thinking_budget
            }
            debug_print(f"[Gemini Stream] Thinking enabled, budget={self.config.thinking_budget}")
        elif self.config.enable_thinking and not can_enable_thinking:
            reasons = []
            if not target_supports_thinking:
                reasons.append(f"model '{mapped_model}' does not support thinking")
            if not history_compatible:
                reasons.append("history has tool_use without thinking")
            debug_print(f"[Gemini Stream] Thinking DISABLED: {', '.join(reasons)}")
            if "thinking" in claude_req:
                del claude_req["thinking"]
        
        debug_print(f"[Gemini Stream] original_model={original_model}, mapped_model={mapped_model}")
        
        gemini_body = self.transformer.transform(claude_req, self.client.project_id, mapped_model)
        
        try:
            status, headers, resp = await self.client.forward_request(gemini_body, stream=True)
            
            if status >= 400:
                error_text = await resp.text()
                await resp.release()
                return web.json_response(
                    {"error": {"message": f"Upstream error ({status}): {error_text[:500]}", "code": status}},
                    status=status
                )
            
            return await self._handle_gemini_streaming(request, resp, original_model)
                
        except Exception as e:
            import traceback
            traceback.print_exc()
            return web.json_response({"error": {"message": str(e), "code": 500}}, status=500)
    
    async def _handle_gemini_non_streaming(self, resp, original_model: str) -> web.Response:
        """Handle non-streaming response for Gemini format.
        
        Collects all parts from upstream and returns as Gemini format.
        """
        collected_parts = []
        usage_meta = {}
        finish_reason = "STOP"
        
        try:
            async for chunk in resp.content.iter_any():
                if chunk:
                    text = chunk.decode("utf-8", errors="ignore")
                    for line in text.split("\n"):
                        line = line.strip()
                        if line.startswith("data:"):
                            data = line[5:].strip()
                            if data and data != "[DONE]":
                                try:
                                    v1_resp = json.loads(data)
                                    gemini_resp = v1_resp.get("response", v1_resp)
                                    candidates = gemini_resp.get("candidates", [])
                                    if candidates:
                                        content = candidates[0].get("content", {})
                                        collected_parts.extend(content.get("parts", []))
                                        if candidates[0].get("finishReason"):
                                            finish_reason = candidates[0]["finishReason"]
                                    if gemini_resp.get("usageMetadata"):
                                        usage_meta = gemini_resp["usageMetadata"]
                                except json.JSONDecodeError:
                                    pass
        finally:
            await resp.release()
        
        # Build Gemini response
        response_data = {
            "candidates": [{
                "content": {
                    "role": "model",
                    "parts": collected_parts
                },
                "finishReason": finish_reason
            }],
            "usageMetadata": usage_meta,
            "modelVersion": original_model
        }
        
        return web.json_response(response_data)
    
    async def _handle_gemini_streaming(self, request: web.Request, resp, original_model: str) -> web.StreamResponse:
        """Handle streaming response for Gemini format.
        
        Passes through upstream SSE events with unwrapping.
        """
        response = web.StreamResponse(status=200, headers={
            "Content-Type": "text/event-stream",
            "Cache-Control": "no-cache",
            "Connection": "keep-alive",
            "X-Accel-Buffering": "no",
        })
        await response.prepare(request)
        client_disconnected = False
        
        try:
            buffer = ""
            async for chunk in resp.content.iter_any():
                if client_disconnected:
                    break
                if chunk:
                    buffer += chunk.decode("utf-8", errors="ignore")
                    
                    while "\n" in buffer:
                        if client_disconnected:
                            break
                        line, buffer = buffer.split("\n", 1)
                        line = line.strip()
                        if not line:
                            continue
                        
                        if line.startswith("data:"):
                            data = line[5:].strip()
                            if data == "[DONE]":
                                if not await self._safe_write(response, b"data: [DONE]\n\n"):
                                    client_disconnected = True
                                continue
                            
                            if data:
                                try:
                                    v1_resp = json.loads(data)
                                    # Unwrap v1internal response
                                    gemini_resp = v1_resp.get("response", v1_resp)
                                    # Add model version
                                    gemini_resp["modelVersion"] = original_model
                                    # Send as SSE
                                    if not await self._safe_write(response, f"data: {json.dumps(gemini_resp)}\n\n".encode()):
                                        client_disconnected = True
                                        break
                                except json.JSONDecodeError:
                                    # Pass through raw line
                                    if not await self._safe_write(response, f"{line}\n\n".encode()):
                                        client_disconnected = True
                                        break
                        else:
                            # Non-data lines (comments, etc.)
                            if not await self._safe_write(response, f"{line}\n\n".encode()):
                                client_disconnected = True
                                break
            
            # Process remaining buffer (only if client still connected)
            if not client_disconnected and buffer.strip():
                line = buffer.strip()
                if line.startswith("data:"):
                    data = line[5:].strip()
                    if data and data != "[DONE]":
                        try:
                            v1_resp = json.loads(data)
                            gemini_resp = v1_resp.get("response", v1_resp)
                            gemini_resp["modelVersion"] = original_model
                            await self._safe_write(response, f"data: {json.dumps(gemini_resp)}\n\n".encode())
                        except json.JSONDecodeError:
                            pass
        finally:
            await resp.release()
        
        if not client_disconnected:
            try:
                await response.write_eof()
            except Exception:
                pass
        return response
    
    async def handle_gemini_models(self, request: web.Request) -> web.Response:
        """Handle /v1beta/models endpoint (Gemini API)."""
        models = []
        for model_id in sorted(SUPPORTED_MODELS):
            models.append({
                "name": f"models/{model_id}",
                "version": "001",
                "displayName": model_id,
                "description": "",
                "inputTokenLimit": 128000,
                "outputTokenLimit": 8192,
                "supportedGenerationMethods": ["generateContent", "countTokens"],
                "temperature": 1.0,
                "topP": 0.95,
                "topK": 64
            })
        return web.json_response({"models": models})
    
    async def handle_gemini_get_model(self, request: web.Request) -> web.Response:
        """Handle /v1beta/models/{model} GET endpoint."""
        model_name = request.match_info.get("model", "")
        return web.json_response({
            "name": f"models/{model_name}",
            "displayName": model_name,
            "version": "001",
            "inputTokenLimit": 128000,
            "outputTokenLimit": 8192,
            "supportedGenerationMethods": ["generateContent", "countTokens"]
        })
    
    # ============ Common Endpoints ============
    
    async def handle_models(self, request: web.Request) -> web.Response:
        """Handle /v1/models endpoint."""
        models = [
            {"id": m, "object": "model", "created": 1700000000, "owned_by": "antigravity"}
            for m in sorted(SUPPORTED_MODELS)
        ]
        return web.json_response({"object": "list", "data": models})
    
    async def handle_health(self, request: web.Request) -> web.Response:
        """Handle health check."""
        return web.json_response({"status": "ok"})


async def create_app(config: Config) -> web.Application:
    """Create aiohttp application."""
    proxy = AntigravityProxy(config)
    
    app = web.Application()
    
    # Anthropic Claude API
    app.router.add_post("/v1/messages", proxy.handle_claude_messages)
    
    # OpenAI API
    app.router.add_post("/v1/chat/completions", proxy.handle_openai_chat)
    app.router.add_post("/v1/completions", proxy.handle_completions)
    app.router.add_post("/v1/responses", proxy.handle_responses)
    
    # Cursor API (OpenAI-compatible with better tool_calls support)
    app.router.add_post("/cursor/v1/chat/completions", proxy.handle_cursor_chat)
    
    # Cursor2 API (OpenAI Responses API format)
    app.router.add_post("/cursor2/v1/responses", proxy.handle_cursor2_responses)
    
    # Gemini API
    app.router.add_post("/v1beta/models/{model_action:.*:generateContent}", proxy.handle_gemini_generate)
    app.router.add_post("/v1beta/models/{model_action:.*:streamGenerateContent}", proxy.handle_gemini_stream_generate)
    app.router.add_get("/v1beta/models", proxy.handle_gemini_models)
    app.router.add_get("/v1beta/models/{model}", proxy.handle_gemini_get_model)
    
    # Common
    app.router.add_get("/v1/models", proxy.handle_models)
    app.router.add_get("/health", proxy.handle_health)
    
    async def on_cleanup(app):
        await proxy.client.close()
    
    app.on_cleanup.append(on_cleanup)
    return app


def main():
    global _rate_limiter
    
    # Load config
    config = Config.load()
    
    # Check for command line overrides
    if len(sys.argv) > 1:
        for i, arg in enumerate(sys.argv[1:], 1):
            if arg == "--refresh-token" and i < len(sys.argv):
                config.refresh_token = sys.argv[i + 1]
            elif arg == "--port" and i < len(sys.argv):
                config.port = int(sys.argv[i + 1])
            elif arg == "--api-key" and i < len(sys.argv):
                config.api_key = sys.argv[i + 1]
    
    if not config.refresh_token:
        print("Error: refresh_token is required.")
        print("Set 'antigravity_refresh_token' in config.json or use --refresh-token")
        sys.exit(1)
    
    # Initialize rate limiter with config
    _rate_limiter = RateLimiter(
        max_requests=config.rate_limit_requests,
        window_seconds=config.rate_limit_window,
        min_interval=config.rate_limit_interval
    )
    
    async def run():
        app = await create_app(config)
        runner = web.AppRunner(app)
        await runner.setup()
        site = web.TCPSite(runner, config.host, config.port)
        
        print(f"Starting Antigravity Proxy on http://{config.host}:{config.port}")
        print(f"API Key: {config.api_key[:10]}...")
        print(f"Rate Limit: {config.rate_limit_requests} req/{config.rate_limit_window}s, min interval {config.rate_limit_interval}s")
        print(f"Thinking: {'enabled' if config.enable_thinking else 'disabled'}, budget={config.thinking_budget}")
        print("\nEndpoints:")
        print("  POST /v1/messages                              - Anthropic Claude API")
        print("  POST /v1/chat/completions                      - OpenAI Chat API")
        print("  POST /v1/completions                           - OpenAI Legacy Completions API")
        print("  POST /v1/responses                             - Codex Responses API")
        print("  POST /cursor/v1/chat/completions               - Cursor API (with tool_calls)")
        print("  POST /cursor2/v1/responses                     - Cursor2 API (OpenAI Responses API)")
        print("  POST /v1beta/models/{model}:generateContent    - Gemini API (non-streaming)")
        print("  POST /v1beta/models/{model}:streamGenerateContent - Gemini API (streaming)")
        print("  GET  /v1/models                                - List models (OpenAI)")
        print("  GET  /v1beta/models                            - List models (Gemini)")
        print("  GET  /health                                   - Health check")
        
        await site.start()
        while True:
            await asyncio.sleep(3600)
    
    try:
        asyncio.run(run())
    except KeyboardInterrupt:
        print("\nShutting down...")


if __name__ == "__main__":
    main()
