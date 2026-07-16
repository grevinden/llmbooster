package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
)

const PluginName = "mcpfilter"

type FilterConfig struct {
	SystemPrompt string `json:"system"`
	TimeoutSecs  int    `json:"timeout,omitempty"`
}

var (
	configMu      sync.RWMutex
	currentConfig FilterConfig
	httpClient    *http.Client
)

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature float64       `json:"temperature"`
	Stream      bool          `json:"stream"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

func Init(config any) error {
	cfg, ok := config.(map[string]any)
	if !ok {
		return nil
	}

	newConfig := FilterConfig{
		TimeoutSecs: 10,
	}
	newConfig.SystemPrompt, _ = cfg["system_prompt"].(string)
	if v, ok := cfg["timeout_seconds"].(float64); ok {
		newConfig.TimeoutSecs = int(v)
	}

	newHTTPClient := &http.Client{
		Timeout: time.Duration(newConfig.TimeoutSecs) * time.Second,
	}

	configMu.Lock()
	currentConfig = newConfig
	httpClient = newHTTPClient
	configMu.Unlock()

	return nil
}

func GetName() string { return PluginName }
func Cleanup() error  { return nil }

// PreRequestHook is called once per top-level request.
// It enriches the user's last message by sub-querying the LLM with a custom system prompt.
// Mutations commit to req and are observed by all subsequent plugins, the provider call,
// and every fallback attempt.
func PreRequestHook(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) error {
	// 1. Read config (thread-safe)
	configMu.RLock()
	cfg := currentConfig
	client := httpClient
	configMu.RUnlock()

	// 2. Check if system prompt is set
	if cfg.SystemPrompt == "" {
		return nil
	}

	// 3. Check if chat request exists
	if req.ChatRequest == nil || len(req.ChatRequest.Input) == 0 {
		return nil
	}

	// 4. Save original input for fallback on error
	originalInput := make([]schemas.ChatMessage, len(req.ChatRequest.Input))
	copy(originalInput, req.ChatRequest.Input)

	// 5. Remove ALL empty messages (any role, any position)
	filtered := removeEmptyMessages(req.ChatRequest.Input)
	if len(filtered) == 0 {
		return nil
	}

	// 6. Last message must be from user
	lastMsg := filtered[len(filtered)-1]
	if lastMsg.Role != schemas.ChatMessageRoleUser {
		return nil
	}

	// Extract user text
	userText := extractMessageContent(&lastMsg)
	if userText == "" {
		return nil
	}

	// 7. Build context_mcpfilter for sub-request
	// - First message: system prompt from config
	// - Rest: all non-system messages from filtered context
	contextMCP := buildContextMCP(filtered, cfg.SystemPrompt)

	// 8. Sub-request to LLM with timeout
	enhanced := enhanceWithTimeout(ctx, contextMCP, req.ChatRequest.Model, req.ChatRequest.Provider, client, cfg.TimeoutSecs)
	if enhanced == "" {
		// On error/timeout: restore original input (mcpfilter did nothing)
		req.ChatRequest.Input = originalInput
		return nil
	}

	// 9. Append assistant message with LLM's response (unmodified)
	assistantMsg := schemas.ChatMessage{
		Role:    schemas.ChatMessageRoleAssistant,
		Content: &schemas.ChatMessageContent{ContentStr: &enhanced},
	}
	req.ChatRequest.Input = append(req.ChatRequest.Input, assistantMsg)

	return nil
}

// PreLLMHook is not used by mcpfilter (we use PreRequestHook instead)
func PreLLMHook(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) (
	*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error,
) {
	return req, nil, nil
}

// PostLLMHook is not used by mcpfilter
func PostLLMHook(ctx *schemas.BifrostContext, resp *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) (
	*schemas.BifrostResponse, *schemas.BifrostError, error,
) {
	return resp, bifrostErr, nil
}

// removeEmptyMessages removes all messages with empty content (any role)
func removeEmptyMessages(messages []schemas.ChatMessage) []schemas.ChatMessage {
	result := make([]schemas.ChatMessage, 0, len(messages))
	for _, msg := range messages {
		if !isEmptyMessage(&msg) {
			result = append(result, msg)
		}
	}
	return result
}

// isEmptyMessage checks if a message has empty content
func isEmptyMessage(msg *schemas.ChatMessage) bool {
	if msg.Content == nil {
		return true
	}
	if msg.Content.ContentStr != nil && *msg.Content.ContentStr == "" {
		return true
	}
	// If ContentBlocks is present, message is not empty
	if len(msg.Content.ContentBlocks) > 0 {
		return false
	}
	return true
}

// extractMessageContent extracts text content from a message
func extractMessageContent(msg *schemas.ChatMessage) string {
	if msg.Content == nil {
		return ""
	}
	if msg.Content.ContentStr != nil {
		return *msg.Content.ContentStr
	}
	// For ContentBlocks, we'd need to serialize - for now return empty
	// This can be enhanced if needed
	return ""
}

// buildContextMCP builds the context for the sub-request
// - First message: system prompt from config
// - Rest: all non-system messages from the filtered context
func buildContextMCP(filtered []schemas.ChatMessage, systemPrompt string) []chatMessage {
	result := make([]chatMessage, 0, len(filtered)+1)

	// First message: system prompt
	result = append(result, chatMessage{
		Role:    "system",
		Content: systemPrompt,
	})

	// Rest: non-system messages
	for _, msg := range filtered {
		if msg.Role == schemas.ChatMessageRoleSystem {
			continue // skip original system messages
		}

		content := extractMessageContent(&msg)
		result = append(result, chatMessage{
			Role:    string(msg.Role),
			Content: content,
		})
	}

	return result
}

// enhanceWithTimeout sends a sub-request to LLM with timeout handling
func enhanceWithTimeout(
	ctx *schemas.BifrostContext,
	messages []chatMessage,
	model string,
	provider schemas.ModelProvider,
	client *http.Client,
	timeoutSecs int,
) string {
	// Create request body
	body, err := json.Marshal(chatRequest{
		Model:       model,
		Messages:    messages,
		MaxTokens:   1024,
		Temperature: 0.3,
	})
	if err != nil {
		ctx.Log(schemas.LogLevelError, fmt.Sprintf("MCPFilter: failed to marshal request: %v", err))
		return ""
	}

	// Use Bifrost's built-in provider URL construction
	// provider is passed as parameter from req.ChatRequest.Provider
	baseURL := getProviderBaseURL(provider)
	url := strings.TrimRight(baseURL, "/") + "/chat/completions"

	// Create HTTP request with context timeout
	httpCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSecs)*time.Second)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(httpCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		ctx.Log(schemas.LogLevelError, fmt.Sprintf("MCPFilter: failed to create request: %v", err))
		return ""
	}
	httpReq.Header.Set("Content-Type", "application/json")

	// Get API key from Bifrost context
	apiKey := getProviderAPIKey(ctx, provider)
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}

	// Execute request
	resp, err := client.Do(httpReq)
	if err != nil {
		if httpCtx.Err() == context.Canceled {
			ctx.Log(schemas.LogLevelInfo, "MCPFilter: client disconnected, skipping enhancement")
		} else {
			ctx.Log(schemas.LogLevelError, fmt.Sprintf("MCPFilter: request error: %v", err))
		}
		return ""
	}
	defer resp.Body.Close()

	// Check status code
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		ctx.Log(schemas.LogLevelError, fmt.Sprintf("MCPFilter: status %d: %s", resp.StatusCode, string(b)))
		return ""
	}

	// Parse response
	var result chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		ctx.Log(schemas.LogLevelError, fmt.Sprintf("MCPFilter: failed to decode response: %v", err))
		return ""
	}
	if len(result.Choices) == 0 {
		ctx.Log(schemas.LogLevelWarn, "MCPFilter: no choices in response")
		return ""
	}

	content := strings.TrimSpace(result.Choices[0].Message.Content)
	if content == "" {
		ctx.Log(schemas.LogLevelWarn, "MCPFilter: empty content in response")
		return ""
	}

	return content
}

// getProviderBaseURL returns the base URL for the given provider
func getProviderBaseURL(provider schemas.ModelProvider) string {
	switch provider {
	case schemas.OpenAI:
		return "https://api.openai.com/v1"
	case schemas.Anthropic:
		return "https://api.anthropic.com/v1"
	case schemas.Azure:
		return "https://<resource-name>.openai.azure.com/openai/deployments"
	case schemas.Gemini:
		return "https://generativelanguage.googleapis.com/v1beta"
	case schemas.Ollama:
		return "http://localhost:11434/v1"
	default:
		return "https://api.openai.com/v1"
	}
}

// getProviderAPIKey returns the API key for the given provider from context
func getProviderAPIKey(ctx *schemas.BifrostContext, provider schemas.ModelProvider) string {
	// For now, returns empty string - plugin will use environment variables or default auth
	// In production, extract from headers or Bifrost's secret store
	// Example: ctx.Value("api_key") or from req headers
	return ""
}
