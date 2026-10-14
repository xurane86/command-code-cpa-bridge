// Package responses implements the OpenAI Responses protocol adapter
// upstream endpoint /v1/responses serving GPT 5.6 Luna and
// any catalog model declared compatible. Request translation is
// response and stream translation, acceptance coverage
// Responses support is REQUIRED first-release scope, not
// experimental (07-open-questions.md).
//
// Per this package is isolated from the scheduler and catalog
// manager and uses only stdlib JSON primitives.
package responses

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"command-code-cpa-bridge/internal/fault"
	"command-code-cpa-bridge/internal/reasoning"
	"command-code-cpa-bridge/internal/registry"
	"command-code-cpa-bridge/internal/translate/common"
)

// EndpointPath is the upstream endpoint path for the Responses route.
var EndpointPath = registry.RouteResponses.EndpointPath()

// BuildRequest translates an inbound request body from sourceFormat
// ("openai" Chat Completions, "claude" Anthropic Messages,
// "openai-response" native Responses passthrough) into a Responses body
// for upstreamModel. upstreamModel replaces whatever
// client-facing model ID the request carried. ts is the target
// model's declared reasoning capability and drives budget→effort mapping
// via the shared thinking package; nil falls back to the default ladder.
// Unknown formats are ClassUnsupported; malformed input is
// ClassTranslation; messages are descriptive and redacted.
func BuildRequest(upstreamModel string, sourceFormat string, sourceBody []byte, ts *pluginapi.ThinkingSupport) ([]byte, *fault.Error) {
	switch sourceFormat {
	case "openai-response":
		return common.RewriteModelID(upstreamModel, sourceBody, "openai-response")
	case "openai":
		return fromChatCompletions(upstreamModel, sourceBody, ts)
	case "claude":
		return fromClaudeMessages(upstreamModel, sourceBody, ts)
	default:
		return nil, common.UnsupportedFormat(sourceFormat, EndpointPath)
	}
}

// ---- target wire shape -------------------------------------------------

