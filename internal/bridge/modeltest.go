package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"

	"command-code-cpa-bridge/internal/config"
	"command-code-cpa-bridge/internal/registry"
)

// modelTestRequest is the console "test this model" payload. It never leaves
// the process with a key attached.
type modelTestRequest struct {
	Model        string `json:"model"`
	CredentialID string `json:"credential_id"`
	Prompt       string `json:"prompt"`
	Stream       bool   `json:"stream"`
	MaxTokens    int    `json:"max_tokens"`
}

const (
	modelTestTimeout   = 60 * time.Second
	modelTestReadSlice = 5 * time.Second
)

// modelTest performs one real upstream call against a selected credential so
// an operator can confirm a model, credential, and route end to end. It
// consumes a small amount of quota by design.
func (m *Manager) modelTest(ctx context.Context, body []byte) pluginapi.ManagementResponse {
	var in modelTestRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &in); err != nil {
			return jsonResponse(http.StatusBadRequest, map[string]any{"error": "测试请求格式无效"})
		}
	}
	in.Model = strings.TrimSpace(in.Model)
	if in.Model == "" {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": "请选择要测试的模型"})
	}
	prompt := strings.TrimSpace(in.Prompt)
	if prompt == "" {
		prompt = "你好，请用一句话回复。"
	}
	if in.MaxTokens <= 0 || in.MaxTokens > 512 {
		in.MaxTokens = 64
	}
	if m.bridge == nil {
		return jsonResponse(http.StatusServiceUnavailable, map[string]any{"error": "host bridge 不可用"})
	}

	m.mu.RLock()
	mgr := m.mgr
	cfg := m.cfg
	m.mu.RUnlock()
	if mgr == nil {
		return jsonResponse(http.StatusServiceUnavailable, map[string]any{"error": "目录尚未初始化"})
	}
	rec, ok := mgr.Lookup(in.Model)
	if !ok {
		return jsonResponse(http.StatusNotFound, map[string]any{"error": "模型不在可路由目录中：" + in.Model})
	}

	key, credID, keyErr := m.resolveTestKey(ctx, cfg, in.CredentialID)
	if keyErr != "" {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": keyErr})
	}

	upstreamBody, err := buildModelTestBody(rec.UpstreamID, prompt, in.MaxTokens, in.Stream)
	if err != nil {
		return jsonResponse(http.StatusInternalServerError, map[string]any{"error": "构造测试请求失败"})
	}
	upstreamBody = m.mappedReasoningBody(upstreamBody, rec.UpstreamID)

	url := registry.JoinUpstreamURL(cfg.BaseURL, rec.EndpointPath)
	headers := upstreamAuthHeaders(rec.Protocol, key, emptyCommandCodeSessionID)
	callCtx, cancel := context.WithTimeout(ctx, modelTestTimeout)
	defer cancel()

	out := map[string]any{
		"model":          in.Model,
		"upstream_model": rec.UpstreamID,
		"protocol":       string(rec.Protocol),
		"credential_id":  credID,
	}
	started := time.Now()

	if in.Stream {
		return m.modelTestStream(callCtx, url, headers, upstreamBody, out, started)
	}

	resp, err := m.bridge.Do(callCtx, pluginapi.HTTPRequest{Method: http.MethodPost, URL: url, Headers: headers, Body: upstreamBody})
	out["latency_ms"] = time.Since(started).Milliseconds()
	if err != nil {
		out["ok"] = false
		out["error"] = err.Error()
		return jsonResponse(http.StatusOK, out)
	}
	out["status"] = resp.StatusCode
	if resp.StatusCode >= 400 {
		out["ok"] = false
		out["error"] = snippet(resp.Body, 600)
		return jsonResponse(http.StatusOK, out)
	}
	inputTokens, outputTokens, cachedTokens := parseUsage(resp.Body)
	out["ok"] = true
	out["reply"] = extractChatText(resp.Body)
	out["usage"] = map[string]any{"input_tokens": inputTokens, "output_tokens": outputTokens, "cached_tokens": cachedTokens}
	return jsonResponse(http.StatusOK, out)
}

