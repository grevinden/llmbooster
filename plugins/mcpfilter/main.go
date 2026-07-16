package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
)

const PluginName = "mcpfilter"

type FilterConfig struct {
	Enabled      bool   `json:"enabled"`
	SystemPrompt string `json:"system_prompt"`
	TimeoutSecs  int    `json:"timeout_seconds,omitempty"`
}

var (
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

	currentConfig = FilterConfig{
		TimeoutSecs: 10,
	}
	currentConfig.Enabled, _ = cfg["enabled"].(bool)
	currentConfig.SystemPrompt, _ = cfg["system_prompt"].(string)
	if v, ok := cfg["timeout_seconds"].(float64); ok {
		currentConfig.TimeoutSecs = int(v)
	}

	httpClient = &http.Client{
		Timeout: time.Duration(currentConfig.TimeoutSecs) * time.Second,
	}
	return nil
}

func GetName() string { return PluginName }
func Cleanup() error  { return nil }

func PreLLMHook(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) (
	*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error,
) {
	if !currentConfig.Enabled || currentConfig.SystemPrompt == "" {
		return req, nil, nil
	}
	if req.ChatRequest == nil || len(req.ChatRequest.Input) == 0 {
		return req, nil, nil
	}

	// Remove trailing empty messages
	for len(req.ChatRequest.Input) > 0 {
		last := req.ChatRequest.Input[len(req.ChatRequest.Input)-1]
		if last.Content == nil || last.Content.ContentStr == nil || *last.Content.ContentStr == "" {
			req.ChatRequest.Input = req.ChatRequest.Input[:len(req.ChatRequest.Input)-1]
		} else {
			break
		}
	}
	if len(req.ChatRequest.Input) == 0 {
		return req, nil, nil
	}

	// Last message must be from user
	lastMsg := req.ChatRequest.Input[len(req.ChatRequest.Input)-1]
	if lastMsg.Role != schemas.ChatMessageRoleUser {
		return req, nil, nil
	}
	userText := *lastMsg.Content.ContentStr

	enhanced := enhance(ctx, userText, req.ChatRequest)
	if enhanced == "" {
		return req, nil, nil
	}

	// Set system message: update existing or prepend new
	found := false
	for i := range req.ChatRequest.Input {
		if req.ChatRequest.Input[i].Role == schemas.ChatMessageRoleSystem {
			if req.ChatRequest.Input[i].Content == nil {
				req.ChatRequest.Input[i].Content = &schemas.ChatMessageContent{}
			}
			req.ChatRequest.Input[i].Content.ContentStr = &enhanced
			found = true
			break
		}
	}
	if !found {
		sysMsg := schemas.ChatMessage{
			Role:    schemas.ChatMessageRoleSystem,
			Content: &schemas.ChatMessageContent{ContentStr: &enhanced},
		}
		req.ChatRequest.Input = append([]schemas.ChatMessage{sysMsg}, req.ChatRequest.Input...)
	}

	return req, nil, nil
}

func PostLLMHook(ctx *schemas.BifrostContext, resp *schemas.BifrostResponse, err *schemas.BifrostError) (
	*schemas.BifrostResponse, *schemas.BifrostError, error,
) {
	return resp, err, nil
}

func enhance(ctx *schemas.BifrostContext, userMsg string, req *schemas.BifrostChatRequest) string {
	model := req.Model

	// Get provider config from Bifrost context
	provider := req.Provider

	body, err := json.Marshal(chatRequest{
		Model: model,
		Messages: []chatMessage{
			{Role: "system", Content: currentConfig.SystemPrompt},
			{Role: "user", Content: userMsg},
		},
		MaxTokens:   1024,
		Temperature: 0.3,
	})
	if err != nil {
		return ""
	}

	// Use Bifrost's built-in provider URL construction
	baseURL := getProviderBaseURL(provider)

	url := strings.TrimRight(baseURL, "/") + "/chat/completions"

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return ""
	}
	httpReq.Header.Set("Content-Type", "application/json")

	// Get API key from Bifrost context
	apiKey := getProviderAPIKey(ctx, provider)

	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		if ctx.Err() == context.Canceled {
			ctx.Log(schemas.LogLevelInfo, "MCPFilter: client disconnected, skipping enhancement")
		}
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		ctx.Log(schemas.LogLevelError, fmt.Sprintf("MCPFilter: status %d: %s", resp.StatusCode, string(b)))
		return ""
	}

	var result chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return ""
	}
	if len(result.Choices) == 0 {
		return ""
	}

	return strings.TrimSpace(result.Choices[0].Message.Content)
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
// For now, returns empty string - plugin will use environment variables or default auth
func getProviderAPIKey(ctx *schemas.BifrostContext, provider schemas.ModelProvider) string {
	return ""
}
