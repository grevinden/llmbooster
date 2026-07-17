package main

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
)

// ---------------------------------------------------------------------------
// Tests: isEmptyMessage
// ---------------------------------------------------------------------------

func TestIsEmptyMessage_NilContent(t *testing.T) {
	msg := schemas.ChatMessage{}
	if !isEmptyMessage(msg) {
		t.Error("expected empty for nil Content")
	}
}

func TestIsEmptyMessage_NilContentStrAndNoBlocks(t *testing.T) {
	msg := schemas.ChatMessage{
		Content: &schemas.ChatMessageContent{},
	}
	if !isEmptyMessage(msg) {
		t.Error("expected empty for nil ContentStr and no ContentBlocks")
	}
}

func TestIsEmptyMessage_EmptyContentStr(t *testing.T) {
	empty := ""
	msg := schemas.ChatMessage{
		Content: &schemas.ChatMessageContent{
			ContentStr: &empty,
		},
	}
	if !isEmptyMessage(msg) {
		t.Error("expected empty for empty string ContentStr")
	}
}

func TestIsEmptyMessage_NonEmptyContentStr(t *testing.T) {
	text := "hello"
	msg := schemas.ChatMessage{
		Content: &schemas.ChatMessageContent{
			ContentStr: &text,
		},
	}
	if isEmptyMessage(msg) {
		t.Error("expected non-empty for non-empty ContentStr")
	}
}

func TestIsEmptyMessage_ContentBlocks(t *testing.T) {
	msg := schemas.ChatMessage{
		Content: &schemas.ChatMessageContent{
			ContentBlocks: []schemas.ChatContentBlock{
				{Type: schemas.ChatContentBlockTypeText, Text: ptr("hello")},
			},
		},
	}
	if isEmptyMessage(msg) {
		t.Error("expected non-empty for ContentBlocks")
	}
}

// ---------------------------------------------------------------------------
// Tests: isMultiturnRequest
// ---------------------------------------------------------------------------

func TestIsMultiturnRequest_NoTool(t *testing.T) {
	input := []schemas.ChatMessage{
		{Role: schemas.ChatMessageRoleSystem, Content: &schemas.ChatMessageContent{ContentStr: ptr("be helpful")}},
		{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: ptr("hello")}},
	}
	if isMultiturnRequest(input) {
		t.Error("expected false for single-turn request")
	}
}

func TestIsMultiturnRequest_ToolMessage(t *testing.T) {
	input := []schemas.ChatMessage{
		{Role: schemas.ChatMessageRoleSystem, Content: &schemas.ChatMessageContent{ContentStr: ptr("be helpful")}},
		{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: ptr("hello")}},
		{Role: schemas.ChatMessageRoleAssistant, Content: &schemas.ChatMessageContent{ContentStr: ptr("I'll help")}},
		{Role: schemas.ChatMessageRoleTool, Content: &schemas.ChatMessageContent{ContentStr: ptr("result")}},
		{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: ptr("thanks")}},
	}
	if !isMultiturnRequest(input) {
		t.Error("expected true when tool role present")
	}
}

func TestIsMultiturnRequest_AssistantToolCalls(t *testing.T) {
	input := []schemas.ChatMessage{
		{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: ptr("hello")}},
		{
			Role:    schemas.ChatMessageRoleAssistant,
			Content: &schemas.ChatMessageContent{ContentStr: ptr("")},
			ChatAssistantMessage: &schemas.ChatAssistantMessage{
				ToolCalls: []schemas.ChatAssistantMessageToolCall{
					{ID: ptr("call_1"), Type: ptr("function")},
				},
			},
		},
	}
	if !isMultiturnRequest(input) {
		t.Error("expected true when assistant has tool_calls")
	}
}

// ---------------------------------------------------------------------------
// Tests: containsThinkingPrompt
// ---------------------------------------------------------------------------