func (m *Manager) modelTestStream(callCtx context.Context, url string, headers http.Header, body []byte, out map[string]any, started time.Time) pluginapi.ManagementResponse {
	status, _, sid, err := m.bridge.DoStream(callCtx, pluginapi.HTTPRequest{Method: http.MethodPost, URL: url, Headers: headers, Body: body})
	if err != nil {
		out["ok"] = false
		out["latency_ms"] = time.Since(started).Milliseconds()
		out["error"] = err.Error()
		return jsonResponse(http.StatusOK, out)
	}
	out["status"] = status
	if status >= 400 {
		var bodyText []byte
		if sid != "" {
			payload, errMsg, _, readErr := m.bridge.StreamRead(sid)
			if readErr == nil {
				if len(payload) > 0 {
					bodyText = payload
				} else if errMsg != "" {
					bodyText = []byte(errMsg)
				}
			}
			_ = m.bridge.StreamClose(sid)
		}
		out["ok"] = false
		out["error"] = snippet(bodyText, 600)
		out["latency_ms"] = time.Since(started).Milliseconds()
		return jsonResponse(http.StatusOK, out)
	}

	// Bound the collection window: closing the stream unblocks a parked read.
	timer := time.AfterFunc(modelTestReadSlice, func() { _ = m.bridge.StreamClose(sid) })
	defer timer.Stop()

	var raw strings.Builder
	var firstByte int64
	for sid != "" {
		payload, errMsg, done, readErr := m.bridge.StreamRead(sid)
		if readErr != nil {
			break
		}
		if errMsg != "" {
			out["error"] = errMsg
			break
		}
		if len(payload) > 0 {
			if firstByte == 0 {
				firstByte = time.Since(started).Milliseconds()
			}
			raw.Write(payload)
		}
		if done {
			break
		}
	}
	_ = m.bridge.StreamClose(sid)
	out["ok"] = true
	out["first_byte_ms"] = firstByte
	out["latency_ms"] = time.Since(started).Milliseconds()
	out["reply"] = extractStreamText(raw.String())
	return jsonResponse(http.StatusOK, out)
}

// resolveTestKey returns the API key to test with: the named credential's key
// or, when empty, any available credential.
func (m *Manager) resolveTestKey(ctx context.Context, cfg config.Config, credentialID string) (key, credID, failure string) {
	credentialID = strings.TrimSpace(credentialID)
	if credentialID == "" {
		resolved, err := m.catalogKey(ctx, cfg)
		if err != nil {
			return "", "", err.Error()
		}
		return resolved, "", ""
	}
	entry, err := m.findCredential(ctx, credentialID)
	if err != nil {
		return "", credentialID, err.Error()
	}
	if path := strings.TrimSpace(entry.Path); path != "" {
		if k := readKeyFromFile(path); k != "" {
			return k, credentialID, ""
		}
	}
	return "", credentialID, "该凭据没有可用的 API Key"
}

func buildModelTestBody(upstreamModel, prompt string, maxTokens int, stream bool) ([]byte, error) {
	return json.Marshal(map[string]any{
		"model": upstreamModel,
		"messages": []map[string]any{
			{"role": "user", "content": prompt},
		},
		"max_tokens": maxTokens,
		"stream":     stream,
	})
}

// extractChatText reads the assistant text from a chat-completions response,
// accepting string or part-array content and falling back to reasoning text.
func extractChatText(body []byte) string {
	message := gjson.GetBytes(body, "choices.0.message")
	if text := contentText(message.Get("content")); text != "" {
		return text
	}
	if text := message.Get("reasoning_content").String(); text != "" {
		return text
	}
	return ""
}

func contentText(node gjson.Result) string {
	if !node.Exists() {
		return ""
	}
	if node.Type == gjson.String {
		return node.String()
	}
	var sb strings.Builder
	for _, part := range node.Array() {
		if part.Get("type").String() == "text" || part.Get("text").Exists() {
			sb.WriteString(part.Get("text").String())
		}
	}
	return sb.String()
}

// extractStreamText concatenates the text deltas of an SSE stream body.
func extractStreamText(raw string) string {
	var sb strings.Builder
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" || !gjson.Valid(payload) {
			continue
		}
		delta := gjson.Get(payload, "choices.0.delta")
		if text := contentText(delta.Get("content")); text != "" {
			sb.WriteString(text)
			continue
		}
		if text := delta.Get("reasoning_content").String(); text != "" {
			sb.WriteString(text)
		}
	}
	return sb.String()
}

// snippet bounds and single-lines an upstream error body for display.
func snippet(body []byte, maxLen int) string {
	s := strings.TrimSpace(string(body))
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return "上游返回空响应"
	}
	if len(s) > maxLen {
		return s[:maxLen] + "…"
	}
	return s
}
