// Package bridge implements the CLIProxyAPI method dispatcher for the
// CommandCode provider: registration, reconfiguration, model publication,
// catalog refresh scheduling, host-callback bridging, and the console
// management API (credentials, catalog, quota, logs, settings).
package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"command-code-cpa-bridge/internal/config"
	"command-code-cpa-bridge/internal/fault"
	"command-code-cpa-bridge/internal/registry"
)

// ProviderID is the single provider key served by this plugin. It is also the
// value written as the auth record type/provider field.
const ProviderID = "commandcode"

// pluginName / pluginVersion are reported in registration metadata. pluginName
// must match the built artifact base name: it is the management resource and
// console URL segment.
const (
	pluginName    = "command-code-cpa-bridge"
	pluginVersion = "0.2.2"
)

// githubRepoURL satisfies the host's validPlugin gate and is the address the
// plugin store polls for updates.
const githubRepoURL = "https://github.com/xurane86/command-code-cpa-bridge"

// registerRefreshTimeout bounds ONLY the synchronous initial/reconfigure
// refreshOnce so a slow catalog cannot block host startup for a full
// request-timeout. On expiry registration proceeds; the ticker retries.
const registerRefreshTimeout = 10 * time.Second

// Manager owns dispatcher state (config snapshot, page settings, telemetry,
// catalog manager, refresh loop) and routes every RPC method. Safe for
// concurrent HandleCall use.
type Manager struct {
	bridge *HostBridge // immutable after NewManager

	// lifeMu serializes whole register/reconfigure/shutdown sequences so
	// their stop-wait-install steps cannot interleave into orphaned tickers.
	lifeMu sync.Mutex

	mu               sync.RWMutex
	cfg              config.Config
	mgr              *registry.Manager
	rawYAML          []byte
	settings         pageSettings
	telemetry        *telemetry
	lastRefreshAt    time.Time
	lastRefreshError string

	// stop/done manage the one background refresh goroutine; both nil
	// when no loop is running.
	stop chan struct{}
	done chan struct{}
}

// NewManager returns a dispatcher whose outbound traffic flows through bridge.
func NewManager(bridge *HostBridge) *Manager {
	return &Manager{bridge: bridge, telemetry: newTelemetry("")}
}

// HandleCall dispatches one RPC method and returns envelope bytes. Handler
// failures travel inside the envelope; a recovered panic becomes a
// "plugin_error" envelope so the host process never dies with us.
func (m *Manager) HandleCall(method string, request []byte) (resp []byte, err error) {
	debugTrace("handle method=%s request_bytes=%d", method, len(request))
	defer func() {
		if r := recover(); r != nil {
			resp = ErrEnvelope("plugin_error", fmt.Sprintf("internal error handling %s", method))
			err = nil
		}
	}()
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return m.handleLifecycle(request)
	case pluginabi.MethodModelStatic, pluginabi.MethodModelForAuth:
		return m.handleModels()
	case pluginabi.MethodPluginShutdown:
		return m.handleShutdown()
	case pluginabi.MethodExecutorExecute:
		return m.handleExecute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return m.handleExecuteStream(request)
	case pluginabi.MethodAuthIdentifier:
		return okEnvelope(map[string]string{"identifier": ProviderID}), nil
	case pluginabi.MethodAuthParse:
		var req pluginapi.AuthParseRequest
		if json.Unmarshal(request, &req) != nil {
			return ErrEnvelope("invalid_request", "malformed auth parse request body"), nil
		}
		resp, err := (authProvider{}).ParseAuth(context.Background(), req)
		if err != nil {
			return ErrEnvelope("auth_failure", err.Error()), nil
		}
		return okEnvelope(resp), nil
	case pluginabi.MethodAuthLoginStart:
		var req struct {
			pluginapi.AuthLoginStartRequest
		}
		if json.Unmarshal(request, &req) != nil {
			return ErrEnvelope("invalid_request", "malformed auth login request body"), nil
		}
		_, err := (authProvider{}).StartLogin(context.Background(), req.AuthLoginStartRequest)
		return ErrEnvelope("unsupported", err.Error()), nil
	case pluginabi.MethodAuthLoginPoll:
		var req struct{ pluginapi.AuthLoginPollRequest }
		if json.Unmarshal(request, &req) != nil {
			return ErrEnvelope("invalid_request", "malformed auth poll request body"), nil
		}
		_, err := (authProvider{}).PollLogin(context.Background(), req.AuthLoginPollRequest)
		return ErrEnvelope("unsupported", err.Error()), nil
	case pluginabi.MethodAuthRefresh:
		var req struct{ pluginapi.AuthRefreshRequest }
		if json.Unmarshal(request, &req) != nil {
			return ErrEnvelope("invalid_request", "malformed auth refresh request body"), nil
		}
		resp, err := (authProvider{}).RefreshAuth(context.Background(), req.AuthRefreshRequest)
		if err != nil {
			return ErrEnvelope("auth_failure", err.Error()), nil
		}
		return okEnvelope(resp), nil
	case pluginabi.MethodManagementRegister:
		return m.registerManagement(request)
	case pluginabi.MethodManagementHandle:
		return m.handleManagement(request)
	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(map[string]string{"identifier": ProviderID}), nil
	case pluginabi.MethodExecutorCountTokens:
		return classEnvelope(&fault.Error{
			Class:   fault.ClassUnsupported,
			Message: "executor.count_tokens has no CommandCode equivalent",
		}), nil
	case pluginabi.MethodExecutorHTTPRequest:
		return classEnvelope(&fault.Error{
			Class:   fault.ClassUnsupported,
			Message: fmt.Sprintf("%s endpoint is not supported by commandcode", pluginabi.MethodExecutorHTTPRequest),
		}), nil
	default:
		return ErrEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// lifecycleRequest mirrors rpcLifecycleRequest: config_yaml is base64.
type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type capabilities struct {
	ModelProvider         bool                         `json:"model_provider"`
	AuthProvider          bool                         `json:"auth_provider"`
	Executor              bool                         `json:"executor"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope,omitempty"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats,omitempty"`
	ManagementAPI         bool                         `json:"management_api"`
}

type registrationResult struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  capabilities       `json:"capabilities"`
}

