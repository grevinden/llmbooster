package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
)

// ---------------------------------------------------------------------------
// Plugin metadata (required by DynamicPlugin loader)
// ---------------------------------------------------------------------------

const pluginName = "llmboster"

// Context keys — custom, not reserved by Bifrost internals.
const (
	thinkModeKey      schemas.BifrostContextKey = "llmboster-think-mode"
	requestIDKey      schemas.BifrostContextKey = "llmboster-request-id"
	requestTypeKey    schemas.BifrostContextKey = "llmboster-request-type"
	thinkActivatedKey schemas.BifrostContextKey = "llmboster-think-activated"
)

// Limits
const (
	maxBufferSize    = 100 * 1024 // 100KB max accumulated thought per buffer
	maxThoughtLength = 4 * 1024   // 4KB max thought in tool_call arguments
	bufferTTL        = 5 * time.Minute
	cleanupInterval  = 30 * time.Second
)

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

type llmbosterConfig struct {
	Prompt string `json:"prompt"`
}

var pluginCfg llmbosterConfig

// Build info, injected via ldflags (see Makefile).
var (
	buildCommit string
	buildTime   string
)

func Init(config any) error {
	if config != nil {
		switch v := config.(type) {
		case []byte:
			if err := json.Unmarshal(v, &pluginCfg); err != nil {
				return fmt.Errorf("llmboster Init: %w", err)
			}
		case string:
			if err := json.Unmarshal([]byte(v), &pluginCfg); err != nil {
				return fmt.Errorf("llmboster Init: %w", err)
			}
		default:
			data, err := json.Marshal(v)
			if err != nil {
				return fmt.Errorf("llmboster Init: marshal config: %w", err)
			}
			if err := json.Unmarshal(data, &pluginCfg); err != nil {
				return fmt.Errorf("llmboster Init: %w", err)
			}
		}
	}

	if pluginCfg.Prompt == "" {
		pluginCfg.Prompt = "Думай шаг за шагом, запиши свои рассуждения, а затем дай финальный ответ."
	}

	return nil
}

func GetName() string {
	return pluginName
}

func Cleanup() error {
	deleteBufferAll()
	return nil
}

// ---------------------------------------------------------------------------
// State management — thinkTracker (global sync.Map)
// ---------------------------------------------------------------------------

var thinkTracker sync.Map // key: RequestID (string), value: *chunkBuffer

type chunkBuffer struct {
	mu        sync.Mutex
	content   strings.Builder
	closed    bool // true when the buffer has been consumed (prevents double-processing)
	createdAt time.Time
}

func getOrCreateBuffer(reqID string) *chunkBuffer {
	v, _ := thinkTracker.LoadOrStore(reqID, &chunkBuffer{createdAt: time.Now()})
	return v.(*chunkBuffer)
}

func deleteBuffer(reqID string) {
	thinkTracker.Delete(reqID)
}

func deleteBufferAll() {
	thinkTracker.Range(func(key, value any) bool {
		thinkTracker.Delete(key)
		return true
	})
}

// ---------------------------------------------------------------------------
// Stale buffer cleanup (prevent memory leaks from aborted streams)
// ---------------------------------------------------------------------------

var (
	lastCleanupMu   sync.Mutex
	lastCleanupTime time.Time
)

func cleanupStaleBuffers() {
	lastCleanupMu.Lock()
	if time.Since(lastCleanupTime) < cleanupInterval {
		lastCleanupMu.Unlock()
		return
	}
	lastCleanupTime = time.Now()
	lastCleanupMu.Unlock()

	now := time.Now()
	thinkTracker.Range(func(key, value any) bool {
		buf := value.(*chunkBuffer)
		buf.mu.Lock()
		stale := !buf.closed && now.Sub(buf.createdAt) > bufferTTL
		buf.mu.Unlock()
		if stale {
			thinkTracker.Delete(key)
		}
		return true
	})
}

// ---------------------------------------------------------------------------
// Helper: ptr
// ---------------------------------------------------------------------------

func ptr[T any](v T) *T {
	return &v
}

// ---------------------------------------------------------------------------
// Helper: isEmptyMessage
// ---------------------------------------------------------------------------

