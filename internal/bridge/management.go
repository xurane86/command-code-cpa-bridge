package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"command-code-cpa-bridge/internal/config"
	"command-code-cpa-bridge/resources"
)

// The console talks to the Management API under the management plugin suffix
// and is served from the resource suffix. Both are derived from pluginName so
// they always match the built artifact name. The version segment (v0 / v8) is
// matched by suffix so the plugin works under either CPA mount.
const (
	apiPrefix      = "/v0/management/plugins/" + pluginName
	consolePath    = "/v0/resource/plugins/" + pluginName + "/console"
	apiSuffix      = "/management/plugins/" + pluginName
	resourceSuffix = "/resource/plugins/" + pluginName + "/console"
)

// registerManagement declares the console resource and the exact Management
// API routes the page uses.
func (m *Manager) registerManagement(request []byte) ([]byte, error) {
	var req struct {
		Plugin           pluginapi.Metadata `json:"Plugin"`
		BasePath         string             `json:"BasePath"`
		ResourceBasePath string             `json:"ResourceBasePath"`
	}
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed management registration request body"), nil
	}
	type route struct {
		Method string `json:"method"`
		Path   string `json:"path"`
	}
	type resource struct {
		Path        string `json:"path"`
		Menu        string `json:"menu"`
		Description string `json:"description"`
	}
	rel := func(p string) string { return "/plugins/" + pluginName + p }
	routes := []route{
		{Method: "GET", Path: rel("/status")},
		{Method: "GET", Path: rel("/config")},
		{Method: "PUT", Path: rel("/config")},
		{Method: "GET", Path: rel("/models")},
		{Method: "POST", Path: rel("/models/refresh")},
		{Method: "PUT", Path: rel("/models/publication")},
		{Method: "POST", Path: rel("/models/test")},
		{Method: "GET", Path: rel("/reasoning")},
		{Method: "PUT", Path: rel("/reasoning")},
		{Method: "GET", Path: rel("/credentials")},
		{Method: "POST", Path: rel("/credentials")},
		{Method: "PUT", Path: rel("/credentials")},
		{Method: "DELETE", Path: rel("/credentials")},
		{Method: "POST", Path: rel("/quota-usage")},
		{Method: "GET", Path: rel("/logs")},
		{Method: "POST", Path: rel("/logs/reset")},
		{Method: "GET", Path: rel("/statistics")},
	}
	return okEnvelope(struct {
		Routes    []route    `json:"routes"`
		Resources []resource `json:"resources"`
	}{
		Routes: routes,
		Resources: []resource{{
			Path:        "/console",
			Menu:        "CommandCode",
			Description: "CommandCode 凭据、模型、额度与请求监控",
		}},
	}), nil
}

// handleManagement decodes the RPC envelope and forwards to the console
// handler.
func (m *Manager) handleManagement(request []byte) ([]byte, error) {
	var req struct {
		pluginapi.ManagementRequest
		HostCallbackID string `json:"host_callback_id,omitempty"`
	}
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed management request body"), nil
	}
	resp, err := m.HandleManagement(context.Background(), req.ManagementRequest)
	if err != nil {
		return ErrEnvelope("management_failure", err.Error()), nil
	}
	return okEnvelope(resp), nil
}

