package bridge

import (
	"strings"

	"github.com/tidwall/gjson"
)

// telemetryRecord forwards one request log entry to the live telemetry store.
func (m *Manager) telemetryRecord(e RequestLog) {
	if m == nil {
		return
	}
	m.mu.RLock()
	tel := m.telemetry
	m.mu.RUnlock()
	if tel != nil {
		tel.record(e)
	}
}

// parseUsage extracts token counters from a non-streaming response body,
// accepting both the OpenAI and the Anthropic response shapes.
func parseUsage(body []byte) (input, output, cached int64) {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return 0, 0, 0
	}
	usage := gjson.GetBytes(body, "usage")
	if !usage.Exists() {
		return 0, 0, 0
	}
	input = firstInt(usage,
		"prompt_tokens",
		"input_tokens",
	)
	output = firstInt(usage,
		"completion_tokens",
		"output_tokens",
	)
	cached = firstInt(usage,
		"prompt_tokens_details.cached_tokens",
		"cache_read_input_tokens",
		"input_tokens_details.cached_tokens",
	)
	return input, output, cached
}

func firstInt(node gjson.Result, paths ...string) int64 {
	for _, p := range paths {
		if v := node.Get(p); v.Exists() {
			return v.Int()
		}
	}
	return 0
}

// scanStreamUsage folds token counters found in one translated stream event
// into the running totals. Only the last reported non-zero value wins, which
// matches how both protocols emit a terminal usage block.
func scanStreamUsage(event []byte, input, output, cached *int64) {
	if len(event) == 0 || !gjson.ValidBytes(event) {
		return
	}
	if !strings.Contains(string(event), "usage") {
		return
	}
	if v := gjson.GetBytes(event, "usage"); v.Exists() {
		if n := firstInt(v, "prompt_tokens", "input_tokens"); n > 0 {
			*input = n
		}
		if n := firstInt(v, "completion_tokens", "output_tokens"); n > 0 {
			*output = n
		}
		if n := firstInt(v, "prompt_tokens_details.cached_tokens", "cache_read_input_tokens", "input_tokens_details.cached_tokens"); n > 0 {
			*cached = n
		}
		return
	}
	// Some adapters report usage on the response object root.
	if v := gjson.GetBytes(event, "response.usage"); v.Exists() {
		if n := firstInt(v, "prompt_tokens", "input_tokens"); n > 0 {
			*input = n
		}
		if n := firstInt(v, "completion_tokens", "output_tokens"); n > 0 {
			*output = n
		}
	}
}