func TestContainsThinkingPrompt_Found(t *testing.T) {
	if !containsThinkingPrompt("prefix\n\nДумай шаг за шагом suffix", "Думай шаг за шагом") {
		t.Error("expected to find prompt in content")
	}
}

func TestContainsThinkingPrompt_NotFound(t *testing.T) {
	if containsThinkingPrompt("just a normal response", "Думай шаг за шагом") {
		t.Error("expected not to find prompt in unrelated content")
	}
}

func TestContainsThinkingPrompt_EmptyPrompt(t *testing.T) {
	if !containsThinkingPrompt("anything", "") {
		t.Error("expected true for empty prompt (nothing to add)")
	}
}

// ---------------------------------------------------------------------------
// Tests: truncateThought
// ---------------------------------------------------------------------------

func TestTruncateThought_Short(t *testing.T) {
	short := "hello world"
	result := truncateThought(short)
	if result != short {
		t.Errorf("expected no truncation, got %q", result)
	}
}

func TestTruncateThought_Long(t *testing.T) {
	long := strings.Repeat("a", maxThoughtLength+100)
	result := truncateThought(long)
	if len(result) > maxThoughtLength+100 {
		t.Error("expected truncation to reduce length")
	}
	if !strings.Contains(result, "[truncated") {
		t.Error("expected truncation notice")
	}
}

// ---------------------------------------------------------------------------
// Tests: extractRequestID
// ---------------------------------------------------------------------------

func TestExtractRequestID_Empty(t *testing.T) {
	ctx := schemas.NewBifrostContext(nil, time.Time{})
	if id := extractRequestID(ctx); id != "" {
		t.Errorf("expected empty string, got %q", id)
	}
}

func TestExtractRequestID_Present(t *testing.T) {
	ctx := schemas.NewBifrostContext(nil, time.Time{})
	ctx.SetValue(schemas.BifrostContextKeyRequestID, "req-123")
	if id := extractRequestID(ctx); id != "req-123" {
		t.Errorf("expected req-123, got %q", id)
	}
}

// ---------------------------------------------------------------------------
// Tests: modifySystemPrompt
// ---------------------------------------------------------------------------

func TestModifySystemPrompt_ExistingSystem(t *testing.T) {
	input := []schemas.ChatMessage{
		{Role: schemas.ChatMessageRoleSystem, Content: &schemas.ChatMessageContent{ContentStr: ptr("original")}},
		{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: ptr("hi")}},
	}
	result := modifySystemPrompt(input, "think step by step")
	if len(result) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result))
	}
	if result[0].Role != schemas.ChatMessageRoleSystem {
		t.Fatalf("expected first to be system")
	}
	if *result[0].Content.ContentStr != "original\n\nthink step by step" {
		t.Errorf("expected appended prompt, got %q", *result[0].Content.ContentStr)
	}
}

func TestModifySystemPrompt_NoSystem(t *testing.T) {
	input := []schemas.ChatMessage{
		{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: ptr("hi")}},
	}
	result := modifySystemPrompt(input, "think step by step")
	if len(result) != 2 {
		t.Fatalf("expected 2 messages (inserted system), got %d", len(result))
	}
	if result[0].Role != schemas.ChatMessageRoleSystem {
		t.Fatalf("expected first to be system, got %v", result[0].Role)
	}
	if *result[0].Content.ContentStr != "think step by step" {
		t.Errorf("expected thinking prompt, got %q", *result[0].Content.ContentStr)
	}
}

func TestModifySystemPrompt_DeveloperRole(t *testing.T) {
	input := []schemas.ChatMessage{
		{Role: schemas.ChatMessageRoleDeveloper, Content: &schemas.ChatMessageContent{ContentStr: ptr("be helpful")}},
		{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: ptr("hi")}},
	}
	result := modifySystemPrompt(input, "think step by step")
	if result[0].Role != schemas.ChatMessageRoleDeveloper {
		t.Fatalf("expected developer to remain, got %v", result[0].Role)
	}
	if *result[0].Content.ContentStr != "be helpful\n\nthink step by step" {
		t.Errorf("expected appended to developer, got %q", *result[0].Content.ContentStr)
	}
}

