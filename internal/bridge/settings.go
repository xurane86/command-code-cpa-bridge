package bridge

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"command-code-cpa-bridge/internal/config"
)

// settingsFile is the page-managed state persisted under data-dir. It holds
// only non-secret preferences: the config overlay and the model publication
// allowlist. Credentials live as host auth records, never here.
const settingsFile = "settings.json"

// pageSettings is the persisted console state.
type pageSettings struct {
	// Overlay wins over the YAML config for every field it sets.
	Overlay *config.Overlay `json:"overlay,omitempty"`
	// EnabledModels is the publication allowlist of upstream model IDs. Empty
	// means "publish every model the catalog discovers".
	EnabledModels []string `json:"enabled_models,omitempty"`
	// DisabledModels is the publication blocklist of upstream model IDs. It is
	// applied after EnabledModels so a single model can be hidden without
	// enumerating the allowlist.
	DisabledModels []string `json:"disabled_models,omitempty"`
	// ReasoningRules rewrite a client-declared reasoning effort before the
	// upstream request is sent (e.g. medium -> high). A rule with an empty
	// Model is the default; a rule with a matching Model wins.
	ReasoningRules []ReasoningRule `json:"reasoning_rules,omitempty"`
	// ModelAliases maps an upstream model ID to the client-facing name that
	// replaces it in model listings and is accepted on inbound requests. The
	// upstream ID keeps working alongside the alias. An empty alias means
	// "use the id derived from the prefix setting".
	ModelAliases map[string]string `json:"model_aliases,omitempty"`
}

// ReasoningRule maps one client-declared reasoning effort to an upstream one.
// To may be "omit" to drop the field entirely.
type ReasoningRule struct {
	Model string `json:"model,omitempty"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// mapReasoningEffort returns the upstream effort for a client-declared effort,
// preferring a model-specific rule over the default, or "" when no rule
// matches.
func (s pageSettings) mapReasoningEffort(model, effort string) string {
	if effort == "" {
		return ""
	}
	fallback := ""
	for _, r := range s.ReasoningRules {
		if r.From == "" || !strings.EqualFold(strings.TrimSpace(r.From), effort) {
			continue
		}
		if r.Model != "" && strings.EqualFold(strings.TrimSpace(r.Model), model) {
			return strings.TrimSpace(r.To)
		}
		if r.Model == "" && fallback == "" {
			fallback = strings.TrimSpace(r.To)
		}
	}
	return fallback
}

// loadSettings reads the page settings from dir. A missing file yields zero
// settings; a corrupt file is reported so the caller can decide.
func loadSettings(dir string) (pageSettings, error) {
	var st pageSettings
	if strings.TrimSpace(dir) == "" {
		return st, nil
	}
	raw, err := os.ReadFile(filepath.Join(dir, settingsFile))
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return st, err
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return pageSettings{}, fmt.Errorf("settings.json is not valid JSON")
	}
	return st, nil
}

// saveSettings writes st atomically under dir. An empty dir is a no-op: the
// page still works in memory, it just cannot survive a restart.
func saveSettings(dir string, st pageSettings) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".settings-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, filepath.Join(dir, settingsFile))
}

// modelPublished reports whether an upstream model ID passes the publication
// allowlist and blocklist.
func (s pageSettings) modelPublished(upstreamID string) bool {
	for _, id := range s.DisabledModels {
		if id == upstreamID {
			return false
		}
	}
	if len(s.EnabledModels) == 0 {
		return true
	}
	for _, id := range s.EnabledModels {
		if id == upstreamID {
			return true
		}
	}
	return false
}

// aliasFor returns the configured client-facing name for an upstream model ID,
// or "" when no alias is set.
func (s pageSettings) aliasFor(upstreamID string) string {
	if s.ModelAliases == nil {
		return ""
	}
	return strings.TrimSpace(s.ModelAliases[upstreamID])
}

// upstreamForAlias returns the upstream model ID an incoming model name maps to
// when that name is a configured alias, or "" when it is not an alias.
func (s pageSettings) upstreamForAlias(model string) string {
	model = strings.TrimSpace(model)
	if model == "" || len(s.ModelAliases) == 0 {
		return ""
	}
	for upstream, alias := range s.ModelAliases {
		if strings.TrimSpace(alias) == model {
			return upstream
		}
	}
	return ""
}
