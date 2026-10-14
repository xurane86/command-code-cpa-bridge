// Package openai implements the CommandCode Chat Completions
// protocol adapter for routes served via /v1/chat/completions:
// request conversion, non-stream response conversion,
// and SSE stream conversion.
package openai

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"command-code-cpa-bridge/internal/fault"
	"command-code-cpa-bridge/internal/reasoning"
	"command-code-cpa-bridge/internal/registry"
	"command-code-cpa-bridge/internal/translate/common"
)

// EndpointPath is the upstream CommandCode Chat Completions endpoint.
var EndpointPath = registry.RouteChatCompletions.EndpointPath()

// AuthHeaders returns the authorization headers for a CommandCode key.
func AuthHeaders(key string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+key)
	return h
}

// BuildRequest translates an inbound request body from sourceFormat
// ("openai" Chat Completions, "openai-response" Responses, "claude"
// Messages) into a Chat Completions body for upstreamModel.
// ts carries the target model's thinking capability so reasoning
// budgets map to a supported effort level instead of being silently
// degraded. Unknown formats are ClassUnsupported; malformed
// input is ClassTranslation. Errors are descriptive and redacted — no
// silent loss of tools or reasoning controls.
func BuildRequest(upstreamModel, sourceFormat string, sourceBody []byte, ts *pluginapi.ThinkingSupport) ([]byte, *fault.Error) {
	switch sourceFormat {
	case "openai":
		return buildOpenAIRequest(upstreamModel, sourceBody)
	case "claude":
		return claudeToChat(upstreamModel, sourceBody, ts)
	case "openai-response":
		return responsesToChat(upstreamModel, sourceBody, ts)
	default:
		return nil, common.UnsupportedFormat(sourceFormat, EndpointPath)
	}
}

// buildOpenAIRequest rewrites the top-level model field of an OpenAI request
// body to upstreamModel and strips any malformed top-level thinking object.
// DeepSeek models fail if thinking lacks a valid string type field.
func buildOpenAIRequest(upstreamModel string, body []byte) ([]byte, *fault.Error) {
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fault.Translation("malformed openai request JSON: " + err.Error())
	}
	if req == nil {
		return nil, fault.Translation("malformed request body: JSON null is not a valid request")
	}
	rawThinking, hasThinking := req["thinking"]
	validThinking := hasThinking && isValidThinking(rawThinking)
	if hasThinking && !validThinking {
		delete(req, "thinking")
	}
	if raw, ok := req["model"]; ok && string(raw) == `"`+upstreamModel+`"` && (validThinking || !hasThinking) {
		return body, nil
	}
	req["model"] = json.RawMessage(`"` + upstreamModel + `"`)
	b, err := json.Marshal(req)
	if err != nil {
		return nil, fault.Translation("model id cannot be represented as JSON")
	}
	return b, nil
}

// isValidThinking reports whether raw is a JSON object with a non-empty string "type" field.
func isValidThinking(raw json.RawMessage) bool {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return false
	}
	rawType, ok := obj["type"]
	if !ok {
		return false
	}
	var typeStr string
	if err := json.Unmarshal(rawType, &typeStr); err != nil {
		return false
	}
	return strings.TrimSpace(typeStr) != ""
}

type imageURLField struct {
	URL string `json:"url"`
}

type ccContentPart struct {
	Type     string         `json:"type"`
	Text     string         `json:"text,omitempty"`
	ImageURL *imageURLField `json:"image_url,omitempty"`
}

type ccMessage struct {
	Role       string              `json:"role"`
	Content    any                 `json:"content"` // string, []ccContentPart, or nil
	ToolCalls  []common.CCToolCall `json:"tool_calls,omitempty"`
	ToolCallID string              `json:"tool_call_id,omitempty"`
}