// HandleManagement serves the console page and its Management API.
func (m *Manager) HandleManagement(ctx context.Context, req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	// Tolerate a trailing slash on either mount.
	reqPath := strings.TrimRight(req.Path, "/")
	if reqPath == "" {
		reqPath = "/"
	}
	if req.Method == http.MethodGet && strings.HasSuffix(reqPath, resourceSuffix) {
		body := strings.ReplaceAll(resources.ConsolePage, "__API_BASE__", apiPrefix)
		body = strings.ReplaceAll(body, "__VERSION__", pluginVersion)
		body = strings.ReplaceAll(body, "/*__AUTH_MODULE__*/", resources.AuthJS)
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"text/html; charset=utf-8"}, "Cache-Control": []string{"no-store"}},
			Body:       []byte(body),
		}, nil
	}

	pos := strings.Index(reqPath, apiSuffix)
	if pos < 0 {
		return jsonResponse(http.StatusNotFound, map[string]any{"error": "not found"}), nil
	}
	suffix := reqPath[pos+len(apiSuffix):]

	switch req.Method + " " + suffix {
	case "GET /status":
		return m.statusResponse(), nil
	case "GET /config":
		return m.configResponse(), nil
	case "PUT /config":
		return m.saveConfigResponse(ctx, req.Body), nil
	case "GET /models":
		return m.modelsResponse(), nil
	case "POST /models/refresh":
		return m.refreshModelsResponse(ctx), nil
	case "PUT /models/publication":
		return m.savePublicationResponse(req.Body), nil
	case "POST /models/test":
		return m.modelTest(ctx, req.Body), nil
	case "GET /reasoning":
		return m.reasoningResponse(), nil
	case "PUT /reasoning":
		return m.saveReasoningResponse(req.Body), nil
	case "GET /credentials":
		return m.credentialsResponse(ctx), nil
	case "POST /credentials":
		return m.addCredentialResponse(ctx, req.Body), nil
	case "PUT /credentials":
		return m.updateCredentialResponse(ctx, req.Body), nil
	case "DELETE /credentials":
		return m.deleteCredentialResponse(ctx, req.Query), nil
	case "POST /quota-usage":
		return m.quotaAPI(ctx, req.Body), nil
	case "GET /logs":
		return m.logsResponse(req.Query), nil
	case "POST /logs/reset":
		m.telemetryReset()
		return jsonResponse(http.StatusOK, map[string]any{"ok": true}), nil
	case "GET /statistics":
		return m.statisticsResponse(), nil
	default:
		return jsonResponse(http.StatusNotFound, map[string]any{"error": "not found"}), nil
	}
}

// ---- helpers ------------------------------------------------------------

func jsonResponse(code int, v any) pluginapi.ManagementResponse {
	body, err := json.Marshal(v)
	if err != nil {
		body = []byte(`{"error":"encoding failed"}`)
		code = http.StatusInternalServerError
	}
	return pluginapi.ManagementResponse{
		StatusCode: code,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}, "Cache-Control": []string{"no-store"}},
		Body:       body,
	}
}

func (m *Manager) telemetryReset() {
	m.mu.RLock()
	tel := m.telemetry
	m.mu.RUnlock()
	if tel != nil {
		tel.reset()
	}
}

// ---- status -------------------------------------------------------------

func (m *Manager) statusResponse() pluginapi.ManagementResponse {
	m.mu.RLock()
	cfg := m.cfg
	lastAt := m.lastRefreshAt
	lastErr := m.lastRefreshError
	modelCount := 0
	if m.mgr != nil {
		modelCount = len(m.mgr.Models())
	}
	settings := m.settings
	dataDir := cfg.DataDir
	m.mu.RUnlock()

	payload := map[string]any{
		"version":             pluginVersion,
		"provider":            ProviderID,
		"model_count":         modelCount,
		"data_dir":            dataDir,
		"yaml_key_count":      len(cfg.APIKeys),
		"prefix_enabled":      cfg.ModelPrefix.Enabled,
		"prefix_value":        cfg.ModelPrefix.Value,
		"base_url":            cfg.BaseURL,
		"last_refresh_error":  lastErr,
		"publication_enabled": len(settings.EnabledModels),
		"publication_hidden":  len(settings.DisabledModels),
	}
	if !lastAt.IsZero() {
		payload["last_refresh_at"] = lastAt
	}
	return jsonResponse(http.StatusOK, payload)
}

// ---- config -------------------------------------------------------------

func (m *Manager) configView(cfg config.Config) map[string]any {
	return map[string]any{
		"base_url":           cfg.BaseURL,
		"catalog_url":        cfg.CatalogURL,
		"model_prefix":       cfg.ModelPrefix,
		"protocols":          cfg.Protocols,
		"allow_http":         cfg.AllowHTTP,
		"request_timeout":    cfg.RequestTimeout.String(),
		"refresh_interval":   cfg.Catalog.RefreshInterval.String(),
		"max_response_bytes": cfg.MaxResponseBytes,
		"data_dir":           cfg.DataDir,
	}
}