func isEmptyMessage(msg schemas.ChatMessage) bool {
	if msg.Content == nil {
		return true
	}
	if msg.Content.ContentStr != nil && *msg.Content.ContentStr != "" {
		return false
	}
	if len(msg.Content.ContentBlocks) > 0 {
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// Helper: extractRequestID
// ---------------------------------------------------------------------------

func extractRequestID(ctx *schemas.BifrostContext) string {
	v := ctx.Value(schemas.BifrostContextKeyRequestID)
	if v == nil {
		return ""
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return ""
	}
	return s
}

// ---------------------------------------------------------------------------
// Helper: isMultiturnRequest — detects if this is a follow-up turn (tool
// messages or assistant tool_calls already in history).
// ---------------------------------------------------------------------------

func isMultiturnRequest(input []schemas.ChatMessage) bool {
	for _, msg := range input {
		if msg.Role == schemas.ChatMessageRoleTool {
			return true
		}
		if msg.Role == schemas.ChatMessageRoleAssistant &&
			msg.ChatAssistantMessage != nil &&
			len(msg.ChatAssistantMessage.ToolCalls) > 0 {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Helper: containsThinkingPrompt — checks if the prompt was already appended
// (protection against fallback re-execution).
// ---------------------------------------------------------------------------

func containsThinkingPrompt(content string, prompt string) bool {
	if prompt == "" {
		return true
	}
	return strings.Contains(content, prompt)
}

// ---------------------------------------------------------------------------
// Helper: truncateThought — limits thought length to prevent oversized
// tool_call arguments.
// ---------------------------------------------------------------------------

func truncateThought(thought string) string {
	if len(thought) <= maxThoughtLength {
		return thought
	}
	truncated := thought[:maxThoughtLength]
	return truncated + "\n\n[truncated: thought exceeded " + fmt.Sprint(maxThoughtLength) + " characters]"
}

// ---------------------------------------------------------------------------
// PreLLMHook — modify system prompt and set context flags
// ---------------------------------------------------------------------------

func PreLLMHook(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) (*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error) {
	// 1. Only chat request types.
	if req == nil || req.ChatRequest == nil {
		return req, nil, nil
	}

	reqType := req.RequestType
	if reqType != schemas.ChatCompletionRequest && reqType != schemas.ChatCompletionStreamRequest {
		return req, nil, nil
	}

	input := req.ChatRequest.Input
	if len(input) == 0 {
		return req, nil, nil
	}

	// P0: Validate RequestID — refuse to activate without a valid ID.
	reqID := extractRequestID(ctx)
	if reqID == "" {
		ctx.Log(schemas.LogLevelError, "llmboster: PreLLMHook skipped — no RequestID available")
		return req, nil, nil
	}

	// Skip multi-turn requests (tool calls already in history).
	if isMultiturnRequest(input) {
		ctx.Log(schemas.LogLevelDebug, "llmboster: PreLLMHook skipped — multi-turn request detected")
		return req, nil, nil
	}

	// 2. Remove empty messages from Input.
	filtered := make([]schemas.ChatMessage, 0, len(input))
	for _, msg := range input {
		if isEmptyMessage(msg) {
			continue
		}
		filtered = append(filtered, msg)
	}
	req.ChatRequest.Input = filtered

	if len(filtered) == 0 {
		return req, nil, nil
	}

	// 3. Check if last message is from user.
	lastMsg := filtered[len(filtered)-1]
	if lastMsg.Role != schemas.ChatMessageRoleUser {
		return req, nil, nil
	}

	// Set context flags.
	ctx.SetValue(thinkModeKey, true)
	ctx.SetValue(requestIDKey, reqID)
	ctx.SetValue(requestTypeKey, string(reqType))

	// 4. Modify system prompt (with fallback-duplication guard).
	req.ChatRequest.Input = modifySystemPrompt(filtered, pluginCfg.Prompt)

	ctx.SetValue(thinkActivatedKey, true)
	ctx.Log(schemas.LogLevelInfo, "llmboster: think mode activated for request "+reqID)

	return req, nil, nil
}

// ---------------------------------------------------------------------------
// modifySystemPrompt — appends thinkingPrompt to the first system or developer
// message. Creates a new system message if none exists.
// ---------------------------------------------------------------------------

func modifySystemPrompt(input []schemas.ChatMessage, thinkingPrompt string) []schemas.ChatMessage {
	systemIdx := -1
	for i, msg := range input {
		if msg.Role == schemas.ChatMessageRoleSystem || msg.Role == schemas.ChatMessageRoleDeveloper {
			systemIdx = i
			break
		}
	}

	if systemIdx >= 0 {
		content := input[systemIdx].Content
		if content != nil {
			if content.ContentStr != nil {
				// Guard: prevent double addition on fallback re-execution.
				if containsThinkingPrompt(*content.ContentStr, thinkingPrompt) {
					return input
				}
				*content.ContentStr = *content.ContentStr + "\n\n" + thinkingPrompt
			} else if len(content.ContentBlocks) > 0 {
				found := false
				for i := range content.ContentBlocks {
					if content.ContentBlocks[i].Type == schemas.ChatContentBlockTypeText && content.ContentBlocks[i].Text != nil {
						if containsThinkingPrompt(*content.ContentBlocks[i].Text, thinkingPrompt) {
							return input
						}
						*content.ContentBlocks[i].Text = *content.ContentBlocks[i].Text + "\n\n" + thinkingPrompt
						found = true
						break
					}
				}
				if !found {
					newBlock := schemas.ChatContentBlock{
						Type: schemas.ChatContentBlockTypeText,
						Text: ptr(thinkingPrompt),
					}
					content.ContentBlocks = append([]schemas.ChatContentBlock{newBlock}, content.ContentBlocks...)
				}
			} else {
				content.ContentStr = ptr(thinkingPrompt)
			}
		} else {
			input[systemIdx].Content = &schemas.ChatMessageContent{ContentStr: ptr(thinkingPrompt)}
		}
	} else {
		// Create new system message at the beginning.
		sysMsg := schemas.ChatMessage{
			Role:    schemas.ChatMessageRoleSystem,
			Content: &schemas.ChatMessageContent{ContentStr: ptr(thinkingPrompt)},
		}
		input = append([]schemas.ChatMessage{sysMsg}, input...)
	}

	return input
}

// ---------------------------------------------------------------------------
// HTTPTransportStreamChunkHook — accumulate chunks and replace last one
// ---------------------------------------------------------------------------

func HTTPTransportStreamChunkHook(ctx *schemas.BifrostContext, req *schemas.HTTPRequest, chunk *schemas.BifrostStreamChunk) (*schemas.BifrostStreamChunk, error) {
	// 1. Check think-mode flag.
	if ctx == nil || chunk == nil {
		return chunk, nil
	}

	thinkMode, ok := ctx.Value(thinkModeKey).(bool)
	if !ok || !thinkMode {
		return chunk, nil
	}

	// Periodic cleanup of stale buffers (memory leak prevention).
	cleanupStaleBuffers()

	// Only process chat responses.
	if chunk.BifrostChatResponse == nil {
		return chunk, nil
	}

	choices := chunk.BifrostChatResponse.Choices
	if len(choices) == 0 || choices[0].ChatStreamResponseChoice == nil {
		return chunk, nil
	}

	// 2. Get RequestID.
	reqID, _ := ctx.Value(requestIDKey).(string)
	if reqID == "" {
		ctx.Log(schemas.LogLevelWarn, "llmboster: HTTPTransportStreamChunkHook — using \"unknown\" for requestID")
		reqID = "unknown"
	}

	// 3. Get/create buffer.
	buf := getOrCreateBuffer(reqID)

	delta := choices[0].ChatStreamResponseChoice.Delta
	finishReason := choices[0].FinishReason

	// 5. Accumulate content when not finished.
	// Handle nil, empty string, and "null" consistently.
	if finishReason == nil || *finishReason == "" || *finishReason == "null" {
		if delta != nil && delta.Content != nil {
			buf.mu.Lock()
			if !buf.closed && buf.content.Len() < maxBufferSize {
				buf.content.WriteString(*delta.Content)
			}
			buf.mu.Unlock()
		}
		return chunk, nil
	}

	finishReasonStr := *finishReason

	// 6. Last chunk — replace with tool_calls.
	if finishReasonStr == "stop" {
		// Accumulate any remaining content from the last delta.
		if delta != nil && delta.Content != nil {
			buf.mu.Lock()
			if !buf.closed && buf.content.Len() < maxBufferSize {
				buf.content.WriteString(*delta.Content)
			}
			buf.mu.Unlock()
		}

		// Atomically read thought and mark buffer as closed.
		buf.mu.Lock()
		if buf.closed {
			// Already processed by another path (e.g., retry).
			buf.mu.Unlock()
			return chunk, nil
		}
		thought := buf.content.String()
		buf.closed = true
		buf.mu.Unlock()

		// Truncate if too long.
		thought = truncateThought(thought)

		// Build the replacement chunk.
		newChunk := buildReplacementChunk(reqID, thought)

		// Clean up buffer.
		deleteBuffer(reqID)

		ctx.Log(schemas.LogLevelDebug, "llmboster: replaced streaming last chunk with tool_calls for request "+reqID)

		return newChunk, nil
	}

	// 7. Other finish reasons — cleanup and pass through.
	if finishReasonStr == "tool_calls" || finishReasonStr == "length" {
		buf.mu.Lock()
		buf.closed = true
		buf.mu.Unlock()
		deleteBuffer(reqID)
	}

	return chunk, nil
}

func buildReplacementChunk(reqID string, thought string) *schemas.BifrostStreamChunk {
	toolCallID := "think_" + reqID
	funcName := "think"

	argsMap := map[string]string{"thought": thought}
	argsBytes, err := json.Marshal(argsMap)
	if err != nil {
		argsBytes = []byte(`{"thought":"[marshal error]","error":"` + err.Error() + `"}`)
	}
	argsJSON := string(argsBytes)

	newChunk := &schemas.BifrostStreamChunk{
		BifrostChatResponse: &schemas.BifrostChatResponse{
			Object: "chat.completion.chunk",
			Choices: []schemas.BifrostResponseChoice{
				{
					Index:        0,
					FinishReason: ptr("tool_calls"),
					ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{
						Delta: &schemas.ChatStreamResponseChoiceDelta{
							ToolCalls: []schemas.ChatAssistantMessageToolCall{
								{
									Index: 0,
									ID:    ptr(toolCallID),
									Type:  ptr("function"),
									Function: schemas.ChatAssistantMessageToolCallFunction{
										Name:      ptr(funcName),
										Arguments: argsJSON,
									},
								},
							},
						},
					},
				},
			},
		},
	}

	return newChunk
}

// ---------------------------------------------------------------------------
// PostLLMHook — replace non-streaming response with tool_calls
// ---------------------------------------------------------------------------

func PostLLMHook(ctx *schemas.BifrostContext, resp *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError, error) {
	// 1. Check think-mode flag.
	if ctx == nil || resp == nil {
		return resp, bifrostErr, nil
	}

	thinkMode, ok := ctx.Value(thinkModeKey).(bool)
	if !ok || !thinkMode {
		return resp, bifrostErr, nil
	}

	// 2. Only non-streaming chat requests.
	reqTypeRaw := ctx.Value(requestTypeKey)
	if reqTypeRaw == nil {
		return resp, bifrostErr, nil
	}

	reqTypeStr, ok := reqTypeRaw.(string)
	if !ok || reqTypeStr != string(schemas.ChatCompletionRequest) {
		// Streaming handled by HTTPTransportStreamChunkHook; skip.
		return resp, bifrostErr, nil
	}

	// 3. If there's an error, pass through.
	if bifrostErr != nil {
		return resp, bifrostErr, nil
	}

	// Only process chat responses.
	if resp.ChatResponse == nil {
		return resp, bifrostErr, nil
	}

	choices := resp.ChatResponse.Choices
	if len(choices) == 0 || choices[0].ChatNonStreamResponseChoice == nil {
		return resp, bifrostErr, nil
	}

	finishReason := choices[0].FinishReason
	if finishReason == nil || *finishReason != "stop" {
		return resp, bifrostErr, nil
	}

	msg := choices[0].ChatNonStreamResponseChoice.Message
	if msg == nil || msg.Role != schemas.ChatMessageRoleAssistant {
		return resp, bifrostErr, nil
	}

	// 6. If already has tool_calls — skip.
	if msg.ChatAssistantMessage != nil && len(msg.ChatAssistantMessage.ToolCalls) > 0 {
		return resp, bifrostErr, nil
	}

	// 7. Extract thought text from message content.
	thought := ""
	if msg.Content != nil && msg.Content.ContentStr != nil {
		thought = *msg.Content.ContentStr
	}

	// Truncate if too long.
	thought = truncateThought(thought)

	reqID, _ := ctx.Value(requestIDKey).(string)
	if reqID == "" {
		reqID = "unknown"
	}

	// 8. Build a NEW response (do NOT mutate the original — prevents side
	//    effects in other hooks, logging, and governance).
	newResp := buildNonStreamResponse(resp, reqID, thought)

	ctx.Log(schemas.LogLevelInfo, "llmboster: replaced non-streaming response with tool_calls for request "+reqID)

	return newResp, nil, nil
}

func buildNonStreamResponse(original *schemas.BifrostResponse, reqID string, thought string) *schemas.BifrostResponse {
	toolCallID := "think_" + reqID
	funcName := "think"

	argsMap := map[string]string{"thought": thought}
	argsBytes, err := json.Marshal(argsMap)
	if err != nil {
		argsBytes = []byte(`{"thought":"[marshal error]","error":"` + err.Error() + `"}`)
	}
	argsJSON := string(argsBytes)

	return &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			ID:                original.ChatResponse.ID,
			Object:            original.ChatResponse.Object,
			Created:           original.ChatResponse.Created,
			Model:             original.ChatResponse.Model,
			SystemFingerprint: original.ChatResponse.SystemFingerprint,
			ServiceTier:       original.ChatResponse.ServiceTier,
			Speed:             original.ChatResponse.Speed,
			InferenceGeo:      original.ChatResponse.InferenceGeo,
			Usage:             original.ChatResponse.Usage,
			ExtraFields:       original.ChatResponse.ExtraFields,
			Choices: []schemas.BifrostResponseChoice{
				{
					Index:        0,
					FinishReason: ptr("tool_calls"),
					ChatNonStreamResponseChoice: &schemas.ChatNonStreamResponseChoice{
						Message: &schemas.ChatMessage{
							Role: schemas.ChatMessageRoleAssistant,
							ChatAssistantMessage: &schemas.ChatAssistantMessage{
								ToolCalls: []schemas.ChatAssistantMessageToolCall{
									{
										Index: 0,
										ID:    ptr(toolCallID),
										Type:  ptr("function"),
										Function: schemas.ChatAssistantMessageToolCallFunction{
											Name:      ptr(funcName),
											Arguments: argsJSON,
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

// ---------------------------------------------------------------------------
// HTTPTransportPostHook — cleanup orphaned buffers
// Handles the case where a stream was aborted without a clean finish_reason.
// ---------------------------------------------------------------------------

func HTTPTransportPostHook(ctx *schemas.BifrostContext, req *schemas.HTTPRequest, resp *schemas.HTTPResponse) error {
	if ctx == nil {
		return nil
	}

	thinkMode, ok := ctx.Value(thinkModeKey).(bool)
	if !ok || !thinkMode {
		return nil
	}

	reqID, _ := ctx.Value(requestIDKey).(string)
	if reqID == "" {
		return nil
	}

	// Check if buffer is still open (orphaned — stream ended without cleanup).
	v, loaded := thinkTracker.Load(reqID)
	if loaded {
		buf := v.(*chunkBuffer)
		buf.mu.Lock()
		isOrphan := !buf.closed
		buf.mu.Unlock()
		if isOrphan {
			deleteBuffer(reqID)
			ctx.Log(schemas.LogLevelWarn, "llmboster: cleaned up orphaned buffer for request "+reqID)
		}
	}

	return nil
}
