# mcpfilter Implementation

## Architecture

The plugin uses `PreRequestHook` to enrich user requests with LLM-generated context.

### Flow

```
Client Request -> PreRequestHook -> [mcpfilter logic] -> Bifrost -> Provider
```

### Steps

1. Config Check: Read thread-safe config, return if disabled
2. Empty Message Removal: Remove ALL empty messages (any role)
3. User Message Check: Only process if last message is from user
4. Context Building: Build context_mcpfilter with custom system prompt
5. Sub-request: Send to LLM with timeout
6. Response Handling: Append assistant message with LLM's response
7. Fallback: On error, restore original input

## Key Changes from Original

| Aspect | Original | New |
|--------|----------|-----|
| Hook | PreLLMHook | PreRequestHook |
| Calls per request | Multiple (per fallback) | Once |
| Empty message removal | Trailing only | ALL (any position) |
| Response role | system | assistant |
| Error handling | Mutated before check | Original preserved |
| Thread safety | No | Yes (sync.RWMutex) |
| Enabled setting | Configurable | Always enabled (system prompt controls activation) |

## Thread Safety

```go
var (
    configMu      sync.RWMutex
    currentConfig FilterConfig
    httpClient    *http.Client
)
```

- Config read with RLock() in hooks
- Config write with Lock() in Init()

## API Key Handling

Currently returns empty string. In production, should extract from:
- Bifrost secret store
- Request headers
- Context values

## Future Enhancements

1. Extract API key from Bifrost context
2. Support ContentBlocks in messages
3. Configurable max_tokens, temperature
4. Retry logic for transient errors
5. Metrics/observability
