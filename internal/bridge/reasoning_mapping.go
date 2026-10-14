package bridge

import (
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// mappedReasoningBody rewrites the upstream body's reasoning_effort according
// to the page-configured mapping. It is a no-op when no rule applies, so the
// verbatim-passthrough behaviour of the provider is preserved by default.
func (m *Manager) mappedReasoningBody(body []byte, upstreamModel string) []byte {
	if m == nil || len(body) == 0 || !gjson.ValidBytes(body) {
		return body
	}
	m.mu.RLock()
	settings := m.settings
	m.mu.RUnlock()
	if len(settings.ReasoningRules) == 0 {
		return body
	}
	effort := gjson.GetBytes(body, "reasoning_effort").String()
	if effort == "" {
		return body
	}
	to := settings.mapReasoningEffort(upstreamModel, effort)
	if to == "" || to == effort {
		return body
	}
	if to == "omit" {
		if out, err := sjson.DeleteBytes(body, "reasoning_effort"); err == nil {
			return out
		}
		return body
	}
	out, err := sjson.SetBytes(body, "reasoning_effort", to)
	if err != nil {
		return body
	}
	return out
}
