package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"command-code-cpa-bridge/internal/config"
)

// The console manages CommandCode credentials as host auth records so CPA's
// scheduler, cooldowns, and per-key statistics keep working unchanged. This
// file is the only place that turns page input into auth files.

// credentialRecord is the JSON persisted to an auth file. It mirrors the shape
// CommandCode's ParseAuth understands.
type credentialRecord struct {
	Type     string `json:"type"`
	Provider string `json:"provider"`
	ID       string `json:"id"`
	Label    string `json:"label"`
	APIKey   string `json:"api_key"`
	Disabled bool   `json:"disabled,omitempty"`
	Priority int    `json:"priority,omitempty"`
}

// credentialView is what the console renders for one auth record.
type credentialView struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Label      string `json:"label"`
	Email      string `json:"email,omitempty"`
	Enabled    bool   `json:"enabled"`
	Status     string `json:"status,omitempty"`
	Priority   int    `json:"priority,omitempty"`
	KeyMasked  string `json:"key_masked,omitempty"`
	HasKey     bool   `json:"has_key"`
	AuthIndex  string `json:"auth_index,omitempty"`
	SourceFile string `json:"source_file,omitempty"`
}

func (m *Manager) authEntries(ctx context.Context) ([]pluginapi.HostAuthFileEntry, error) {
	if m.bridge == nil {
		return nil, fmt.Errorf("host bridge unavailable")
	}
	entries, err := m.bridge.AuthList(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]pluginapi.HostAuthFileEntry, 0, len(entries))
	for _, e := range entries {
		if strings.EqualFold(strings.TrimSpace(e.Provider), ProviderID) || strings.EqualFold(strings.TrimSpace(e.Type), ProviderID) {
			out = append(out, e)
		}
	}
	return out, nil
}

// listCredentials renders every CommandCode auth record the host knows.
func (m *Manager) listCredentials(ctx context.Context) ([]credentialView, error) {
	entries, err := m.authEntries(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]credentialView, 0, len(entries))
	for _, e := range entries {
		v := credentialView{
			ID:         strings.TrimSpace(e.ID),
			Name:       strings.TrimSpace(e.Name),
			Label:      strings.TrimSpace(e.Label),
			Email:      strings.TrimSpace(e.Email),
			Enabled:    !e.Disabled,
			Status:     strings.TrimSpace(e.Status),
			Priority:   e.Priority,
			AuthIndex:  strings.TrimSpace(e.AuthIndex),
			SourceFile: filepath.Base(strings.TrimSpace(e.Path)),
		}
		if v.ID == "" {
			v.ID = strings.TrimSuffix(v.Name, ".json")
		}
		if v.Label == "" {
			v.Label = v.ID
		}
		// A masked key (last 4) helps an operator tell two keys apart without
		// exposing the secret. Reading it requires one auth.get round-trip.
		if path := strings.TrimSpace(e.Path); path != "" {
			if key := readKeyFromFile(path); key != "" {
				v.HasKey = true
				v.KeyMasked = maskKey(key)
			}
		}
		out = append(out, v)
	}
	return out, nil
}

func maskKey(key string) string {
	key = strings.TrimSpace(key)
	if len(key) <= 4 {
		return "••••"
	}
	return "••••••" + key[len(key)-4:]
}

func readKeyFromFile(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil || !gjson.ValidBytes(raw) {
		return ""
	}
	return strings.TrimSpace(gjson.GetBytes(raw, "api_key").String())
}

// credentialInput is the add/update payload from the console. Pointers keep
// "field omitted" distinct from "clear to empty".
type credentialInput struct {
	ID       string  `json:"id"`
	Label    *string `json:"label"`
	APIKey   *string `json:"api_key"`
	Disabled *bool   `json:"disabled"`
	Priority *int    `json:"priority"`
}

// addCredential creates one auth record and returns its file name.
func (m *Manager) addCredential(ctx context.Context, in credentialInput) (string, error) {
	key := ""
	if in.APIKey != nil {
		key = strings.TrimSpace(*in.APIKey)
	}
	if len(key) < 8 || strings.ContainsAny(key, "\r\n") {
		return "", fmt.Errorf("API key looks invalid (need at least 8 characters, single line)")
	}
	if m.bridge == nil {
		return "", fmt.Errorf("host bridge unavailable")
	}
	label := ""
	if in.Label != nil {
		label = strings.TrimSpace(*in.Label)
	}
	if label == "" {
		label = "CommandCode"
	}
	if len(label) > 100 {
		return "", fmt.Errorf("label exceeds 100 characters")
	}
	hash := authKeyHash(key)
	record := credentialRecord{
		Type:     ProviderID,
		Provider: ProviderID,
		ID:       authRecordIDFromHash(hash),
		Label:    label,
		APIKey:   key,
	}
	if in.Disabled != nil {
		record.Disabled = *in.Disabled
	}
	if in.Priority != nil {
		record.Priority = *in.Priority
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return "", fmt.Errorf("build credential record failed")
	}
	name := authFileNameFromHash(hash)
	if _, err := m.bridge.AuthSave(ctx, pluginapi.HostAuthSaveRequest{Name: name, JSON: raw}); err != nil {
		return "", err
	}
	return name, nil
}

