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

// ─── Configuration ──────────────────────────────────────────────────────────

type FilterConfig struct {
	Enabled         bool     `json:"enabled"`
	SystemPrompt    string   `json:"system_prompt"`
	ModelOverride   string   `json:"model_override,omitempty"`
	MaxTokens       int      `json:"max_tokens,omitempty"`
	RejectOnBlock   bool     `json:"reject_on_block"`
	FailOpen        bool     `json:"fail_open"`
	TimeoutSeconds  int      `json:"timeout_seconds,omitempty"`
	BlockKeywords   []string `json:"block_keywords,omitempty"`
	ProviderBaseURL string   `json:"provider_base_url,omitempty"`
	ProviderAPIKey  string   `json:"provider_api_key,omitempty"`
}

// ─── Package-level state (shared across all requests) ───────────────────────

var (
	currentConfig FilterConfig
	httpClient    *http.Client
	pluginMu      sync.RWMutex
	pluginStats   FilterStats
)

type FilterStats struct {
	TotalRequests   int64 `json:"total_requests"`
	BlockedRequests int64 `json:"blocked_requests"`
	PassedRequests  int64 `json:"passed_requests"`
	AvgFilterTimeMs int64 `json:"avg_filter_time_ms"`
}

// ─── HTTP request/response types (for direct provider calls) ────────────────

type filterChatRequest struct {
	Model       string              `json:"model"`
	Messages    []filterChatMessage `json:"messages"`
	MaxTokens   int                 `json:"max_tokens,omitempty"`
	Temperature float64             `json:"temperature"`
	Stream      bool                `json:"stream"`
}

type filterChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type filterChatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// ─── Plugin lifecycle (required symbols) ────────────────────────────────────

// Init is called when the plugin is loaded.
func Init(config any) error {
	cfg, ok := config.(map[string]any)
	if !ok {
		return nil
	}

	pluginMu.Lock()
	defer pluginMu.Unlock()

	currentConfig = FilterConfig{
		RejectOnBlock:   true,
		FailOpen:        true,
		TimeoutSeconds:  30,
		ProviderBaseURL: "https://api.openai.com/v1",
	}

	if v, ok := cfg["enabled"].(bool); ok {
		currentConfig.Enabled = v
	}
	if v, ok := cfg["system_prompt"].(string); ok {
		currentConfig.SystemPrompt = v
	}
	if v, ok := cfg["model_override"].(string); ok {
		currentConfig.ModelOverride = v
	}
	if v, ok := cfg["max_tokens"].(float64); ok {
		currentConfig.MaxTokens = int(v)
	}
	if v, ok := cfg["reject_on_block"].(bool); ok {
		currentConfig.RejectOnBlock = v
	}
	if v, ok := cfg["fail_open"].(bool); ok {
		currentConfig.FailOpen = v
	}
	if v, ok := cfg["timeout_seconds"].(float64); ok {
		currentConfig.TimeoutSeconds = int(v)
	}
	if keywords, ok := cfg["block_keywords"].([]any); ok {
		for _, k := range keywords {
			if s, ok := k.(string); ok {
				currentConfig.BlockKeywords = append(currentConfig.BlockKeywords, s)
			}
		}
	}
	if v, ok := cfg["provider_base_url"].(string); ok {
		currentConfig.ProviderBaseURL = v
	}
	if v, ok := cfg["provider_api_key"].(string); ok {
		currentConfig.ProviderAPIKey = v
	}

	httpClient = &http.Client{
		Timeout: time.Duration(currentConfig.TimeoutSeconds) * time.Second,
	}

	return nil
}

// GetName returns the plugin identifier (required).
func GetName() string { return PluginName }

// Cleanup is called when the plugin is unloaded (required).
func Cleanup() error { return nil }

// ─── LLM hooks ──────────────────────────────────────────────────────────────

// PreLLMHook is called before the request is sent to the provider.
func PreLLMHook(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) (
	*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error,
) {
	pluginMu.RLock()
	cfg := currentConfig
	pluginMu.RUnlock()

	if !cfg.Enabled {
		return req, nil, nil
	}

	pluginMu.Lock()
	pluginStats.TotalRequests++
	pluginMu.Unlock()

	if req.ChatRequest == nil || len(req.ChatRequest.Input) == 0 {
		return req, nil, nil
	}

	userMsg := extractUserMessage(req.ChatRequest.Input)
	if userMsg == "" {
		recordPass()
		return req, nil, nil
	}

	if cfg.SystemPrompt == "" && len(cfg.BlockKeywords) == 0 {
		recordPass()
		return req, nil, nil
	}

	start := time.Now()
	result := runFilter(ctx, cfg, userMsg, req.ChatRequest.Model)
	elapsed := time.Since(start).Milliseconds()
	updateStats(result.Blocked, elapsed)

	if result.Blocked && cfg.RejectOnBlock {
		statusCode := 400
		errMsg := fmt.Sprintf("Request blocked by filter: %s", result.Reason)
		return nil, &schemas.LLMPluginShortCircuit{
			Error: &schemas.BifrostError{
				IsBifrostError: true,
				StatusCode:     &statusCode,
				Error: &schemas.ErrorField{
					Message: errMsg,
				},
			},
		}, nil
	}

	return req, nil, nil
}