func registrationEnvelope() []byte {
	formats := []string{"openai", "claude", "openai-response"}
	return okEnvelope(registrationResult{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           "command-code",
			GitHubRepository: githubRepoURL,
			ConfigFields: []pluginapi.ConfigField{
				{Name: "data-dir", Type: pluginapi.ConfigFieldTypeString, Description: "Persistent plugin state directory (default plugins/command-code-cpa-bridge-data)"},
				{Name: "base-url", Type: pluginapi.ConfigFieldTypeString, Description: "CommandCode provider base URL (default https://api.commandcode.ai/provider/v1)"},
				{Name: "api-keys", Type: pluginapi.ConfigFieldTypeArray, Description: "Optional static API keys; keys are normally managed from the console"},
			},
		},
		Capabilities: capabilities{
			ModelProvider:         true,
			AuthProvider:          true,
			Executor:              true,
			ExecutorModelScope:    pluginapi.ExecutorModelScopeOAuth,
			ExecutorInputFormats:  formats,
			ExecutorOutputFormats: formats,
			ManagementAPI:         true,
		},
	})
}

// handleLifecycle implements plugin.register / plugin.reconfigure: load
// config + page settings, refresh the catalog once, publish state, restart
// the refresh loop. Registration succeeds even when the initial refresh fails.
func (m *Manager) handleLifecycle(request []byte) ([]byte, error) {
	var req lifecycleRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed lifecycle request body"), nil
	}
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()

	cfg, settings, err := buildConfig(req.ConfigYAML)
	if err != nil {
		debugTrace("lifecycle config_error=%s", err.Error())
		return ErrEnvelope("invalid_config", err.Error()), nil
	}
	debugTrace("lifecycle config_loaded key_count=%d prefix_enabled=%t prefix=%s data_dir=%s", len(cfg.APIKeys), cfg.ModelPrefix.Enabled, cfg.ModelPrefix.Value, cfg.DataDir)

	m.mu.Lock()
	m.rawYAML = req.ConfigYAML
	m.settings = settings
	m.telemetry = newTelemetry(cfg.DataDir)
	m.mu.Unlock()

	if err := m.install(context.Background(), cfg); err != nil {
		return ErrEnvelope("auth_materialization_failed", err.Error()), nil
	}
	return registrationEnvelope(), nil
}

// buildConfig loads the YAML config and applies the persisted page overlay.
func buildConfig(rawYAML []byte) (config.Config, pageSettings, error) {
	cfg, err := config.Load(rawYAML)
	if err != nil {
		return config.Config{}, pageSettings{}, err
	}
	settings, err := loadSettings(cfg.DataDir)
	if err != nil {
		return config.Config{}, pageSettings{}, err
	}
	if settings.Overlay != nil {
		cfg, err = cfg.Apply(*settings.Overlay)
		if err != nil {
			return config.Config{}, pageSettings{}, err
		}
	}
	return cfg, settings, nil
}

