// Non-stream response conversion for the Messages adapter
// into the client protocol — validated passthrough for claude clients,
// Chat Completions / Responses synthesis otherwise. Reasoning content has
// no representation outside Anthropic wire format and is explicitly
// omitted (policy).
package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"command-code-cpa-bridge/internal/fault"
	"command-code-cpa-bridge/internal/translate/common"
)

// ---- upstream Anthropic Messages response shapes (decode-only) ----

type claudeBlockIn struct {
	Type  string          `json:"type"` // text | thinking | tool_use | ...
	Text  string          `json:"text"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type claudeUsageIn struct {
	InputTokens   int64  `json:"input_tokens"`
	OutputTokens  int64  `json:"output_tokens"`
	CacheRead     *int64 `json:"cache_read_input_tokens"`
	CacheCreation *int64 `json:"cache_creation_input_tokens"`
}

type claudeResponseIn struct {
	ID         string          `json:"id"`
	Model      string          `json:"model"`
	Content    []claudeBlockIn `json:"content"`
	StopReason string          `json:"stop_reason"`
	Usage      claudeUsageIn   `json:"usage"`
}

// ConvertNonStreamResponse translates an upstream Anthropic Messages HTTP
// response back into the client protocol sourceFormat.
// Statuses >= 400 become classified redacted errors; "claude"
// passes through; "openai" and "openai-response" are converted, preserving
// text, tool_use, stop reason, and usage while dropping thinking blocks
// (explicit omission). Unknown formats are ClassUnsupported;
// malformed upstream bodies are ClassTranslation.
func ConvertNonStreamResponse(sourceFormat string, status int, upstreamBody []byte) ([]byte, *fault.Error) {
	if status >= 400 {
		return nil, common.UpstreamStatusError(status, upstreamBody)
	}
	switch sourceFormat {
	case "claude":
		if !json.Valid(upstreamBody) {
			return nil, fault.Translation("malformed Messages response JSON")
		}
		return upstreamBody, nil
	case "openai":
		return claudeToChat(upstreamBody)
	case "openai-response":
		return claudeToResponses(upstreamBody)
	default:
		return nil, common.UnsupportedFormat(sourceFormat, EndpointPath)
	}
}

// decodeClaude validates and decodes an upstream non-stream Messages body;
// a missing content array is a translation failure, never silently empty.
func decodeClaude(body []byte) (*claudeResponseIn, *fault.Error) {
	var resp claudeResponseIn
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fault.Translation("malformed Messages response JSON: " + err.Error())
	}
	if resp.Content == nil {
		return nil, fault.Translation("Messages response carries no content blocks")
	}
	return &resp, nil
}

// claudeToChat converts a Messages response into a Chat Completions
// response: text blocks concatenate into message.content, tool_use blocks
// become indexed tool_calls with JSON-encoded arguments, stop_reason maps
// to finish_reason, and input/output tokens map to prompt/completion
// usage. Thinking and redacted_thinking blocks are
// omitted — Chat Completions has no standard reasoning-delta field
// (explicit omission).
func claudeToChat(body []byte) ([]byte, *fault.Error) {
	resp, eErr := decodeClaude(body)
	if eErr != nil {
		return nil, eErr
	}
	var text strings.Builder
	msg := common.CCMessage{Role: "assistant"}
	for _, blk := range resp.Content {
		switch blk.Type {
		case "text":
			text.WriteString(blk.Text)
		case "tool_use":
			args, _ := json.Marshal(decodeBlockInput(blk.Input))
			tc := common.CCToolCall{ID: blk.ID, Type: "function"}
			tc.Function.Name = blk.Name
			tc.Function.Arguments = string(args)
			msg.ToolCalls = append(msg.ToolCalls, tc)
		case "thinking", "redacted_thinking":
			// explicit omission policy: no standard Chat
			// Completions reasoning field; dropped rather than put in a
			// non-standard one. redacted_thinking is pure encrypted
			// reasoning metadata, omitted like the stream path instead of
			// failing the conversion.
		default:
			return nil, fault.Translation(fmt.Sprintf(
				"%s output cannot represent %q content blocks", EndpointPath, blk.Type))
		}
	}
	// Aggregated text ships verbatim (repo-wide verbatim policy, matching
	// OutputAssembler.Render and the streaming legs): no trimming here.
	if text.Len() > 0 {
		msg.Content, _ = json.Marshal(text.String())
	}
	// Observed tool calls outrank the status-derived reason: a terminal
	// max_tokens cannot downgrade tool_calls to length.
	finish := common.TerminalReason(len(msg.ToolCalls) > 0, "tool_calls",
		common.ClaudeStopToFinish(resp.StopReason))
	out := common.CompletionEnvelope(resp.ID, resp.Model, time.Now().Unix(),
		[]map[string]any{{
			"index":         0,
			"message":       msg,
			"finish_reason": finish,
		}},
		common.CCUsageFrom(resp.Usage.InputTokens+valueOrZero(resp.Usage.CacheRead)+valueOrZero(resp.Usage.CacheCreation), resp.Usage.OutputTokens, common.UsageDetails{CachedTokens: resp.Usage.CacheRead}))
	b, _ := json.Marshal(out) // composed marshallable types only; cannot fail
	return b, nil
}

// decodeBlockInput normalizes a tool_use input payload: absent/empty
// decodes to an empty object so every tool_call carries valid arguments.
// The raw bytes come from an already-validated outer document, so they are
// always valid JSON.
func decodeBlockInput(raw json.RawMessage) any {
	if !common.HasContent(raw) {
		return map[string]any{}
	}
	var v any
	json.Unmarshal(raw, &v)
	return v
}

// ---- Messages -> openai-response (Responses) ----

// claudeToResponses converts a Messages response into a Responses result:
// text blocks aggregate into an assistant output_text message item,
// tool_use blocks become function_call items, stop_reason end_turn maps to
// status completed, and input/output tokens map to input/output usage.
// Thinking and redacted_thinking blocks are omitted — Responses
// has no standard reasoning-summary equivalent without signatures (
// policy).
func claudeToResponses(body []byte) ([]byte, *fault.Error) {
	resp, eErr := decodeClaude(body)
	if eErr != nil {
		return nil, eErr
	}
	// Stop reason maps straight onto the Responses status via the shared
	// table (max_tokens -> incomplete, end_turn -> completed); function_call
	// items already carry the tool signal.
	status := common.ResponseStatusFromClaudeStop(resp.StopReason)
	out := common.ResponsesResult{
		ID:     resp.ID,
		Object: "response",
		Model:  resp.Model,
		Status: status,
		Output: []any{},
		Usage:  common.NewResponsesUsageFrom(resp.Usage.InputTokens+valueOrZero(resp.Usage.CacheRead)+valueOrZero(resp.Usage.CacheCreation), resp.Usage.OutputTokens, common.UsageDetails{CachedTokens: resp.Usage.CacheRead, CacheWriteTokens: resp.Usage.CacheCreation}),
	}
	oa := common.NewOutputAssembler(resp.ID)
	for _, blk := range resp.Content {
		switch blk.Type {
		case "text":
			// The shared assembler reserves the first text block's slot so
			// the output array matches the streaming synthesizer's
			// upstream-order emission (stream.go outputItems): identical
			// input must not reorder by mode.
			oa.ReserveTextSlot()
			oa.AddText(blk.Text)
		case "tool_use":
			args, _ := json.Marshal(decodeBlockInput(blk.Input))
			oa.AppendFunctionCall(blk.ID, blk.Name, string(args))
		case "thinking", "redacted_thinking":
			// explicit omission policy (see claudeToChat).
		default:
			return nil, fault.Translation(fmt.Sprintf(
				"%s output cannot represent %q content blocks", EndpointPath, blk.Type))
		}
	}
	out.Output = oa.Render()
	b, _ := json.Marshal(out) // only marshallable composed types; cannot fail
	return b, nil
}

func valueOrZero(v *int64) int64 {
	if v != nil {
		return *v
	}
	return 0
}
