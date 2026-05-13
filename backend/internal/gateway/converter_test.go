package gateway

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
	"github.com/tidwall/gjson"
)

func TestMapToKiroModel(t *testing.T) {
	tests := []struct {
		input     string
		wantID    string
		wantCtx   int
		wantError bool
	}{
		{"claude-sonnet-4-6", "claude-sonnet-4.6", 1_000_000, false},
		{"claude-sonnet-4-5-20250929", "claude-sonnet-4.5", 200_000, false},
		{"claude-opus-4-6", "claude-opus-4.6", 1_000_000, false},
		{"claude-opus-4-5-20251101", "claude-opus-4.5", 200_000, false},
		{"claude-haiku-4-5-20251001", "claude-haiku-4.5", 200_000, false},
		{"gpt-4", "", 0, true},
	}

	for _, tt := range tests {
		id, ctx, err := MapToKiroModel(tt.input)
		if tt.wantError {
			if err == nil {
				t.Errorf("MapToKiroModel(%q): expected error, got nil", tt.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("MapToKiroModel(%q): unexpected error: %v", tt.input, err)
			continue
		}
		if id != tt.wantID {
			t.Errorf("MapToKiroModel(%q): got ID %q, want %q", tt.input, id, tt.wantID)
		}
		if ctx != tt.wantCtx {
			t.Errorf("MapToKiroModel(%q): got context %d, want %d", tt.input, ctx, tt.wantCtx)
		}
	}
}

func TestBuildThinkingPrefix(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		expect string
	}{
		{
			"enabled",
			`{"thinking":{"type":"enabled","budget_tokens":10000}}`,
			"<thinking_mode>enabled</thinking_mode><max_thinking_length>10000</max_thinking_length>",
		},
		{
			"enabled_capped",
			`{"thinking":{"type":"enabled","budget_tokens":999999}}`,
			"<thinking_mode>enabled</thinking_mode><max_thinking_length>102400</max_thinking_length>",
		},
		{
			"adaptive",
			`{"thinking":{"type":"adaptive","thinking_effort":"high"}}`,
			"<thinking_mode>adaptive</thinking_mode><thinking_effort>high</thinking_effort>",
		},
		{
			"disabled",
			`{"thinking":{"type":"disabled"}}`,
			"",
		},
		{
			"no_thinking",
			`{}`,
			"",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed := gjson.Parse(tt.body)
			result := buildThinkingPrefix(parsed)
			if result != tt.expect {
				t.Errorf("got %q, want %q", result, tt.expect)
			}
		})
	}
}

func TestConvertTools_NameTruncation(t *testing.T) {
	longName := strings.Repeat("a", 100)
	tools := []gjson.Result{
		gjson.Parse(`{"name":"` + longName + `","description":"test","input_schema":{"type":"object"}}`),
	}

	result, nameMap := convertTools(tools)
	if len(result) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(result))
	}

	spec := result[0].(map[string]any)["toolSpecification"].(map[string]any)
	actualName := spec["name"].(string)
	if len(actualName) > maxToolNameLen {
		t.Errorf("truncated name too long: %d chars", len(actualName))
	}

	if original, ok := nameMap[actualName]; !ok || original != longName {
		t.Error("name map not populated correctly")
	}
}

func TestConvertTools_ShortName(t *testing.T) {
	tools := []gjson.Result{
		gjson.Parse(`{"name":"read_file","description":"Read","input_schema":{"type":"object","properties":{}}}`),
	}

	result, nameMap := convertTools(tools)
	if len(result) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(result))
	}

	spec := result[0].(map[string]any)["toolSpecification"].(map[string]any)
	if spec["name"] != "read_file" {
		t.Errorf("expected 'read_file', got %v", spec["name"])
	}
	if len(nameMap) != 0 {
		t.Error("expected empty name map for short name")
	}
}