func (m *Manager) configResponse() pluginapi.ManagementResponse {
	return jsonResponse(http.StatusOK, m.configView(m.currentConfig()))
}

// saveConfigResponse persists the overlay and reinstalls the catalog + loop.
func (m *Manager) saveConfigResponse(ctx context.Context, body []byte) pluginapi.ManagementResponse {
	var overlay config.Overlay
	if len(body) > 0 {
		if err := json.Unmarshal(body, &overlay); err != nil {
			return jsonResponse(http.StatusBadRequest, map[string]any{"error": "配置格式无效"})
		}
	}

	m.mu.RLock()
	rawYAML := append([]byte(nil), m.rawYAML...)
	settings := m.settings
	dataDir := m.cfg.DataDir
	m.mu.RUnlock()

	settings.Overlay = &overlay
	cfg, err := config.Load(rawYAML)
	if err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": err.Error()})
	}
	cfg, err = cfg.Apply(overlay)
	if err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": err.Error()})
	}
	if err := saveSettings(dataDir, settings); err != nil {
		return jsonResponse(http.StatusInternalServerError, map[string]any{"error": "保存设置失败: " + err.Error()})
	}

	m.mu.Lock()
	m.settings = settings
	m.mu.Unlock()

	m.lifeMu.Lock()
	err = m.install(ctx, cfg)
	m.lifeMu.Unlock()
	if err != nil {
		return jsonResponse(http.StatusBadGateway, map[string]any{"error": "配置已保存，但目录刷新失败: " + err.Error()})
	}
	return jsonResponse(http.StatusOK, map[string]any{"config": m.configView(cfg), "message": "配置已保存并生效"})
}

// ---- models -------------------------------------------------------------

func (m *Manager) modelsResponse() pluginapi.ManagementResponse {
	m.mu.RLock()
	mgr := m.mgr
	settings := m.settings
	m.mu.RUnlock()

	models := []map[string]any{}
	warnings := []string{}
	unsupported := []map[string]any{}
	if mgr != nil {
		for _, rec := range mgr.Models() {
			alias := settings.aliasFor(rec.UpstreamID)
			effective := rec.PublicID
			if alias != "" {
				effective = alias
			}
			models = append(models, map[string]any{
				"public_id":      effective,
				"default_id":     rec.PublicID,
				"alias":          alias,
				"upstream_id":    rec.UpstreamID,
				"display_name":   rec.DisplayName,
				"protocol":       string(rec.Protocol),
				"context_length": rec.ContextLimit,
				"output_limit":   rec.OutputLimit,
				"thinking":       rec.Thinking != nil,
				"published":      settings.modelPublished(rec.UpstreamID),
			})
		}
		warnings = mgr.Warnings()
		for _, u := range mgr.Unsupported() {
			unsupported = append(unsupported, map[string]any{"upstream_id": u.UpstreamID, "reason": u.Reason})
		}
	}
	return jsonResponse(http.StatusOK, map[string]any{
		"models":          models,
		"warnings":        warnings,
		"unsupported":     unsupported,
		"enabled_models":  settings.EnabledModels,
		"disabled_models": settings.DisabledModels,
		"model_aliases":   settings.ModelAliases,
		"prefix_enabled":  m.currentConfig().ModelPrefix.Enabled,
	})
}

func (m *Manager) refreshModelsResponse(ctx context.Context) pluginapi.ManagementResponse {
	m.mu.RLock()
	mgr := m.mgr
	cfg := m.cfg
	m.mu.RUnlock()
	if mgr == nil {
		return jsonResponse(http.StatusServiceUnavailable, map[string]any{"error": "目录尚未初始化"})
	}
	refreshCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
	defer cancel()
	if err := m.refreshOnce(refreshCtx, mgr, m.bridge, cfg.RequestTimeout, cfg); err != nil {
		return jsonResponse(http.StatusBadGateway, map[string]any{"error": "刷新目录失败: " + err.Error()})
	}
	resp := m.modelsResponse()
	return resp
}

