package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// mgmtCall invokes the console handler and decodes a JSON body into v.
func mgmtCall(t *testing.T, m *Manager, method, path string, body any, query url.Values) (int, map[string]any) {
	t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: method, Path: apiPrefix + path, Query: query, Body: raw,
	})
	if err != nil {
		t.Fatalf("management %s %s: %v", method, path, err)
	}
	var out map[string]any
	if len(resp.Body) > 0 {
		if err := json.Unmarshal(resp.Body, &out); err != nil {
			t.Fatalf("decode body %s %s: %v (%s)", method, path, err, resp.Body)
		}
	}
	return resp.StatusCode, out
}

func TestConsolePageIsFullyTemplated(t *testing.T) {
	m := NewManager(nil)
	resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodGet, Path: consolePath})
	if err != nil {
		t.Fatalf("console: %v", err)
	}
	page := string(resp.Body)
	if strings.Contains(page, "__API_BASE__") || strings.Contains(page, "__VERSION__") || strings.Contains(page, "/*__AUTH_MODULE__*/") {
		t.Fatal("console page still contains template placeholders")
	}
	if !strings.Contains(page, apiPrefix) || !strings.Contains(page, pluginVersion) || !strings.Contains(page, "CommandCode") {
		t.Fatal("console page missing injected base path, version, or title")
	}
	// The auth module is injected in place and bridges the host theme.
	for _, marker := range []string{"CCBridgeAuthHeaders", "CCBridgeSaveAuth", "CCBridgeClearAuth", `data-theme="white"`} {
		if !strings.Contains(page, marker) {
			t.Fatalf("console page missing injected auth/theme marker %q", marker)
		}
	}
}

func TestConfigOverlayRoundTripPersists(t *testing.T) {
	dir := t.TempDir()
	yamlText := "data-dir: '" + filepath.ToSlash(dir) + "'\napi-keys:\n  - value: sk-cfg\n"
	f := &fakeCaller{responder: catalogResponder(true, testCatalogJSON)}
	m := NewManager(NewHostBridge(f.call))
	defer func() { _, _ = m.HandleCall(pluginabi.MethodPluginShutdown, nil) }()
	if _, err := m.HandleCall(pluginabi.MethodPluginRegister, lifecycleRequestBody(yamlText)); err != nil {
		t.Fatalf("register: %v", err)
	}

	code, _ := mgmtCall(t, m, http.MethodPut, "/config", map[string]any{
		"base_url":        "https://override.test/provider/v1",
		"request_timeout": "30s",
	}, nil)
	if code != http.StatusOK {
		t.Fatalf("PUT /config status = %d", code)
	}

	_, cfg := mgmtCall(t, m, http.MethodGet, "/config", nil, nil)
	if cfg["base_url"] != "https://override.test/provider/v1" || cfg["request_timeout"] != "30s" {
		t.Fatalf("config after save = %+v", cfg)
	}
	if cfg["catalog_url"] != "https://override.test/provider/v1/models" {
		t.Fatalf("catalog url not recomputed: %v", cfg["catalog_url"])
	}

	if _, err := os.Stat(filepath.Join(dir, settingsFile)); err != nil {
		t.Fatalf("settings.json not persisted: %v", err)
	}

	// A fresh manager reading the same data dir must pick the overlay back up.
	m2 := NewManager(NewHostBridge(f.call))
	defer func() { _, _ = m2.HandleCall(pluginabi.MethodPluginShutdown, nil) }()
	if _, err := m2.HandleCall(pluginabi.MethodPluginRegister, lifecycleRequestBody(yamlText)); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if got := m2.currentConfig().BaseURL; got != "https://override.test/provider/v1" {
		t.Fatalf("reloaded base url = %q, want overlay value", got)
	}
}