func TestModifySystemPrompt_DuplicateGuard(t *testing.T) {
	input := []schemas.ChatMessage{
		{Role: schemas.ChatMessageRoleSystem, Content: &schemas.ChatMessageContent{ContentStr: ptr("original\n\nthink step by step")}},
		{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: ptr("hi")}},
	}
	result := modifySystemPrompt(input, "think step by step")
	if *result[0].Content.ContentStr != "original\n\nthink step by step" {
		t.Errorf("expected unchanged content, got %q", *result[0].Content.ContentStr)
	}
}

// ---------------------------------------------------------------------------
// Tests: buildReplacementChunk
// ---------------------------------------------------------------------------

func TestBuildReplacementChunk_Structure(t *testing.T) {
	chunk := buildReplacementChunk("req-42", "I think therefore I am")
	if chunk == nil {
		t.Fatal("expected non-nil chunk")
	}
	if chunk.BifrostChatResponse == nil {
		t.Fatal("expected BifrostChatResponse")
	}
	if len(chunk.BifrostChatResponse.Choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(chunk.BifrostChatResponse.Choices))
	}
	ch := chunk.BifrostChatResponse.Choices[0]
	if ch.FinishReason == nil || *ch.FinishReason != "tool_calls" {
		t.Errorf("expected finish_reason=tool_calls, got %v", ch.FinishReason)
	}
	if ch.ChatStreamResponseChoice == nil || ch.ChatStreamResponseChoice.Delta == nil {
		t.Fatal("expected delta")
	}
	tcs := ch.ChatStreamResponseChoice.Delta.ToolCalls
	if len(tcs) != 1 {
		t.Fatalf("expected 1 tool_call, got %d", len(tcs))
	}
	tc := tcs[0]
	if tc.ID == nil || *tc.ID != "think_req-42" {
		t.Errorf("expected ID=think_req-42, got %v", tc.ID)
	}
	if tc.Function.Name == nil || *tc.Function.Name != "think" {
		t.Errorf("expected name=think, got %v", tc.Function.Name)
	}
	// Verify arguments is valid JSON with the thought
	var args map[string]string
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		t.Fatalf("arguments should be valid JSON: %v", err)
	}
	if args["thought"] != "I think therefore I am" {
		t.Errorf("expected thought in arguments, got %q", args["thought"])
	}
}

func TestBuildReplacementChunk_MarshalError(t *testing.T) {
	// json.Marshal should never fail for map[string]string, but if it does,
	// the fallback should produce valid JSON.
	chunk := buildReplacementChunk("req-1", "test")
	_ = chunk // just ensure no panic
}

// ---------------------------------------------------------------------------
// Tests: buildNonStreamResponse
// ---------------------------------------------------------------------------