func (m *Manager) savePublicationResponse(body []byte) pluginapi.ManagementResponse {
	var in struct {
		EnabledModels  []string          `json:"enabled_models"`
		DisabledModels []string          `json:"disabled_models"`
		ModelAliases   map[string]string `json:"model_aliases"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": "发布配置格式无效"})
	}
	aliases, aliasErr := m.validateModelAliases(in.ModelAliases)
	if aliasErr != "" {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": aliasErr})
	}
	m.mu.Lock()
	settings := m.settings
	settings.EnabledModels = cleanIDs(in.EnabledModels)
	settings.DisabledModels = cleanIDs(in.DisabledModels)
	if in.ModelAliases != nil {
		settings.ModelAliases = aliases
	}
	m.settings = settings
	dataDir := m.cfg.DataDir
	m.mu.Unlock()

	if err := saveSettings(dataDir, settings); err != nil {
		return jsonResponse(http.StatusInternalServerError, map[string]any{"error": "保存发布配置失败"})
	}
	// Nudge the host to re-query model.static so the change is visible.
	m.nudgeModels()
	return m.modelsResponse()
}

// validateModelAliases normalizes the incoming upstream→client-name map and
// checks it against the live catalog so a saved alias cannot shadow another
// model's id or an existing alias. A nil input means "leave aliases unchanged".
func (m *Manager) validateModelAliases(raw map[string]string) (map[string]string, string) {
	if raw == nil {
		return nil, ""
	}
	m.mu.RLock()
	mgr := m.mgr
	m.mu.RUnlock()

	// reserved maps every id a client may already call (upstream id and default
	// public id) to the model that owns it.
	reserved := map[string]string{}
	defaults := map[string]string{}
	if mgr != nil {
		for _, rec := range mgr.Models() {
			reserved[rec.UpstreamID] = rec.UpstreamID
			reserved[rec.PublicID] = rec.UpstreamID
			defaults[rec.UpstreamID] = rec.PublicID
		}
	}
	out := map[string]string{}
	owner := map[string]string{}
	for up, a := range raw {
		up = strings.TrimSpace(up)
		a = strings.TrimSpace(a)
		if up == "" || a == "" {
			continue
		}
		if a == defaults[up] {
			// Alias identical to the model's own default id is a no-op.
			continue
		}
		if ownerID, clash := reserved[a]; clash && ownerID != up {
			return nil, "客户端模型名「" + a + "」与模型「" + ownerID + "」的 ID 冲突，请换一个名字"
		}
		if prev, dup := owner[a]; dup && prev != up {
			return nil, "客户端模型名「" + a + "」被多个模型占用，请改成唯一名字"
		}
		owner[a] = up
		out[up] = a
	}
	return out, ""
}

func cleanIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// ---- reasoning effort mapping ------------------------------------------

// reasoningLevels are the effort values the console offers as suggestions.
// "omit" drops the field entirely.
var reasoningLevels = []string{"low", "medium", "high", "xhigh", "max", "auto", "none", "omit"}

func (m *Manager) reasoningResponse() pluginapi.ManagementResponse {
	m.mu.RLock()
	settings := m.settings
	mgr := m.mgr
	m.mu.RUnlock()
	models := []string{}
	if mgr != nil {
		for _, rec := range mgr.Models() {
			models = append(models, rec.UpstreamID)
		}
	}
	rules := settings.ReasoningRules
	if rules == nil {
		rules = []ReasoningRule{}
	}
	return jsonResponse(http.StatusOK, map[string]any{
		"rules":  rules,
		"levels": reasoningLevels,
		"models": models,
	})
}

func (m *Manager) saveReasoningResponse(body []byte) pluginapi.ManagementResponse {
	var in struct {
		Rules []ReasoningRule `json:"rules"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": "思考强度映射格式无效"})
	}
	if len(in.Rules) > 200 {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": "规则数量过多（上限 200）"})
	}
	rules := make([]ReasoningRule, 0, len(in.Rules))
	for _, r := range in.Rules {
		from := strings.TrimSpace(r.From)
		to := strings.TrimSpace(r.To)
		if from == "" || to == "" {
			continue
		}
		rules = append(rules, ReasoningRule{Model: strings.TrimSpace(r.Model), From: from, To: to})
	}
	m.mu.Lock()
	settings := m.settings
	settings.ReasoningRules = rules
	m.settings = settings
	dataDir := m.cfg.DataDir
	m.mu.Unlock()
	if err := saveSettings(dataDir, settings); err != nil {
		return jsonResponse(http.StatusInternalServerError, map[string]any{"error": "保存思考强度映射失败"})
	}
	return jsonResponse(http.StatusOK, map[string]any{"rules": rules, "message": "思考强度映射已保存"})
}