func TestPublicationControlsModelListing(t *testing.T) {
	f := &fakeCaller{responder: catalogResponder(true, testCatalogJSON)}
	m := NewManager(NewHostBridge(f.call))
	defer func() { _, _ = m.HandleCall(pluginabi.MethodPluginShutdown, nil) }()
	yamlText := "data-dir: '" + filepath.ToSlash(t.TempDir()) + "'\napi-keys:\n  - value: sk-publish\n"
	if _, err := m.HandleCall(pluginabi.MethodPluginRegister, lifecycleRequestBody(yamlText)); err != nil {
		t.Fatalf("register: %v", err)
	}

	_, listed := mgmtCall(t, m, http.MethodGet, "/models", nil, nil)
	models, _ := listed["models"].([]any)
	if len(models) != 1 {
		t.Fatalf("models = %+v", listed["models"])
	}
	if first := models[0].(map[string]any); first["published"] != true {
		t.Fatalf("model should start published: %+v", first)
	}

	mgmtCall(t, m, http.MethodPut, "/models/publication", map[string]any{
		"disabled_models": []string{"glm-5.3"},
	}, nil)

	_, listed = mgmtCall(t, m, http.MethodGet, "/models", nil, nil)
	if first := listed["models"].([]any)[0].(map[string]any); first["published"] != false {
		t.Fatalf("model should be unpublished: %+v", first)
	}

	// The static model registration must honor the same filter.
	var reg pluginapi.ModelResponse
	decodeResult(t, mustHandle(t, m, pluginabi.MethodModelStatic, nil), &reg)
	if len(reg.Models) != 0 {
		t.Fatalf("static models = %+v, want none after unpublish", reg.Models)
	}
}

func TestCredentialLifecycleWritesAuthFile(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "commandcode-key-abc.json")
	original := `{"type":"commandcode","id":"commandcode-key-abc","label":"Old","api_key":"sk-old-secret-value"}`
	if err := os.WriteFile(authPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return hostOK(hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{{
				ID: "commandcode-key-abc", Name: "commandcode-key-abc.json", Type: ProviderID,
				Provider: ProviderID, Label: "Old", Path: authPath,
			}}}), nil
		case pluginabi.MethodHostAuthGet:
			raw, _ := os.ReadFile(authPath)
			return hostOK(pluginapi.HostAuthGetResponse{AuthIndex: "0", Path: authPath, JSON: raw}), nil
		case pluginabi.MethodHostAuthSave:
			return hostOK(pluginapi.HostAuthSaveResponse{Name: "commandcode-key-new.json", Path: filepath.Join(dir, "commandcode-key-new.json")}), nil
		default:
			return hostOK(map[string]any{}), nil
		}
	}}
	m := NewManager(NewHostBridge(f.call))

	_, list := mgmtCall(t, m, http.MethodGet, "/credentials", nil, nil)
	items, _ := list["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("credentials = %+v", list["items"])
	}
	first := items[0].(map[string]any)
	if first["has_key"] != true || first["key_masked"] == "" {
		t.Fatalf("credential should expose a masked key: %+v", first)
	}
	if s, _ := first["key_masked"].(string); strings.Contains(s, "secret") {
		t.Fatalf("masked key leaked secret material: %q", s)
	}

	// Update label and disable the credential in place.
	code, _ := mgmtCall(t, m, http.MethodPut, "/credentials", map[string]any{
		"id": "commandcode-key-abc", "label": "New name", "disabled": true,
	}, nil)
	if code != http.StatusOK {
		t.Fatalf("PUT /credentials status = %d", code)
	}
	raw, _ := os.ReadFile(authPath)
	if !strings.Contains(string(raw), `"label":"New name"`) || !strings.Contains(string(raw), `"disabled":true`) {
		t.Fatalf("auth file not updated: %s", raw)
	}
	if !strings.Contains(string(raw), "sk-old-secret-value") {
		t.Fatalf("update dropped the api key: %s", raw)
	}

	// Delete removes the backing file.
	code, _ = mgmtCall(t, m, http.MethodDelete, "/credentials", nil, url.Values{"id": {"commandcode-key-abc"}})
	if code != http.StatusOK {
		t.Fatalf("DELETE /credentials status = %d", code)
	}
	if _, err := os.Stat(authPath); !os.IsNotExist(err) {
		t.Fatalf("auth file not removed: %v", err)
	}
}