// findCredential resolves a console-supplied id to its host auth entry.
func (m *Manager) findCredential(ctx context.Context, id string) (pluginapi.HostAuthFileEntry, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return pluginapi.HostAuthFileEntry{}, fmt.Errorf("credential id is required")
	}
	entries, err := m.authEntries(ctx)
	if err != nil {
		return pluginapi.HostAuthFileEntry{}, err
	}
	for _, e := range entries {
		if strings.EqualFold(strings.TrimSpace(e.ID), id) || strings.TrimSuffix(strings.TrimSpace(e.Name), ".json") == id {
			return e, nil
		}
	}
	return pluginapi.HostAuthFileEntry{}, fmt.Errorf("credential not found")
}

// updateCredential edits label/key/disabled/priority in place, preserving any
// host-managed fields the file already carries.
func (m *Manager) updateCredential(ctx context.Context, in credentialInput) error {
	entry, err := m.findCredential(ctx, in.ID)
	if err != nil {
		return err
	}
	path := strings.TrimSpace(entry.Path)
	if path == "" {
		return fmt.Errorf("credential file path is unknown")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("credential file is unreadable")
	}
	if !gjson.ValidBytes(raw) {
		return fmt.Errorf("credential file is not valid JSON")
	}
	next := raw
	set := func(key string, value any) error {
		var e error
		next, e = sjson.SetBytes(next, key, value)
		return e
	}
	if in.Label != nil {
		label := strings.TrimSpace(*in.Label)
		if len(label) > 100 {
			return fmt.Errorf("label exceeds 100 characters")
		}
		if label == "" {
			label = "CommandCode"
		}
		if err := set("label", label); err != nil {
			return err
		}
	}
	if in.APIKey != nil && strings.TrimSpace(*in.APIKey) != "" {
		key := strings.TrimSpace(*in.APIKey)
		if len(key) < 8 || strings.ContainsAny(key, "\r\n") {
			return fmt.Errorf("API key looks invalid")
		}
		if err := set("api_key", key); err != nil {
			return err
		}
	}
	if in.Disabled != nil {
		if err := set("disabled", *in.Disabled); err != nil {
			return err
		}
	}
	if in.Priority != nil {
		if err := set("priority", *in.Priority); err != nil {
			return err
		}
	}
	if string(next) == string(raw) {
		return nil
	}
	return writeFileAtomic(path, next)
}

// deleteCredential removes the backing auth file.
func (m *Manager) deleteCredential(ctx context.Context, id string) error {
	entry, err := m.findCredential(ctx, id)
	if err != nil {
		return err
	}
	path := strings.TrimSpace(entry.Path)
	if path == "" {
		return fmt.Errorf("credential file path is unknown")
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("credential removal failed")
	}
	return nil
}

// writeFileAtomic replaces path with data via a temp file + rename so the
// host's auth-directory watcher only ever observes a complete file.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".commandcode-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// touchAuthRevision stamps a revision into an auth file so the host's watcher
// re-parses it and re-queries the plugin's static model list. Unknown fields
// (host-managed metadata) survive because the edit is in place.
func touchAuthRevision(path, revision string) {
	raw, err := os.ReadFile(path)
	if err != nil || !gjson.ValidBytes(raw) {
		return
	}
	next, err := sjson.SetBytes(raw, "model_revision", revision)
	if err != nil {
		return
	}
	_ = writeFileAtomic(path, next)
}

// catalogKey resolves an API key for a catalog refresh: the first YAML key,
// otherwise any key from the host auth records. This is what lets a fully
// page-managed key pool drive model discovery.
func (m *Manager) catalogKey(ctx context.Context, cfg config.Config) (string, error) {
	for _, k := range cfg.APIKeys {
		if key := strings.TrimSpace(k.Value); key != "" {
			return key, nil
		}
	}
	if m.bridge == nil {
		return "", fmt.Errorf("no api key configured and host bridge unavailable")
	}
	entries, err := m.authEntries(ctx)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.Disabled {
			continue
		}
		if path := strings.TrimSpace(e.Path); path != "" {
			if key := readKeyFromFile(path); key != "" {
				return key, nil
			}
		}
		if idx := strings.TrimSpace(e.AuthIndex); idx != "" {
			got, err := m.bridge.AuthGet(ctx, idx)
			if err == nil {
				if key := strings.TrimSpace(gjson.GetBytes(got.JSON, "api_key").String()); key != "" {
					return key, nil
				}
			}
		}
	}
	return "", fmt.Errorf("no CommandCode credential available for catalog refresh")
}
