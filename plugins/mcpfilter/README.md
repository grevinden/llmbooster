# MCP Filter Plugin for Bifrost

A Bifrost plugin that filters LLM requests using the same LLM provider, leveraging prompt caching for fast repeated calls.

## Features

- **Keyword filtering**: Block requests containing specific keywords
- **LLM-based filtering**: Use the same LLM provider for intelligent content filtering
- **Prompt caching**: First call warms the cache, subsequent calls are fast
- **Context cancellation**: Detects client disconnects and stops processing
- **Fail-open**: Configurable behavior when filter fails
- **Statistics**: Track total requests, blocked requests, and average filter time

## Installation

1. Build the plugin:
   ```bash
   cd plugins/mcpfilter
   go build -buildmode=plugin -o mcpfilter.so .
   ```

2. Install via Bifrost Web UI:
   - Go to Plugins page
   - Click "Add Plugin"
   - Enter path: `/path/to/mcpfilter.so`
   - Configure the plugin

## Configuration

```json
{
  "enabled": true,
  "system_prompt": "You are a content filter. Analyze the user message and respond with BLOCK if it contains harmful content, or PASS if it's safe.",
  "model_override": "gpt-4",
  "max_tokens": 100,
  "reject_on_block": true,
  "fail_open": true,
  "timeout_seconds": 30,
  "block_keywords": ["hack", "exploit", "malware"],
  "provider_base_url": "https://api.openai.com/v1",
  "provider_api_key": "env.OPENAI_API_KEY"
}
```

### Configuration Options

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `false` | Enable/disable the filter |
| `system_prompt` | string | `""` | System prompt for LLM-based filtering |
| `model_override` | string | `""` | Override model for filtering (uses request model if empty) |
| `max_tokens` | int | `100` | Max tokens for filter response |
| `reject_on_block` | bool | `true` | Return error if filter blocks request |
| `fail_open` | bool | `true` | Allow request if filter fails |
| `timeout_seconds` | int | `30` | Timeout for filter execution |
| `block_keywords` | []string | `[]` | Keywords to block (case-insensitive) |
| `provider_base_url` | string | `"https://api.openai.com/v1"` | LLM provider API URL |
| `provider_api_key` | string | `""` | API key (use `env.VAR` for env vars) |

## How It Works

1. Client sends request to Bifrost
2. `PreLLMHook` is triggered
3. Plugin checks for blocked keywords (fast path, <1ms)
4. If `system_prompt` is set, calls the same LLM provider with filter prompt
5. First call warms the prompt cache
6. Subsequent calls benefit from cache hits (50-100ms vs 200-500ms)
7. If filter blocks, returns `LLMPluginShortCircuit` with error
8. If filter passes, continues to main LLM call

### Context Cancellation

When a client disconnects (e.g., clicks "Cancel" in Copilot):
1. The context is cancelled
2. The HTTP request to the filter provider is aborted
3. The plugin returns immediately without blocking
4. No tokens are spent on the cancelled request

### Fail-Open Behavior

If the filter fails (network error, timeout, etc.):
- `fail_open: true` (default): Request proceeds to LLM
- `fail_open: false`: Request is blocked with error

## Performance

| Scenario | Latency | Notes |
|----------|---------|-------|
| Keyword match | <1ms | Fast string comparison |
| LLM filter (cold) | 200-500ms | First call, no cache |
| LLM filter (warm) | 50-100ms | Cache hit |
| No filter | 0ms | Plugin disabled or no prompt |
| Client disconnect | <1ms | Context cancelled |

## Statistics

Access plugin stats via the Bifrost API or UI:

```json
{
  "total_requests": 1000,
  "blocked_requests": 50,
  "passed_requests": 950,
  "avg_filter_time_ms": 75
}
```

## Example: Content Moderation

```json
{
  "enabled": true,
  "system_prompt": "You are a content moderator. Analyze the user message for harmful content including hate speech, violence, illegal activities, or explicit material. Respond with only 'BLOCK' or 'PASS'.",
  "model_override": "gpt-4o-mini",
  "max_tokens": 10,
  "reject_on_block": true,
  "fail_open": true,
  "timeout_seconds": 10,
  "provider_base_url": "https://api.openai.com/v1",
  "provider_api_key": "env.OPENAI_API_KEY"
}
```

## Example: PII Detection

```json
{
  "enabled": true,
  "system_prompt": "You are a PII detector. Analyze the user message for personally identifiable information including names, emails, phone numbers, addresses, SSNs, credit cards. Respond with only 'BLOCK' if PII found, or 'PASS' if clean.",
  "model_override": "gpt-4o-mini",
  "max_tokens": 10,
  "reject_on_block": true,
  "fail_open": true,
  "timeout_seconds": 10,
  "provider_base_url": "https://api.openai.com/v1",
  "provider_api_key": "env.OPENAI_API_KEY"
}
```