// PostLLMHook is called after the provider responds.
func PostLLMHook(ctx *schemas.BifrostContext, resp *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) (
	*schemas.BifrostResponse, *schemas.BifrostError, error,
) {
	return resp, bifrostErr, nil
}

// ─── Filtering logic ────────────────────────────────────────────────────────

type filterResult struct {
	Blocked bool
	Reason  string
}

func runFilter(ctx *schemas.BifrostContext, cfg FilterConfig, userMessage string, reqModel string) filterResult {
	// Fast path: keyword check
	for _, kw := range cfg.BlockKeywords {
		if strings.Contains(strings.ToLower(userMessage), strings.ToLower(kw)) {
			return filterResult{
				Blocked: true,
				Reason:  fmt.Sprintf("blocked keyword: %s", kw),
			}
		}
	}

	// No LLM filter configured
	if cfg.SystemPrompt == "" {
		return filterResult{Blocked: false}
	}

	// Build filter request
	filterModel := reqModel
	if cfg.ModelOverride != "" {
		filterModel = cfg.ModelOverride
	}
	maxTokens := cfg.MaxTokens
	if maxTokens == 0 {
		maxTokens = 100
	}

	body, err := json.Marshal(filterChatRequest{
		Model: filterModel,
		Messages: []filterChatMessage{
			{Role: "system", Content: cfg.SystemPrompt},
			{Role: "user", Content: userMessage},
		},
		MaxTokens:   maxTokens,
		Temperature: 0.0,
		Stream:      false,
	})
	if err != nil {
		ctx.Log(schemas.LogLevelError, fmt.Sprintf("MCPFilter: marshal failed: %v", err))
		return filterResult{Blocked: false}
	}

	url := strings.TrimRight(cfg.ProviderBaseURL, "/") + "/chat/completions"

	// Use ctx as context.Context — BifrostContext implements it, so client
	// disconnects cancel the filter call immediately.
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		ctx.Log(schemas.LogLevelError, fmt.Sprintf("MCPFilter: NewRequest failed: %v", err))
		return filterResult{Blocked: false}
	}

	httpReq.Header.Set("Content-Type", "application/json")
	if cfg.ProviderAPIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+cfg.ProviderAPIKey)
	}

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		if ctx.Err() == context.Canceled {
			ctx.Log(schemas.LogLevelInfo, "MCPFilter: client disconnected, aborting filter")
			return filterResult{Blocked: false}
		}
		ctx.Log(schemas.LogLevelError, fmt.Sprintf("MCPFilter: HTTP failed: %v", err))
		if cfg.FailOpen {
			return filterResult{Blocked: false}
		}
		return filterResult{Blocked: true, Reason: fmt.Sprintf("filter error: %v", err)}
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		ctx.Log(schemas.LogLevelError, fmt.Sprintf("MCPFilter: read body failed: %v", err))
		if cfg.FailOpen {
			return filterResult{Blocked: false}
		}
		return filterResult{Blocked: true, Reason: "filter read error"}
	}

	if resp.StatusCode != http.StatusOK {
		ctx.Log(schemas.LogLevelError, fmt.Sprintf("MCPFilter: status %d: %s", resp.StatusCode, string(respBody)))
		if cfg.FailOpen {
			return filterResult{Blocked: false}
		}
		return filterResult{Blocked: true, Reason: fmt.Sprintf("provider error: status %d", resp.StatusCode)}
	}

	var filterResp filterChatResponse
	if err := json.Unmarshal(respBody, &filterResp); err != nil {
		ctx.Log(schemas.LogLevelError, fmt.Sprintf("MCPFilter: unmarshal failed: %v", err))
		if cfg.FailOpen {
			return filterResult{Blocked: false}
		}
		return filterResult{Blocked: true, Reason: "filter parse error"}
	}

	if len(filterResp.Choices) == 0 {
		return filterResult{Blocked: false}
	}

	response := strings.TrimSpace(strings.ToLower(filterResp.Choices[0].Message.Content))
	if strings.Contains(response, "block") || strings.Contains(response, "reject") {
		return filterResult{
			Blocked: true,
			Reason:  fmt.Sprintf("LLM filter blocked: %s", filterResp.Choices[0].Message.Content),
		}
	}

	return filterResult{Blocked: false}
}

// ─── Helpers ────────────────────────────────────────────────────────────────

func extractUserMessage(messages []schemas.ChatMessage) string {
	for _, msg := range messages {
		if msg.Role == schemas.ChatMessageRoleUser {
			if msg.Content != nil && msg.Content.ContentStr != nil {
				return *msg.Content.ContentStr
			}
		}
	}
	return ""
}

func recordPass() {
	pluginMu.Lock()
	pluginStats.TotalRequests++
	pluginStats.PassedRequests++
	pluginMu.Unlock()
}

func updateStats(blocked bool, elapsedMs int64) {
	pluginMu.Lock()
	defer pluginMu.Unlock()

	if blocked {
		pluginStats.BlockedRequests++
	} else {
		pluginStats.PassedRequests++
	}

	total := pluginStats.TotalRequests
	if total > 0 {
		pluginStats.AvgFilterTimeMs = (pluginStats.AvgFilterTimeMs*(total-1) + elapsedMs) / total
	}
}