// ---- credentials --------------------------------------------------------

func (m *Manager) credentialsResponse(ctx context.Context) pluginapi.ManagementResponse {
	items, err := m.listCredentials(ctx)
	if err != nil {
		return jsonResponse(http.StatusBadGateway, map[string]any{"error": err.Error(), "items": []any{}})
	}
	return jsonResponse(http.StatusOK, map[string]any{"items": items})
}

func (m *Manager) addCredentialResponse(ctx context.Context, body []byte) pluginapi.ManagementResponse {
	var in credentialInput
	if err := json.Unmarshal(body, &in); err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": "凭据格式无效"})
	}
	name, err := m.addCredential(ctx, in)
	if err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": err.Error()})
	}
	m.nudgeModels()
	return jsonResponse(http.StatusCreated, map[string]any{"name": name, "message": "凭据已添加"})
}

func (m *Manager) updateCredentialResponse(ctx context.Context, body []byte) pluginapi.ManagementResponse {
	var in credentialInput
	if err := json.Unmarshal(body, &in); err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": "凭据格式无效"})
	}
	if err := m.updateCredential(ctx, in); err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": err.Error()})
	}
	return jsonResponse(http.StatusOK, map[string]any{"message": "凭据已更新"})
}

func (m *Manager) deleteCredentialResponse(ctx context.Context, query url.Values) pluginapi.ManagementResponse {
	if err := m.deleteCredential(ctx, query.Get("id")); err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": err.Error()})
	}
	return jsonResponse(http.StatusOK, map[string]any{"message": "凭据已删除"})
}

// ---- logs & statistics --------------------------------------------------

func (m *Manager) logsResponse(query url.Values) pluginapi.ManagementResponse {
	limit := atoiDefault(query.Get("limit"), 50)
	offset := atoiDefault(query.Get("offset"), 0)
	m.mu.RLock()
	tel := m.telemetry
	m.mu.RUnlock()
	if tel == nil {
		return jsonResponse(http.StatusOK, map[string]any{"items": []any{}, "total": 0, "summary": logSummary{}})
	}
	items, total, summary := tel.query(limit, offset, query.Get("search"), query.Get("status"))
	return jsonResponse(http.StatusOK, map[string]any{"items": items, "total": total, "summary": summary})
}

func (m *Manager) statisticsResponse() pluginapi.ManagementResponse {
	m.mu.RLock()
	tel := m.telemetry
	m.mu.RUnlock()
	if tel == nil {
		return jsonResponse(http.StatusOK, map[string]any{"summary": logSummary{}, "models": []any{}})
	}
	summary, models := tel.stats()
	return jsonResponse(http.StatusOK, map[string]any{"summary": summary, "models": models})
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return def
		}
		n = n*10 + int(r-'0')
		if n > 1000000 {
			return def
		}
	}
	return n
}

// nudgeModels re-saves each CommandCode auth file with a revision stamp so the
// host's watcher re-parses it and re-queries the plugin's static models.
func (m *Manager) nudgeModels() {
	if m.bridge == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entries, err := m.authEntries(ctx)
	if err != nil {
		return
	}
	revision := time.Now().UTC().Format(time.RFC3339Nano)
	for _, e := range entries {
		path := strings.TrimSpace(e.Path)
		if path != "" {
			touchAuthRevision(path, revision)
		}
	}
}