type ccRequest struct {
	Model             string          `json:"model"`
	Messages          []ccMessage     `json:"messages"`
	MaxTokens         *int64          `json:"max_tokens,omitempty"`
	Stop              any             `json:"stop,omitempty"`
	Stream            bool            `json:"stream,omitempty"`
	Temperature       *float64        `json:"temperature,omitempty"`
	TopP              *float64        `json:"top_p,omitempty"`
	ReasoningEffort   string          `json:"reasoning_effort,omitempty"`
	Tools             []common.CCTool `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
}

// encode finalizes and encodes a translated request; the composed types
// are always marshallable.
func encode(req *ccRequest) []byte {
	if req.Messages == nil {
		req.Messages = []ccMessage{}
	}
	b, _ := json.Marshal(req)
	return b
}

// ---- claude (Anthropic Messages) -> Chat Completions ----

type claudeBlock = map[string]any

// claudeToChat translates an Anthropic Messages request into a Chat
// Completions request: top-level system becomes a system
// message, text blocks become string content, tool_use blocks become
// assistant tool_calls, tool_result blocks become role:"tool" messages,
// max_tokens/stop_sequences/tools map to their CC equivalents
// (max_tokens defaulted by the shared kernel,), and the thinking
// budget maps to a capability-aware reasoning_effort via
// reasoning.EffortFromBudget. Decoding is owned entirely by the shared
// Claude-request kernel; only target-shape rendering stays local.
func claudeToChat(upstreamModel string, body []byte, ts *pluginapi.ThinkingSupport) ([]byte, *fault.Error) {
	src, eErr := common.DecodeClaudeMessages(body)
	if eErr != nil {
		return nil, eErr
	}
	out := &ccRequest{
		Model:       upstreamModel,
		Stream:      src.Stream,
		Temperature: src.Temperature,
		TopP:        src.TopP,
	}
	out.MaxTokens = &src.MaxTokens
	if len(src.StopSequences) > 0 {
		out.Stop = src.StopSequences
	}
	if common.ThinkingEnabled(src.Thinking) {
		out.ReasoningEffort = reasoning.EffortFromBudget(src.Thinking.BudgetTokens, ts)
	}
	applyToolChoiceCC(out, src.ToolChoiceKind, src.ToolChoiceName)
	if src.System != "" {
		out.Messages = append(out.Messages, ccMessage{Role: "system", Content: src.System})
	}
	for i := range src.Messages {
		m := &src.Messages[i]
		switch m.Role {
		case "user":
			msgs, eErr := claudeUserMessages(m)
			if eErr != nil {
				return nil, eErr
			}
			out.Messages = append(out.Messages, msgs...)
		case "assistant":
			msg, eErr := claudeAssistantMessage(m)
			if eErr != nil {
				return nil, eErr
			}
			if msg != nil {
				out.Messages = append(out.Messages, *msg)
			}
		default:
			return nil, common.ValidateRole(m.Role, EndpointPath)
		}
	}
	for _, t := range src.Tools {
		// Claude tools carry no type field; validation cannot reject here.
		schema := common.ObjectSchema(t.InputSchema)
		out.Tools = append(out.Tools, common.CCTool{
			Type:     "function",
			Function: common.CCFunction{Name: t.Name, Description: t.Description, Parameters: schema},
		})
	}
	return encode(out), nil
}

// claudeResultText flattens tool_result content (string or text blocks)
// into a tool-message string via the shared kernel; non-text blocks have
// no tool-message equivalent and are rejected descriptively rather than
// dropped.
func claudeResultText(content json.RawMessage) (string, *fault.Error) {
	return common.ToolResultText(content, "tool messages carry text only")
}

// claudeUserMessages converts a user turn into Chat Completions messages:
// text/image blocks become one user message (plain string when only
// text), each tool_result block becomes a separate role:"tool" message
// carrying its tool_use_id.
func claudeUserMessages(m *common.ClaudeMessageRecord) ([]ccMessage, *fault.Error) {
	var msgs []ccMessage
	var parts []ccContentPart
	flush := func() {
		if len(parts) == 0 {
			return
		}
		var c any = parts
		if len(parts) == 1 && parts[0].Type == "text" {
			c = parts[0].Text
		}
		msgs = append(msgs, ccMessage{Role: "user", Content: c})
		parts = nil
	}
	if m.Content != "" {
		parts = append(parts, ccContentPart{Type: "text", Text: m.Content})
	}
	for i := range m.Blocks {
		blk := &m.Blocks[i]
		switch blk.Kind {
		case "text":
			parts = append(parts, ccContentPart{Type: "text", Text: blk.Text})
		case "image":
			parts = append(parts, ccContentPart{Type: "image_url", ImageURL: &imageURLField{URL: blk.URL}})
		case "tool_result":
			text, eErr := claudeResultText(blk.Result)
			if eErr != nil {
				return nil, eErr
			}
			if blk.IsError {
				// Same convention as the Responses-route twin so the
				// flag is never silently erased.
				text = common.ToolResultErrorPrefix + text
			}
			flush()
			msgs = append(msgs, ccMessage{
				Role: "tool", Content: text, ToolCallID: blk.CallID,
			})
		default:
			return nil, common.UnsupportedPartType(blk.Kind, EndpointPath)
		}
	}
	flush()
	return msgs, nil
}

// claudeAssistantMessage converts an assistant turn: text blocks join
// the message content, tool_use blocks become tool_calls. Historical
// thinking and redacted_thinking blocks have no Chat Completions
// representation and are omitted by the explicit omission policy
// (the forward-looking control maps via effortFromBudget instead; the
// redacted variant is pure encrypted metadata). Returns nil for empty turns.
func claudeAssistantMessage(m *common.ClaudeMessageRecord) (*ccMessage, *fault.Error) {
	msg := &ccMessage{Role: "assistant"}
	var sb strings.Builder
	sb.WriteString(m.Content)
	for i := range m.Blocks {
		blk := &m.Blocks[i]
		switch blk.Kind {
		case "text":
			sb.WriteString(blk.Text)
		case "thinking", "redacted_thinking":
			// omitted: no Chat Completions equivalent (policy)
		case "tool_use":
			tc := common.CCToolCall{ID: blk.CallID, Type: "function"}
			tc.Function.Name = blk.Name
			tc.Function.Arguments = common.DefaultArgs(string(blk.Input))
			msg.ToolCalls = append(msg.ToolCalls, tc)
		default:
			return nil, common.UnsupportedPartType(blk.Kind, EndpointPath)
		}
	}
	if sb.Len() > 0 {
		msg.Content = sb.String()
	}
	if msg.Content == nil && len(msg.ToolCalls) == 0 {
		return nil, nil
	}
	return msg, nil
}

// ---- openai-response (Responses) -> Chat Completions ----

// responsesToChat translates an OpenAI Responses request into a Chat
// Completions request: instructions and system items become
// system messages, message items map by role, function_call items merge
// into assistant tool_calls, function_call_output items become role:"tool"
// messages, reasoning.effort maps to reasoning_effort,
// parallel_tool_calls passes through as-is (the reverse leg forwards the
// same field), and max_output_tokens maps to max_tokens. Historical
// reasoning items are omitted (no CC equivalent; explicit omission
// policy).
func responsesToChat(upstreamModel string, body []byte, ts *pluginapi.ThinkingSupport) ([]byte, *fault.Error) {
	var src common.ResponsesRequest
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, fault.Translation("malformed openai-response request JSON: " + err.Error())
	}
	out := &ccRequest{
		Model:             upstreamModel,
		Stream:            src.Stream,
		Temperature:       src.Temperature,
		TopP:              src.TopP,
		ParallelToolCalls: src.ParallelToolCalls,
	}
	if src.MaxOutputTokens != nil && *src.MaxOutputTokens > 0 {
		out.MaxTokens = src.MaxOutputTokens
	}
	if src.Reasoning != nil && src.Reasoning.Effort != "" {
		if eErr := reasoning.ValidateEffort(src.Reasoning.Effort, ts); eErr != nil {
			return nil, eErr
		}
		out.ReasoningEffort = strings.ToLower(strings.TrimSpace(src.Reasoning.Effort))
	}
	kind, tcName, eErr := common.DecodeToolChoice(src.ToolChoice)
	if eErr != nil {
		return nil, eErr
	}
	applyToolChoiceCC(out, kind, tcName)

	addSystem := func(text string) {
		if text != "" {
			out.Messages = append(out.Messages, ccMessage{Role: "system", Content: text})
		}
	}
	instr, eErr := src.DecodeInstructions()
	if eErr != nil {
		return nil, eErr
	}
	addSystem(instr)

	items, eErr := src.DecodeInputItems()
	if eErr != nil {
		return nil, eErr
	}
	for _, item := range items {
		kind := common.NormalizeResponsesItemType(item)
		if kind == "" {
			continue
		}
		switch kind {
		case "message":
			content, eErr := respContent(item.Content)
			if eErr != nil {
				return nil, eErr
			}
			switch item.Role {
			case "system", "developer":
				// Flatten to text; images have no system-message
				// equivalent and are rejected descriptively.
				text := ""
				if s, ok := content.(string); ok {
					text = s
				} else if parts, ok := content.([]ccContentPart); ok {
					var sb strings.Builder
					for _, p := range parts {
						if p.Type != "text" {
							return nil, common.SystemImageRejected()
						}
						sb.WriteString(p.Text)
					}
					text = sb.String()
				}
				addSystem(text)
			case "user", "assistant":
				if content != nil {
					out.Messages = append(out.Messages, ccMessage{Role: item.Role, Content: content})
				}
			default:
				return nil, common.ValidateRole(item.Role, EndpointPath)
			}
		case "function_call":
			tc := common.CCToolCall{ID: item.CallID, Type: "function"}
			tc.Function.Name = item.Name
			tc.Function.Arguments = common.DefaultArgs(item.Arguments)
			// Merge consecutive function_call items into one
			// assistant message so multi-call turns round-trip.
			if n := len(out.Messages); n > 0 {
				last := &out.Messages[n-1]
				if last.Role == "assistant" && last.Content == nil {
					last.ToolCalls = append(last.ToolCalls, tc)
					continue
				}
			}
			out.Messages = append(out.Messages, ccMessage{
				Role: "assistant", ToolCalls: []common.CCToolCall{tc},
			})
		case "function_call_output":
			text, eErr := common.FunctionCallOutputText(item.Output)
			if eErr != nil {
				return nil, eErr
			}
			out.Messages = append(out.Messages, ccMessage{
				Role: "tool", Content: text, ToolCallID: item.CallID,
			})
		case "reasoning":
			// omitted: no Chat Completions equivalent (policy)
		default:
			return nil, common.UnsupportedInputItemType(item.Type)
		}
	}

	for _, t := range src.Tools {
		if eErr := common.FunctionTool(t.Type, EndpointPath); eErr != nil {
			return nil, eErr
		}
		schema := common.ObjectSchema(t.Parameters)
		out.Tools = append(out.Tools, common.CCTool{
			Type:     "function",
			Function: common.CCFunction{Name: t.Name, Description: t.Description, Parameters: schema},
		})
	}
	return encode(out), nil
}

// respContent normalizes a Responses content field (JSON string or typed
// parts) into Chat Completions content: a plain string, content parts,
// or nil when empty (multimodal preservation). The shared kernel
// owns validation and the empty-input policy — empty-text messages drop
// identically on every route (parity).
func respContent(raw json.RawMessage) (any, *fault.Error) {
	parts, eErr := common.DecodeStringOrParts(raw, EndpointPath)
	if eErr != nil {
		return nil, eErr
	}
	switch {
	case len(parts) == 0:
		return nil, nil
	case len(parts) == 1 && parts[0].ImageURL == "":
		return parts[0].Text, nil
	}
	out := make([]ccContentPart, 0, len(parts))
	for _, p := range parts {
		if p.ImageURL != "" {
			out = append(out, ccContentPart{Type: "image_url", ImageURL: &imageURLField{URL: p.ImageURL}})
			continue
		}
		out = append(out, ccContentPart{Type: "text", Text: p.Text})
	}
	return out, nil
}

// applyToolChoiceCC maps a normalized tool choice onto the outbound Chat
// Completions request, which natively represents every normalized kind.
// It is infallible: common.DecodeToolChoice emits exactly the
// closed set below, so the switch covers every possible kind (the
// exhaustiveness test in request_test.go enforces this against
// common.AllToolChoiceKinds); absent omits the field.
func applyToolChoiceCC(req *ccRequest, kind, name string) {
	switch kind {
	case common.ToolChoiceAuto:
		req.ToolChoice = json.RawMessage(`"auto"`)
	case common.ToolChoiceAny:
		req.ToolChoice = json.RawMessage(`"required"`)
	case common.ToolChoiceNone:
		req.ToolChoice = json.RawMessage(`"none"`)
	case common.ToolChoiceNamed:
		b, _ := json.Marshal(map[string]any{
			"type": "function", "function": map[string]any{"name": name},
		})
		req.ToolChoice = b
	}
}