// install materializes auth records for YAML keys, (re)builds the catalog
// snapshot, and (re)starts the refresh loop for cfg. The caller holds lifeMu.
func (m *Manager) install(ctx context.Context, cfg config.Config) error {
	if err := m.materializeAuthRecords(ctx, cfg); err != nil {
		return err
	}
	var client registry.HostClient
	if m.bridge != nil {
		client = m.bridge
	}
	mgr := registry.New(cfg, client)

	refreshCtx, cancel := context.WithTimeout(ctx, registerRefreshTimeout)
	refreshErr := m.refreshOnce(refreshCtx, mgr, m.bridge, registerRefreshTimeout, cfg)
	cancel()
	debugTrace("lifecycle refresh_complete model_count=%d refresh_error=%t", len(mgr.Models()), refreshErr != nil)

	if oldDone := m.closeStop(); oldDone != nil {
		<-oldDone
	}

	m.mu.Lock()
	m.cfg = cfg
	if refreshErr != nil && cfg.Catalog.StaleWhileUnavailable && m.mgr != nil && len(m.mgr.Models()) > 0 {
		mgr.SeedFrom(m.mgr)
	}
	m.mgr = mgr
	stop, done := make(chan struct{}), make(chan struct{})
	m.stop, m.done = stop, done
	interval := cfg.Catalog.RefreshInterval
	m.mu.Unlock()

	m.startRefreshLoop(cfg, mgr, interval, stop, done)
	return nil
}

// currentConfig returns the live config snapshot.
func (m *Manager) currentConfig() config.Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg
}

// currentSettings returns the live page settings snapshot.
func (m *Manager) currentSettings() pageSettings {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.settings
}

// authKeyHash is the non-secret digest that identifies one config key. It is
// independent of config ordering, which keeps the records derived from it
// idempotent across restarts.
func authKeyHash(key string) string {
	digest := sha256.Sum256([]byte(key))
	return hex.EncodeToString(digest[:])
}

func authRecordIDFromHash(hash string) string { return ProviderID + "-key-" + hash }
func authFileNameFromHash(hash string) string { return authRecordIDFromHash(hash) + ".json" }

// materializeAuthRecords makes CPA-visible auth files for YAML-configured keys
// idempotently. Console-managed keys are already auth files and are left
// untouched. The host ABI has no delete callback, so removed YAML keys remain
// stale records and are not claimed as removed.
func (m *Manager) materializeAuthRecords(ctx context.Context, cfg config.Config) error {
	if m.bridge == nil || len(cfg.APIKeys) == 0 {
		return nil
	}
	entries, err := m.bridge.AuthList(ctx)
	if err != nil {
		return fmt.Errorf("list existing auth records: %w", err)
	}
	existing := make(map[string]struct{}, len(entries)*2)
	for _, entry := range entries {
		if name := strings.TrimSpace(entry.Name); name != "" {
			existing[name] = struct{}{}
		}
		if id := strings.TrimSpace(entry.ID); id != "" {
			existing[id] = struct{}{}
		}
	}
	for _, key := range cfg.APIKeys {
		hash := authKeyHash(key.Value)
		id := authRecordIDFromHash(hash)
		name := authFileNameFromHash(hash)
		if _, ok := existing[id]; ok {
			continue
		}
		if _, ok := existing[name]; ok {
			continue
		}
		record, err := json.Marshal(credentialRecord{
			Type: ProviderID, Provider: ProviderID, ID: id, Label: "CommandCode credential " + hash, APIKey: key.Value,
		})
		if err != nil {
			return fmt.Errorf("build auth record")
		}
		if _, err := m.bridge.AuthSave(ctx, pluginapi.HostAuthSaveRequest{Name: name, JSON: record}); err != nil {
			return err
		}
		existing[id] = struct{}{}
		existing[name] = struct{}{}
		debugTrace("auth materialized id=%s file=%s", id, name)
	}
	return nil
}

// handleModels implements model.static / model.for_auth: the last good catalog
// snapshot mapped to wire ModelInfos, minus models hidden by the page
// publication allowlist/blocklist. An empty catalog yields an empty slice.
func (m *Manager) handleModels() ([]byte, error) {
	m.mu.RLock()
	mgr := m.mgr
	settings := m.settings
	m.mu.RUnlock()
	models := make([]pluginapi.ModelInfo, 0)
	if mgr != nil {
		for _, rec := range mgr.Models() {
			if !settings.modelPublished(rec.UpstreamID) {
				continue
			}
			id := rec.PublicID
			if a := settings.aliasFor(rec.UpstreamID); a != "" {
				id = a
			}
			models = append(models, pluginapi.ModelInfo{
				ID:                        id,
				Object:                    "model",
				OwnedBy:                   ProviderID,
				DisplayName:               rec.DisplayName,
				ContextLength:             rec.ContextLimit,
				MaxCompletionTokens:       rec.OutputLimit,
				SupportedInputModalities:  rec.InputModes,
				SupportedOutputModalities: rec.OutputModes,
				Thinking:                  rec.Thinking,
			})
		}
	}
	return okEnvelope(pluginapi.ModelResponse{Provider: ProviderID, Models: models}), nil
}

