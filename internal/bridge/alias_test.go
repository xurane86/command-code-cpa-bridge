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

// registeredAliasManager registers a manager backed by a temp data dir so page
// settings (aliases) persist, with a fake host that answers the catalog and a
// chat-completions test call.
func registeredAliasManager(t *testing.T, dir string) *Manager {
	t.Helper()
	f := &fakeCaller{responder: testResponder(false)}
	m := NewManager(NewHostBridge(f.call))
	yamlText := "data-dir: '" + filepath.ToSlash(dir) + "'\napi-keys:\n  - value: sk-alias\n"
	if _, err := m.HandleCall(pluginabi.MethodPluginRegister, lifecycleRequestBody(yamlText)); err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() { _, _ = m.HandleCall(pluginabi.MethodPluginShutdown, nil) })
	return m
}

func modelEntry(t *testing.T, m *Manager, upstream string) map[string]any {
	t.Helper()
	_, listed := mgmtCall(t, m, http.MethodGet, "/models", nil, nil)
	models, _ := listed["models"].([]any)
	for _, raw := range models {
		e, _ := raw.(map[string]any)
		if e["upstream_id"] == upstream {
			return e
		}
	}
	t.Fatalf("model %q not in listing: %+v", upstream, listed["models"])
	return nil
}

// TestModelAliasEditsListingAndResolves covers the mapping contract: without an
// alias the client name is the bare upstream id; after saving one the alias is
// advertised and callable, and the original upstream id keeps working.
func TestModelAliasEditsListingAndResolves(t *testing.T) {
	m := registeredAliasManager(t, t.TempDir())

	if e := modelEntry(t, m, "glm-5.3"); e["alias"] != "" || e["public_id"] != "glm-5.3" {
		t.Fatalf("default listing = %+v, want bare upstream id", e)
	}

	code, _ := mgmtCall(t, m, http.MethodPut, "/models/publication", map[string]any{
		"model_aliases": map[string]string{"glm-5.3": "my-glm"},
	}, nil)
	if code != http.StatusOK {
		t.Fatalf("save aliases status = %d", code)
	}

	if e := modelEntry(t, m, "glm-5.3"); e["alias"] != "my-glm" || e["public_id"] != "my-glm" {
		t.Fatalf("alias not applied to listing: %+v", e)
	}

	// model.static advertises the alias, not the upstream id.
	var reg pluginapi.ModelResponse
	decodeResult(t, mustHandle(t, m, pluginabi.MethodModelStatic, nil), &reg)
	if len(reg.Models) != 1 || reg.Models[0].ID != "my-glm" {
		t.Fatalf("static ids = %+v, want [my-glm]", reg.Models)
	}

	// Both the alias and the original upstream id resolve.
	if code, _ := mgmtCall(t, m, http.MethodPost, "/models/test", map[string]any{"model": "my-glm"}, nil); code != http.StatusOK {
		t.Fatalf("alias model test status = %d", code)
	}
	if code, _ := mgmtCall(t, m, http.MethodPost, "/models/test", map[string]any{"model": "glm-5.3"}, nil); code != http.StatusOK {
		t.Fatalf("upstream model test status = %d", code)
	}
}

// TestModelAliasResolvesOnExecuteAndPersists covers the request path and
// durability: an alias resolves on executor.execute, and a fresh manager on the
// same data dir reloads it.
func TestModelAliasResolvesOnExecuteAndPersists(t *testing.T) {
	dir := t.TempDir()
	m := registeredAliasManager(t, dir)
	code, _ := mgmtCall(t, m, http.MethodPut, "/models/publication", map[string]any{
		"model_aliases": map[string]string{"glm-5.3": "exec-alias"},
	}, nil)
	if code != http.StatusOK {
		t.Fatalf("save aliases status = %d", code)
	}

	body, _ := json.Marshal(map[string]any{
		"model":    "exec-alias",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	resp := mustHandle(t, m, "executor.execute", execReqBodyWithKey("exec-alias", "openai", body, false, "sk-alias"))
	if env := decodeEnv(t, resp); !env.OK {
		t.Fatalf("alias execute failed: %+v", env.Error)
	}

	m2 := registeredAliasManager(t, dir)
	if e := modelEntry(t, m2, "glm-5.3"); e["alias"] != "exec-alias" {
		t.Fatalf("alias not persisted across restart: %+v", e)
	}
}

// TestModelAliasRejectsCollision pins that an alias cannot shadow another
// model's callable id.
func TestModelAliasRejectsCollision(t *testing.T) {
	f := &fakeCaller{responder: catalogResponder(true, `{"data":[{"id":"glm-5.3"},{"id":"kimi-k2"}]}`)}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall(pluginabi.MethodPluginShutdown, nil) })
	if _, err := m.HandleCall(pluginabi.MethodPluginRegister, lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}

	// "kimi-k2" is another model's upstream id.
	code, got := mgmtCall(t, m, http.MethodPut, "/models/publication", map[string]any{
		"model_aliases": map[string]string{"glm-5.3": "kimi-k2"},
	}, nil)
	if code != http.StatusBadRequest || !strings.Contains(got["error"].(string), "冲突") {
		t.Fatalf("collision accepted: status=%d body=%+v", code, got)
	}
}
