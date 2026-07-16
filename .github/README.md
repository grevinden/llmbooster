# llmbooster

Bifrost plugin for pre-LLM request filtering. Replaces Plano AI with a pure OSS `.so` plugin that filters requests using the same LLM provider, leveraging prompt caching for fast repeated calls.

## Architecture

```
Client (Copilot SSE)
  → Bifrost
    → mcpfilter PreLLMHook
      ├─ Keyword check (<1ms)
      └─ LLM filter call (same provider, prompt cache warm)
           ├─ Context cancelled → abort immediately, 0 tokens spent
           ├─ Block → LLMPluginShortCircuit{Error}
           └─ Pass → continue to main LLM call
```

The filter call goes directly to the LLM provider (not through Bifrost) via `http.NewRequestWithContext(ctx, ...)`. When the client disconnects, the `BifrostContext` is cancelled, aborting the filter HTTP request instantly.

## Quick Start

```bash
# Build the plugin
cd plugins/mcpfilter
go mod tidy
go build -buildmode=plugin -o mcpfilter.so .

# Load in Bifrost Web UI → Plugins → Add Plugin → path to mcpfilter.so
```

## Configuration

```json
{
  "enabled": true,
  "system_prompt": "You are a content filter. Respond with BLOCK or PASS.",
  "model_override": "gpt-4o-mini",
  "max_tokens": 100,
  "reject_on_block": true,
  "fail_open": true,
  "timeout_seconds": 30,
  "block_keywords": ["hack", "exploit"],
  "provider_base_url": "https://api.openai.com/v1",
  "provider_api_key": "env.OPENAI_API_KEY"
}
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `false` | Enable the filter |
| `system_prompt` | string | `""` | LLM filter prompt (empty = keyword-only) |
| `model_override` | string | `""` | Model for filter calls (uses request model if empty) |
| `max_tokens` | int | `100` | Max tokens for filter response |
| `reject_on_block` | bool | `true` | Return 400 error when blocked |
| `fail_open` | bool | `true` | Allow request if filter fails |
| `timeout_seconds` | int | `30` | Filter HTTP timeout |
| `block_keywords` | []string | `[]` | Case-insensitive keyword blocklist |
| `provider_base_url` | string | `https://api.openai.com/v1` | LLM API endpoint |
| `provider_api_key` | string | `""` | API key |

## How It Works

1. Request arrives at Bifrost
2. `PreLLMHook` fires — keyword check runs first (<1ms)
3. If `system_prompt` is set, sends a non-streaming filter call to the same provider
4. First call warms the prompt cache; subsequent calls are 50-100ms (vs 200-500ms cold)
5. If filter response contains "block" or "reject" → `LLMPluginShortCircuit` with 400 error
6. If pass → request continues to the main LLM call

## Context Cancellation

When a client disconnects (e.g., Copilot "Cancel"):

1. `BifrostContext.Done()` fires
2. `http.NewRequestWithContext` propagates the cancellation
3. Filter HTTP call aborts immediately
4. Plugin returns `filterResult{Blocked: false}` — no tokens spent
5. Main LLM call never starts

## Project Structure

```
llmbooster/
├── .github/README.md          # This file
├── .gitmodules                # Bifrost submodule
├── lib/bifrost/               # Bifrost (git submodule)
├── plugins/mcpfilter/
│   ├── main.go                # Plugin implementation
│   ├── go.mod                 # Go module with replace directive
│   └── README.md              # Plugin-specific docs
├── .agents/skills/
│   └── code-simplification/   # Agent skill
└── talks/                     # Research notes
```

## Development

The plugin is a Go shared object (`.so`) loaded by Bifrost via `plugin.Open()`. It exports package-level functions:

- `Init(config any) error` — parse config map
- `GetName() string` — plugin identifier
- `Cleanup() error` — cleanup on unload
- `PreLLMHook(ctx, req) (req, shortCircuit, error)` — pre-filter
- `PostLLMHook(ctx, resp, err) (resp, err, error)` — post-hook (passthrough)

Build requires the same Go version as Bifrost (currently 1.26.4).

## License

MIT