type responsesEnvelope struct {
	Model             string            `json:"model"`
	Input             []any             `json:"input,omitempty"`
	Instructions      string            `json:"instructions,omitempty"`
	Tools             []common.RespTool `json:"tools,omitempty"`
	ToolChoice        json.RawMessage   `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool             `json:"parallel_tool_calls,omitempty"`
	Reasoning         map[string]any    `json:"reasoning,omitempty"`
	MaxOutputTokens   *int64            `json:"max_output_tokens,omitempty"`
	Stream            bool              `json:"stream,omitempty"`
	Temperature       *float64          `json:"temperature,omitempty"`
	TopP              *float64          `json:"top_p,omitempty"`
}

func msgItem(role string, parts []map[string]any) map[string]any {
	return map[string]any{"type": "message", "role": role, "content": parts}
}

// textPart renders message text; assistant history replays with
// output_text, every other role uses input_text.
func textPart(role, text string) map[string]any {
	if role == "assistant" {
		return map[string]any{"type": "output_text", "text": text}
	}
	return map[string]any{"type": "input_text", "text": text}
}

func imagePart(url string) map[string]any {
	return map[string]any{"type": "input_image", "image_url": url}
}

// respToolChoice renders a common.DecodeToolChoice kind into the Responses
// wire value; absent yields nil so omitempty drops the field. It is
// infallible: common.DecodeToolChoice emits exactly the closed set below,
// so the switch covers every possible kind (the exhaustiveness test in
// request_test.go enforces this against common.AllToolChoiceKinds).
func respToolChoice(kind, name string) json.RawMessage {
	switch kind {
	case common.ToolChoiceAuto:
		return json.RawMessage(`"auto"`)
	case common.ToolChoiceAny:
		return json.RawMessage(`"required"`)
	case common.ToolChoiceNone:
		return json.RawMessage(`"none"`)
	case common.ToolChoiceNamed:
		b, _ := json.Marshal(map[string]any{"type": "function", "name": name})
		return b
	}
	return nil
}

// ---- Chat Completions source -------------------------------------------

// contentParts normalizes an OpenAI-style content field (JSON string or
// typed parts) into Responses input parts for the given role (
// multimodal preservation). The shared kernel owns validation and the
// empty-input policy: empty-text parts decode to zero parts so empty
// messages drop identically on every route (parity).
func contentParts(raw json.RawMessage, role string) ([]map[string]any, *fault.Error) {
	parts, eErr := common.DecodeStringOrParts(raw, EndpointPath)
	if eErr != nil {
		return nil, eErr
	}
	out := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		if p.ImageURL != "" {
			out = append(out, imagePart(p.ImageURL))
			continue
		}
		out = append(out, textPart(role, p.Text))
	}
	return out, nil
}

// fromChatCompletions translates a Chat Completions request into a
// Responses request: system/developer messages become
// instructions, user/assistant turns become message items (assistant
// replaying output_text), tool_calls history becomes function_call items,
// tool results become function_call_output items, max_tokens/
// max_completion_tokens become max_output_tokens, and reasoning_effort is
// capability-checked against ts and rejected descriptively when the model
// cannot represent it (matching the reverse Responses→CC leg).
// tool_choice is decoded by the shared classifier and rendered into the
// Responses wire values ("auto"/"none"/"required", flat
// {type:function,name} for a forced tool); parallel_tool_calls passes
// through as-is.
//
// Explicit omission policy ("where compatible"): `stop` has no
// Responses equivalent and is dropped, as are sampling/extras without an
// equivalent (logprobs, frequency_penalty, presence_penalty, n, seed,
// response_format, logit_bias) via struct selection. Tool calls and
// reasoning controls are never dropped silently.
func fromChatCompletions(upstreamModel string, body []byte, ts *pluginapi.ThinkingSupport) ([]byte, *fault.Error) {
	var src common.ChatCompletionsRequest
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, fault.Translation("malformed openai request JSON: " + err.Error())
	}
	req := &responsesEnvelope{
		Model:             upstreamModel,
		Stream:            src.Stream,
		Temperature:       src.Temperature,
		TopP:              src.TopP,
		ParallelToolCalls: src.ParallelToolCalls,
	}
	kind, tcName, eErr := common.DecodeToolChoice(src.ToolChoice)
	if eErr != nil {
		return nil, eErr
	}
	req.ToolChoice = respToolChoice(kind, tcName)
	if src.MaxTokens != nil && *src.MaxTokens > 0 {
		req.MaxOutputTokens = src.MaxTokens
	} else if src.MaxCompletionTokens != nil && *src.MaxCompletionTokens > 0 {
		req.MaxOutputTokens = src.MaxCompletionTokens
	}
	if src.ReasoningEffort != "" {
		if eErr := reasoning.ValidateEffort(src.ReasoningEffort, ts); eErr != nil {
			return nil, eErr
		}
		// The dynamic "auto" sentinel omits reasoning via the shared policy
		// below; a declared "none" and every other validated value forward
		// as-is (matching the CC-upstream leg).
		if effort, ok := reasoningEffortFor(strings.ToLower(strings.TrimSpace(src.ReasoningEffort)), ts); ok {
			req.Reasoning = map[string]any{"effort": effort}
		}
	}

	var instr strings.Builder
	for i := range src.Messages {
		m := &src.Messages[i]
		switch m.Role {
		case "system", "developer":
			parts, eErr := contentParts(m.Content, m.Role)
			if eErr != nil {
				return nil, eErr
			}
			// Instructions have no image representation: reject like the
			// sibling routes instead of dropping silently.
			for _, p := range parts {
				if typ, _ := p["type"].(string); typ != "input_text" && typ != "output_text" {
					return nil, common.SystemImageRejected()
				}
			}
			if text := common.JoinTexts(parts); text != "" {
				if instr.Len() > 0 {
					instr.WriteString("\n\n")
				}
				instr.WriteString(text)
			}
		case "user":
			parts, eErr := contentParts(m.Content, m.Role)
			if eErr != nil {
				return nil, eErr
			}
			if len(parts) > 0 {
				req.Input = append(req.Input, msgItem(m.Role, parts))
			}
		case "assistant":
			parts, eErr := contentParts(m.Content, m.Role)
			if eErr != nil {
				return nil, eErr
			}
			if len(parts) > 0 {
				req.Input = append(req.Input, msgItem(m.Role, parts))
			}
			for _, tc := range m.ToolCalls {
				req.Input = append(req.Input, map[string]any{
					"type":      "function_call",
					"call_id":   tc.ID,
					"name":      tc.Function.Name,
					"arguments": common.DefaultArgs(tc.Function.Arguments),
				})
			}
		case "tool":
			parts, eErr := contentParts(m.Content, m.Role)
			if eErr != nil {
				return nil, eErr
			}
			for _, p := range parts {
				if typ, _ := p["type"].(string); typ != "input_text" && typ != "output_text" {
					return nil, fault.Translation(fmt.Sprintf(
						"unsupported tool content part type %q; function_call_output carries text only", typ))
				}
			}
			req.Input = append(req.Input, map[string]any{
				"type":    "function_call_output",
				"call_id": m.ToolCallID,
				"output":  common.JoinTexts(parts),
			})
		default:
			return nil, common.ValidateRole(m.Role, EndpointPath)
		}
	}
	req.Instructions = instr.String()

	for _, t := range src.Tools {
		if eErr := common.FunctionTool(t.Type, EndpointPath); eErr != nil {
			return nil, eErr
		}
		req.Tools = append(req.Tools, common.RespTool{
			Type:        "function",
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  common.ObjectSchema(t.Function.Parameters),
		})
	}
	b, _ := json.Marshal(req) // only marshallable composed types; cannot fail
	return b, nil
}

// ---- Claude Messages source --------------------------------------------

// reasoningEffortFor is the single off-state policy for the Responses
// target, applied to an already-resolved effort value from both
// source legs. The dynamic "auto" sentinel has no wire representation here,
// so the reasoning field is omitted entirely (no dynamic sentinel on the
// Responses wire). "none" forwards verbatim when the model declares it via
// SupportedLevels — the same admission reasoning.ValidateEffort applies and
// matching the CC-upstream leg which forwards the validated value as-is;
// when "none" is not declared there is nothing to express, so the field is
// omitted rather than clamped to the weakest representable level. Every
// other validated level forwards as-is.
func reasoningEffortFor(effort string, ts *pluginapi.ThinkingSupport) (string, bool) {
	switch {
	case effort == "auto":
		return "", false
	case effort == "none":
		if slices.Contains(reasoning.SupportedLevels(ts), "none") {
			return effort, true
		}
		return "", false
	default:
		return effort, true
	}
}

// fromClaudeMessages translates an Anthropic Messages request into a
// Responses request: system becomes instructions, text/
// image blocks become input parts, tool_use/tool_result become
// function_call/function_call_output items, an enabled thinking budget
// maps to reasoning.effort through reasoningEffortFor (the "auto" sentinel
// and an undeclared "none" omit the field entirely; a declared "none"
// forwards), max_tokens becomes max_output_tokens (defaulted by the
// shared kernel,). Decoding is owned entirely by the shared
// Claude-request kernel; only target-shape rendering stays local.
//
// Explicit omission policy ("where compatible"): stop_sequences is
// dropped (no Responses equivalent); thinking/redacted_thinking blocks are
// dropped because signed Anthropic chain-of-thought has no Responses input
// representation (the thinking *control* maps to reasoning.effort); images
// nested inside tool_result content are rejected descriptively, never
// silently lost. The tool_result is_error flag has no Responses
// field and is preserved as an "[error] " marker in the output text rather
// than lost silently.
func fromClaudeMessages(upstreamModel string, body []byte, ts *pluginapi.ThinkingSupport) ([]byte, *fault.Error) {
	src, eErr := common.DecodeClaudeMessages(body)
	if eErr != nil {
		return nil, eErr
	}
	req := &responsesEnvelope{
		Model:           upstreamModel,
		MaxOutputTokens: &src.MaxTokens,
		Stream:          src.Stream,
		Temperature:     src.Temperature,
		TopP:            src.TopP,
	}
	req.ToolChoice = respToolChoice(src.ToolChoiceKind, src.ToolChoiceName)
	if common.ThinkingEnabled(src.Thinking) {
		if effort, ok := reasoningEffortFor(reasoning.EffortFromBudget(src.Thinking.BudgetTokens, ts), ts); ok {
			req.Reasoning = map[string]any{"effort": effort}
		}
	}
	req.Instructions = src.System

	for _, m := range src.Messages {
		switch m.Role {
		case "user", "assistant":
		default:
			return nil, common.ValidateRole(m.Role, EndpointPath)
		}
		var parts []map[string]any
		flush := func() {
			if len(parts) > 0 {
				req.Input = append(req.Input, msgItem(m.Role, parts))
				parts = nil
			}
		}
		if m.Content != "" {
			parts = append(parts, textPart(m.Role, m.Content))
		}
		for _, blk := range m.Blocks {
			switch blk.Kind {
			case "text":
				parts = append(parts, textPart(m.Role, blk.Text))
			case "image":
				parts = append(parts, imagePart(blk.URL))
			case "tool_use":
				flush()
				args := common.DefaultArgs(string(blk.Input))
				req.Input = append(req.Input, map[string]any{
					"type":      "function_call",
					"call_id":   blk.CallID,
					"name":      blk.Name,
					"arguments": args,
				})
			case "tool_result":
				flush()
				output, eErr := common.ToolResultText(blk.Result, "function_call_output carries text only")
				if eErr != nil {
					return nil, eErr
				}
				if blk.IsError {
					output = common.ToolResultErrorPrefix + output
				}
				req.Input = append(req.Input, map[string]any{
					"type":    "function_call_output",
					"call_id": blk.CallID,
					"output":  output,
				})
			case "thinking", "redacted_thinking":
				// omitted per the policy in the doc comment
			default:
				flush()
				return nil, common.UnsupportedPartType(blk.Kind, EndpointPath)
			}
		}
		flush()
	}

	for _, t := range src.Tools {
		// Claude tools carry no type field (validation cannot reject here).
		req.Tools = append(req.Tools, common.RespTool{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			Parameters:  common.ObjectSchema(t.InputSchema),
		})
	}
	b, _ := json.Marshal(req) // only marshallable composed types; cannot fail
	return b, nil
}