func TestLogsAndStatisticsReflectTelemetry(t *testing.T) {
	m := NewManager(nil)
	m.telemetry = newTelemetry("")
	m.telemetryRecord(RequestLog{Model: "commandcode/glm-5.3", Status: 200, DurationMS: 10, InputTokens: 100, OutputTokens: 40, CachedTokens: 25})
	m.telemetryRecord(RequestLog{Model: "commandcode/glm-5.3", Status: 200, DurationMS: 30})
	m.telemetryRecord(RequestLog{Model: "commandcode/kimi-k2", Status: 502, Error: "上游返回 HTTP 502", DurationMS: 20})

	_, logs := mgmtCall(t, m, http.MethodGet, "/logs", nil, nil)
	if total, _ := logs["total"].(float64); total != 3 {
		t.Fatalf("logs total = %v, want 3", logs["total"])
	}
	summary := logs["summary"].(map[string]any)
	if summary["requests"].(float64) != 3 || summary["failed"].(float64) != 1 {
		t.Fatalf("summary = %+v", summary)
	}

	_, stats := mgmtCall(t, m, http.MethodGet, "/statistics", nil, nil)
	perModel, _ := stats["models"].([]any)
	if len(perModel) != 2 {
		t.Fatalf("per-model stats = %+v", stats["models"])
	}

	// Status filter narrows the view.
	_, filtered := mgmtCall(t, m, http.MethodGet, "/logs", nil, url.Values{"status": {"error"}})
	if total, _ := filtered["total"].(float64); total != 1 {
		t.Fatalf("error-filtered total = %v, want 1", filtered["total"])
	}

	mgmtCall(t, m, http.MethodPost, "/logs/reset", nil, nil)
	_, after := mgmtCall(t, m, http.MethodGet, "/logs", nil, nil)
	if total, _ := after["total"].(float64); total != 0 {
		t.Fatalf("logs after reset = %v, want 0", after["total"])
	}
}

func TestManagementAcceptsV8Mount(t *testing.T) {
	m := NewManager(nil)
	resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodGet, Path: "/v8/resource/plugins/" + pluginName + "/console",
	})
	if err != nil || resp.StatusCode != http.StatusOK || !strings.Contains(string(resp.Body), apiPrefix) {
		t.Fatalf("v8 console = %+v err=%v", resp, err)
	}
	status, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodGet, Path: "/v8/management/plugins/" + pluginName + "/status",
	})
	if err != nil || status.StatusCode != http.StatusOK {
		t.Fatalf("v8 status = %+v err=%v", status, err)
	}
	var got map[string]any
	if err := json.Unmarshal(status.Body, &got); err != nil || got["provider"] != ProviderID {
		t.Fatalf("v8 status body = %s err=%v", status.Body, err)
	}
}

func TestStatusReportsConfiguredState(t *testing.T) {
	dir := t.TempDir()
	yamlText := "data-dir: '" + filepath.ToSlash(dir) + "'\napi-keys:\n  - value: sk-status\n"
	f := &fakeCaller{responder: catalogResponder(true, testCatalogJSON)}
	m := NewManager(NewHostBridge(f.call))
	defer func() { _, _ = m.HandleCall(pluginabi.MethodPluginShutdown, nil) }()
	if _, err := m.HandleCall(pluginabi.MethodPluginRegister, lifecycleRequestBody(yamlText)); err != nil {
		t.Fatalf("register: %v", err)
	}
	code, status := mgmtCall(t, m, http.MethodGet, "/status", nil, nil)
	if code != http.StatusOK || status["provider"] != ProviderID {
		t.Fatalf("status = %+v code=%d", status, code)
	}
	if status["model_count"].(float64) != 1 {
		t.Fatalf("model_count = %v", status["model_count"])
	}
	if s, ok := status["last_refresh_at"].(string); !ok || !strings.Contains(s, "T") {
		t.Fatalf("last_refresh_at should be an RFC3339 timestamp: %+v", status["last_refresh_at"])
	}
	if time.Since(m.lastRefreshAt) > time.Minute {
		t.Fatal("lastRefreshAt not updated by registration refresh")
	}
}