func TestBuildNonStreamResponse_Structure(t *testing.T) {
	original := &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			ID:                "chatcmpl-abc123",
			Object:            "chat.completion",
			Created:           1234567890,
			Model:             "gpt-4",
			SystemFingerprint: "fp_abc",
			Choices: []schemas.BifrostResponseChoice{
				{
					Index:        0,
					FinishReason: ptr("stop"),
					ChatNonStreamResponseChoice: &schemas.ChatNonStreamResponseChoice{
						Message: &schemas.ChatMessage{
							Role:    schemas.ChatMessageRoleAssistant,
							Content: &schemas.ChatMessageContent{ContentStr: ptr("I think")},
						},
					},
				},
			},
		},
	}

	resp := buildNonStreamResponse(original, "req-42", "I think")
	if resp == nil || resp.ChatResponse == nil {
		t.Fatal("expected non-nil response")
	}
	if resp.ChatResponse.ID != "chatcmpl-abc123" {
		t.Errorf("expected ID to be preserved, got %q", resp.ChatResponse.ID)
	}
	if resp.ChatResponse.Object != "chat.completion" {
		t.Errorf("expected Object to be preserved")
	}
	if resp.ChatResponse.Model != "gpt-4" {
		t.Errorf("expected Model to be preserved")
	}
	if len(resp.ChatResponse.Choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(resp.ChatResponse.Choices))
	}
	ch := resp.ChatResponse.Choices[0]
	if ch.FinishReason == nil || *ch.FinishReason != "tool_calls" {
		t.Errorf("expected finish_reason=tool_calls, got %v", ch.FinishReason)
	}
	if ch.ChatNonStreamResponseChoice == nil || ch.ChatNonStreamResponseChoice.Message == nil {
		t.Fatal("expected non-stream choice with message")
	}
	if ch.ChatNonStreamResponseChoice.Message.ChatAssistantMessage == nil {
		t.Fatal("expected ChatAssistantMessage with tool_calls")
	}
	if len(ch.ChatNonStreamResponseChoice.Message.ChatAssistantMessage.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool_call, got %d", len(ch.ChatNonStreamResponseChoice.Message.ChatAssistantMessage.ToolCalls))
	}

	// Verify the original response was NOT mutated
	origMsg := original.ChatResponse.Choices[0].ChatNonStreamResponseChoice.Message
	if origMsg.Content == nil || origMsg.Content.ContentStr == nil {
		t.Error("original message Content should be preserved (not set to nil)")
	}
	if origMsg.ChatAssistantMessage != nil {
		t.Error("original message should NOT have ChatAssistantMessage")
	}
}

// ---------------------------------------------------------------------------
// Tests: buffer thread safety
// ---------------------------------------------------------------------------

func TestChunkBuffer_ClosedFlag(t *testing.T) {
	buf := &chunkBuffer{createdAt: time.Now()}

	// First read
	buf.mu.Lock()
	if buf.closed {
		t.Error("should not be closed initially")
	}
	buf.closed = true
	buf.mu.Unlock()

	// Second read should be rejected
	buf.mu.Lock()
	if !buf.closed {
		t.Error("should be closed after first read")
	}
	buf.mu.Unlock()
}

func TestChunkBuffer_ConcurrentAccess(t *testing.T) {
	buf := &chunkBuffer{createdAt: time.Now()}
	var wg sync.WaitGroup

	// Concurrent writes
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			buf.mu.Lock()
			if !buf.closed {
				buf.content.WriteString("data")
			}
			buf.mu.Unlock()
		}(i)
	}

	// Concurrent read + close
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf.mu.Lock()
		_ = buf.content.String()
		buf.closed = true
		buf.mu.Unlock()
	}()

	wg.Wait()
}

// ---------------------------------------------------------------------------
// Tests: thinkTracker sync.Map
// ---------------------------------------------------------------------------

func TestThinkTracker_CreateAndDelete(t *testing.T) {
	// Clean start
	thinkTracker = sync.Map{}

	buf := getOrCreateBuffer("test-1")
	if buf == nil {
		t.Fatal("expected non-nil buffer")
	}

	buf.mu.Lock()
	buf.content.WriteString("hello")
	buf.mu.Unlock()

	// Same key returns same buffer
	buf2 := getOrCreateBuffer("test-1")
	buf2.mu.Lock()
	content := buf2.content.String()
	buf2.mu.Unlock()
	if content != "hello" {
		t.Errorf("expected to share buffer content, got %q", content)
	}

	deleteBuffer("test-1")

	// After delete, a new buffer is created
	buf3 := getOrCreateBuffer("test-1")
	buf3.mu.Lock()
	content3 := buf3.content.String()
	buf3.mu.Unlock()
	if content3 != "" {
		t.Errorf("expected new empty buffer, got %q", content3)
	}
}
