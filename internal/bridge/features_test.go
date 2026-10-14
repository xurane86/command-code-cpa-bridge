package bridge

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// ---- reasoning effort mapping -------------------------------------------

func TestMappedReasoningBody(t *testing.T) {
	m := NewManager(nil)
	m.settings = pageSettings{ReasoningRules: []ReasoningRule{
		{From: "medium", To: "high"},
		{Model: "minimax-m3", From: "high", To: "max"},
		{From: "auto", To: "omit"},
	}}

	cases := []struct {
		name  string
		model string
		body  string
		want  string
	}{
		{"default rule", "glm-5.3", `{"model":"glm-5.3","reasoning_effort":"medium"}`, `"high"`},
		{"model rule wins", "minimax-m3", `{"reasoning_effort":"high"}`, `"max"`},
		{"omit removes field", "glm-5.3", `{"reasoning_effort":"auto"}`, `reasoning_effort`},
		{"unmapped untouched", "glm-5.3", `{"reasoning_effort":"low"}`, `"low"`},
		{"no field untouched", "glm-5.3", `{"messages":[]}`, `"messages"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(m.mappedReasoningBody([]byte(tc.body), tc.model))
			switch tc.name {
			case "omit removes field":
				if strings.Contains(got, "reasoning_effort") {
					t.Fatalf("omit left reasoning_effort: %s", got)
				}
				return
			case "no field untouched":
				if !strings.Contains(got, tc.want) || strings.Contains(got, "reasoning_effort") {
					t.Fatalf("body changed unexpectedly: %s", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("body = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestMappedReasoningBodyNoRulesIsPassthrough(t *testing.T) {
	m := NewManager(nil)
	in := []byte(`{"reasoning_effort":"xhigh"}`)
	if got := string(m.mappedReasoningBody(in, "m")); got != string(in) {
		t.Fatalf("passthrough changed body: %s", got)
	}
}

func TestSaveReasoningRulesPersists(t *testing.T) {
	dir := t.TempDir()
	f := &fakeCaller{responder: catalogResponder(true, testCatalogJSON)}
	m := NewManager(NewHostBridge(f.call))
	defer func() { _, _ = m.HandleCall(pluginabi.MethodPluginShutdown, nil) }()
	if _, err := m.HandleCall(pluginabi.MethodPluginRegister, lifecycleRequestBody(
		"data-dir: '"+filepath.ToSlash(dir)+"'\napi-keys:\n  - value: sk-reasoning\n")); err != nil {
		t.Fatalf("register: %v", err)
	}

	code, _ := mgmtCall(t, m, http.MethodPut, "/reasoning", map[string]any{
		"rules": []map[string]any{
			{"from": "medium", "to": "high"},
			{"model": "glm-5.3", "from": "high", "to": "max"},
		},
	}, nil)
	if code != http.StatusOK {
		t.Fatalf("PUT /reasoning status = %d", code)
	}
	_, got := mgmtCall(t, m, http.MethodGet, "/reasoning", nil, nil)
	rules, _ := got["rules"].([]any)
	if len(rules) != 2 {
		t.Fatalf("rules = %+v", got["rules"])
	}
	if m.currentSettings().mapReasoningEffort("glm-5.3", "medium") != "high" {
		t.Fatal("default rule not applied")
	}
	if m.currentSettings().mapReasoningEffort("glm-5.3", "high") != "max" {
		t.Fatal("model rule not applied")
	}
}

// ---- model test ----------------------------------------------------------

// testResponder answers the catalog call and the chat-completions test call
// from one fake host, keyed on the request URL.
func testResponder(stream bool) func(string, []byte) ([]byte, error) {
	return func(method string, payload []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostHTTPDo, pluginabi.MethodHostHTTPDoStream:
			var req struct {
				URL string `json:"url"`
			}
			_ = json.Unmarshal(payload, &req)
			if strings.HasSuffix(req.URL, "/models") {
				return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(testCatalogJSON)}), nil
			}
			if stream && method == pluginabi.MethodHostHTTPDoStream {
				return hostOK(hostStreamStartResp{StatusCode: http.StatusOK, StreamID: "s1"}), nil
			}
			return hostOK(pluginapi.HTTPResponse{
				StatusCode: http.StatusOK,
				Body:       []byte(`{"choices":[{"message":{"role":"assistant","content":"你好，这是测试回复"}}],"usage":{"prompt_tokens":7,"completion_tokens":9}}`),
			}), nil
		case pluginabi.MethodHostHTTPStreamRead:
			return hostOK(hostStreamReadResp{
				Payload: []byte("data: {\"choices\":[{\"delta\":{\"content\":\"你好，这也是\"}}]}\n\n"),
				Done:    true,
			}), nil
		default:
			return hostOK(map[string]any{}), nil
		}
	}
}

func registeredForTest(t *testing.T, stream bool) *Manager {
	t.Helper()
	f := &fakeCaller{responder: testResponder(stream)}
	m := NewManager(NewHostBridge(f.call))
	if _, err := m.HandleCall(pluginabi.MethodPluginRegister, lifecycleRequestBody(dummyKeyYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() { _, _ = m.HandleCall(pluginabi.MethodPluginShutdown, nil) })
	return m
}

func TestModelTestNonStream(t *testing.T) {
	m := registeredForTest(t, false)
	code, got := mgmtCall(t, m, http.MethodPost, "/models/test", map[string]any{"model": "commandcode/glm-5.3"}, nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if got["ok"] != true {
		t.Fatalf("result = %+v", got)
	}
	if reply, _ := got["reply"].(string); !strings.Contains(reply, "测试回复") {
		t.Fatalf("reply = %q", reply)
	}
	if got["upstream_model"] != "glm-5.3" {
		t.Fatalf("upstream_model = %v", got["upstream_model"])
	}
}

func TestModelTestStream(t *testing.T) {
	m := registeredForTest(t, true)
	code, got := mgmtCall(t, m, http.MethodPost, "/models/test", map[string]any{"model": "commandcode/glm-5.3", "stream": true}, nil)
	if code != http.StatusOK || got["ok"] != true {
		t.Fatalf("result = %+v code=%d", got, code)
	}
	if reply, _ := got["reply"].(string); !strings.Contains(reply, "这也是") {
		t.Fatalf("stream reply = %q", reply)
	}
}

func TestModelTestUnknownModel(t *testing.T) {
	m := registeredForTest(t, false)
	code, got := mgmtCall(t, m, http.MethodPost, "/models/test", map[string]any{"model": "nope"}, nil)
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %+v)", code, got)
	}
}