func TestConvertRequest_BasicMessage(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-6",
		"max_tokens": 1024,
		"messages": [
			{"role": "user", "content": "Hello"}
		]
	}`)

	account := &sdk.Account{
		Type:        "oauth",
		Credentials: map[string]string{"profile_arn": "arn:test"},
	}

	result, convCtx, err := convertRequest(body, account, convertConfig{ProfileArn: "arn:test"}, slog.Default())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if convCtx.KiroModelID != "claude-sonnet-4.6" {
		t.Errorf("expected model claude-sonnet-4.6, got %s", convCtx.KiroModelID)
	}

	parsed := gjson.ParseBytes(result)
	if parsed.Get("profileArn").String() != "arn:test" {
		t.Error("profileArn not injected")
	}

	currentMsg := parsed.Get("conversationState.currentMessage.userInputMessage.content").String()
	if currentMsg != "Hello" {
		t.Errorf("expected 'Hello', got %q", currentMsg)
	}

	if parsed.Get("conversationState.agentTaskType").String() != "vibe" {
		t.Error("expected agentTaskType = vibe")
	}
}

func TestConvertRequest_WithSystemPrompt(t *testing.T) {
	body := []byte(`{
		"model": "claude-haiku-4-5-20251001",
		"system": "You are helpful.",
		"messages": [
			{"role": "user", "content": "Hi"}
		]
	}`)

	account := &sdk.Account{Type: "oauth", Credentials: map[string]string{}}
	result, _, err := convertRequest(body, account, convertConfig{}, slog.Default())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	parsed := gjson.ParseBytes(result)
	history := parsed.Get("conversationState.history").Array()

	// System prompt 应编码为 history 的首对消息
	if len(history) < 2 {
		t.Fatalf("expected at least 2 history entries for system prompt, got %d", len(history))
	}

	firstUser := history[0].Get("userInputMessage.content").String()
	if firstUser != "You are helpful." {
		t.Errorf("expected system in first user message, got %q", firstUser)
	}

	firstAssistant := history[1].Get("assistantResponseMessage.content").String()
	if firstAssistant != "I will follow these instructions." {
		t.Errorf("expected acknowledgment, got %q", firstAssistant)
	}
}

func TestConvertRequest_WithThinking(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-6",
		"system": "Be helpful",
		"thinking": {"type": "enabled", "budget_tokens": 5000},
		"messages": [{"role": "user", "content": "Hi"}]
	}`)

	account := &sdk.Account{Type: "oauth", Credentials: map[string]string{}}
	result, _, err := convertRequest(body, account, convertConfig{}, slog.Default())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	parsed := gjson.ParseBytes(result)
	firstUser := parsed.Get("conversationState.history.0.userInputMessage.content").String()
	if !strings.Contains(firstUser, "<thinking_mode>enabled</thinking_mode>") {
		t.Error("thinking prefix not found in system message")
	}
	if !strings.Contains(firstUser, "Be helpful") {
		t.Error("system prompt not found")
	}
}

func TestExtractConversationID(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantUUID bool
	}{
		{
			"from_session_string",
			`{"metadata":{"user_id":"user_xxx_account_yyy_session_12345678-1234-1234-1234-123456789abc"}}`,
			true,
		},
		{
			"from_json_format",
			`{"metadata":{"user_id":"{\"session_id\":\"12345678-1234-1234-1234-123456789abc\"}"}}`,
			true,
		},
		{
			"random_fallback",
			`{}`,
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed := gjson.Parse(tt.body)
			id := extractConversationID(parsed)
			if id == "" {
				t.Error("expected non-empty conversation ID")
			}
		})
	}
}

func TestNormalizeSchema(t *testing.T) {
	input := `{"type":"object","required":null,"properties":null}`
	result := normalizeSchema(input)

	var parsed map[string]any
	json.Unmarshal([]byte(result), &parsed)

	if parsed["required"] == nil {
		t.Error("expected required to be non-null")
	}
	if parsed["properties"] == nil {
		t.Error("expected properties to be non-null")
	}
}