// shutdownDrainTimeout bounds how long handleShutdown waits for orphaned
// host-callback goroutines before returning.
var shutdownDrainTimeout = 15 * time.Second

// handleShutdown stops the refresh loop, drains orphaned host callbacks, and
// clears state.
func (m *Manager) handleShutdown() ([]byte, error) {
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	oldDone := m.closeStop()
	if oldDone != nil {
		<-oldDone
	}
	m.mu.RLock()
	tel := m.telemetry
	m.mu.RUnlock()
	if tel != nil {
		tel.flush()
	}
	if m.bridge != nil && !m.bridge.WaitForInFlight(shutdownDrainTimeout) {
		_ = m.bridge.Log("warn", "shutdown proceeding with host callbacks still in flight", nil)
	}
	m.mu.Lock()
	m.cfg, m.mgr = config.Config{}, nil
	m.mu.Unlock()
	return okEnvelope(struct{}{}), nil
}

// startRefreshLoop spawns the single background ticker goroutine over the
// caller-supplied stop/done pair (already stored under m.mu).
func (m *Manager) startRefreshLoop(cfg config.Config, mgr *registry.Manager, interval time.Duration, stop, done chan struct{}) {
	stopCtx, cancel := context.WithCancel(context.Background())
	go func() {
		<-stop
		cancel()
	}()
	go func() {
		ticker := time.NewTicker(interval)
		defer close(done)
		defer ticker.Stop()
		defer cancel()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				func() {
					defer func() {
						if r := recover(); r != nil && m.bridge != nil {
							_ = m.bridge.Log("error", "catalog refresh panicked", nil)
						}
					}()
					m.refreshOnce(stopCtx, mgr, m.bridge, cfg.RequestTimeout, cfg)
				}()
			}
		}
	}()
}

// closeStop signals a running loop to exit and clears the stop/done pair.
func (m *Manager) closeStop() chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stop == nil {
		return nil
	}
	done := m.done
	close(m.stop)
	m.stop, m.done = nil, nil
	return done
}

// refreshOnce runs one bounded catalog refresh. The key is taken from the
// YAML config when present, otherwise from the host auth records so a fully
// page-managed key pool still drives discovery.
func (m *Manager) refreshOnce(parent context.Context, mgr *registry.Manager, bridge *HostBridge, timeout time.Duration, cfg config.Config) error {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	key, kerr := m.catalogKey(ctx, cfg)
	if kerr != nil {
		if bridge != nil {
			_ = bridge.Log("warn", "catalog refresh skipped", map[string]any{"error": kerr.Error()})
		}
		return kerr
	}
	err := mgr.Refresh(ctx, key)
	if err != nil {
		m.mu.Lock()
		m.lastRefreshAt = time.Now().UTC()
		m.lastRefreshError = err.Error()
		m.mu.Unlock()
		if bridge != nil {
			_ = bridge.Log("warn", "catalog refresh failed", map[string]any{"error": err.Error()})
		}
		return err
	}
	m.mu.Lock()
	m.lastRefreshAt = time.Now().UTC()
	m.lastRefreshError = ""
	m.mu.Unlock()
	warnings := mgr.Warnings()
	if uns := mgr.Unsupported(); len(uns) > 0 {
		parts := make([]string, len(uns))
		for i, u := range uns {
			parts[i] = fmt.Sprintf("%q: %s", u.UpstreamID, u.Reason)
		}
		fields := map[string]any{"models": strings.Join(parts, ", ")}
		if len(warnings) > 0 {
			fields["warnings"] = strings.Join(warnings, "; ")
		}
		_ = bridge.Log("warn", "unsupported models excluded from routable catalog", fields)
	} else if len(warnings) > 0 {
		_ = bridge.Log("warn", "catalog normalization warnings",
			map[string]any{"warnings": strings.Join(warnings, "; ")})
	}
	return nil
}

func okEnvelope(result any) []byte {
	raw, _ := json.Marshal(result)
	out, _ := json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
	return out
}

func ErrEnvelope(code, message string) []byte {
	out, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: &pluginabi.Error{Code: code, Message: message}})
	return out
}
