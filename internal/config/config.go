// Package config loads and validates the commandcode plugin configuration
// per (configuration) and (HTTPS/timeouts).
package config

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Defaults (/).
const (
	DefaultBaseURL          = "https://api.commandcode.ai/provider/v1"
	DefaultModelPrefix      = "commandcode"
	DefaultRefreshInterval  = 15 * time.Minute
	DefaultRequestTimeout   = 5 * time.Minute
	DefaultMaxResponseBytes = int64(67108864) // 64 MiB
)

type ModelPrefix struct {
	Enabled bool   `json:"enabled"`
	Value   string `json:"value"`
}

type APIKey struct {
	Value string
}

type Catalog struct {
	RefreshInterval       time.Duration
	StaleWhileUnavailable bool
}

type Protocols struct {
	ChatCompletions bool `json:"chat_completions"`
	Messages        bool `json:"messages"`
	Responses       bool `json:"responses"`
}

type RouteOverride struct {
	Protocol string `yaml:"protocol"`
	Endpoint string `yaml:"endpoint"`
}

type Config struct {
	BaseURL          string
	CatalogURL       string
	ModelPrefix      ModelPrefix
	APIKeys          []APIKey
	Catalog          Catalog
	Protocols        Protocols
	RouteOverrides   map[string]RouteOverride
	AllowHTTP        bool
	RequestTimeout   time.Duration
	MaxResponseBytes int64
	// DataDir is the persistent plugin state directory (page-managed settings,
	// request logs). Empty means the page cannot persist state.
	DataDir string
}

// Overlay carries page-managed overrides that win over the YAML config. Every
// field is a pointer so "unset" (keep YAML/default) stays distinct from an
// explicit value.
type Overlay struct {
	BaseURL          *string      `json:"base_url,omitempty"`
	CatalogURL       *string      `json:"catalog_url,omitempty"`
	ModelPrefix      *ModelPrefix `json:"model_prefix,omitempty"`
	Protocols        *Protocols   `json:"protocols,omitempty"`
	AllowHTTP        *bool        `json:"allow_http,omitempty"`
	RequestTimeout   *string      `json:"request_timeout,omitempty"`
	RefreshInterval  *string      `json:"refresh_interval,omitempty"`
	MaxResponseBytes *int64       `json:"max_response_bytes,omitempty"`
}

// Apply returns cfg with the overlay applied and re-validated. CatalogURL is
// recomputed from a changed BaseURL unless the overlay pins one explicitly.
func (c Config) Apply(o Overlay) (Config, error) {
	if o.BaseURL != nil {
		c.BaseURL = strings.TrimSpace(*o.BaseURL)
	}
	if o.ModelPrefix != nil {
		c.ModelPrefix = *o.ModelPrefix
	}
	if o.Protocols != nil {
		c.Protocols = *o.Protocols
	}
	if o.AllowHTTP != nil {
		c.AllowHTTP = *o.AllowHTTP
	}
	if o.MaxResponseBytes != nil {
		c.MaxResponseBytes = *o.MaxResponseBytes
	}
	if o.RequestTimeout != nil {
		d, err := time.ParseDuration(strings.TrimSpace(*o.RequestTimeout))
		if err != nil {
			return Config{}, fmt.Errorf("request-timeout: invalid duration")
		}
		if d <= 0 {
			return Config{}, fmt.Errorf("request-timeout: must be positive")
		}
		c.RequestTimeout = d
	}
	if o.RefreshInterval != nil {
		d, err := time.ParseDuration(strings.TrimSpace(*o.RefreshInterval))
		if err != nil {
			return Config{}, fmt.Errorf("catalog.refresh-interval: invalid duration")
		}
		c.Catalog.RefreshInterval = d
	}
	switch {
	case o.CatalogURL != nil:
		c.CatalogURL = strings.TrimRight(strings.TrimSpace(*o.CatalogURL), "/")
	default:
		c.CatalogURL = strings.TrimRight(c.BaseURL, "/") + "/models"
	}
	if err := c.validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// rawConfig mirrors the YAML shape; pointer fields distinguish "unset"
// (apply default) from explicitly-set values including "" (validate as-is).
// Unknown fields are ignored (host may pass extra keys).
type rawConfig struct {
	BaseURL          *string                  `yaml:"base-url"`
	CatalogURL       *string                  `yaml:"catalog-url"`
	ModelPrefix      rawPrefix                `yaml:"model-prefix"`
	APIKeys          []rawKey                 `yaml:"api-keys"`
	Catalog          rawCatalog               `yaml:"catalog"`
	Protocols        rawProtocols             `yaml:"protocols"`
	RouteOverrides   map[string]RouteOverride `yaml:"route-overrides"`
	AllowHTTP        bool                     `yaml:"allow-http"`
	RequestTimeout   *string                  `yaml:"request-timeout"`
	MaxResponseBytes *int64                   `yaml:"max-response-bytes"`
	DataDir          *string                  `yaml:"data-dir"`
}

type rawPrefix struct {
	Enabled *bool   `yaml:"enabled"`
	Value   *string `yaml:"value"`
}

type rawKey struct {
	Value string `yaml:"value"`
}

type rawCatalog struct {
	RefreshInterval       *string `yaml:"refresh-interval"`
	StaleWhileUnavailable *bool   `yaml:"stale-while-unavailable"`
}

type rawProtocols struct {
	ChatCompletions *bool `yaml:"chat-completions"`
	Messages        *bool `yaml:"messages"`
	Responses       *bool `yaml:"responses"`
}

var (
	validProtocols = map[string]bool{"chat-completions": true, "messages": true, "responses": true}
)

// Load decodes YAML, expands ${VAR} references in api-key values only,
// applies defaults, and validates. Decode errors never echo
// decoded node values — a malformed entry (e.g. a bare-scalar API key)
// must not leak into the invalid_config envelope the host logs.
func Load(yamlBytes []byte) (Config, error) {
	var raw rawConfig
	if err := yaml.Unmarshal(yamlBytes, &raw); err != nil {
		if n := regexp.MustCompile(`line (\d+)`).FindStringSubmatch(err.Error()); n != nil {
			return Config{}, fmt.Errorf("decode config: invalid YAML structure near line %s", n[1])
		}
		return Config{}, fmt.Errorf("decode config: invalid YAML structure")
	}
	keys := make([]APIKey, len(raw.APIKeys))
	for i, k := range raw.APIKeys {
		keys[i] = APIKey{Value: os.ExpandEnv(k.Value)}
	}
	refreshInterval, err := parseDuration("catalog.refresh-interval", raw.Catalog.RefreshInterval, DefaultRefreshInterval)
	if err != nil {
		return Config{}, err
	}
	requestTimeout, err := parseDuration("request-timeout", raw.RequestTimeout, DefaultRequestTimeout)
	if err != nil {
		return Config{}, err
	}
	if requestTimeout <= 0 {
		return Config{}, fmt.Errorf("request-timeout: must be positive")
	}
	c := Config{
		BaseURL: orDefault(raw.BaseURL, DefaultBaseURL),
		ModelPrefix: ModelPrefix{
			Enabled: orDefault(raw.ModelPrefix.Enabled, true),
			Value:   orDefault(raw.ModelPrefix.Value, DefaultModelPrefix),
		},
		APIKeys: keys,
		Catalog: Catalog{
			RefreshInterval:       refreshInterval,
			StaleWhileUnavailable: orDefault(raw.Catalog.StaleWhileUnavailable, true),
		},
		Protocols: Protocols{
			ChatCompletions: orDefault(raw.Protocols.ChatCompletions, true),
			Messages:        orDefault(raw.Protocols.Messages, true),
			Responses:       orDefault(raw.Protocols.Responses, true),
		},
		RouteOverrides:   raw.RouteOverrides,
		AllowHTTP:        raw.AllowHTTP,
		RequestTimeout:   requestTimeout,
		MaxResponseBytes: orDefault(raw.MaxResponseBytes, DefaultMaxResponseBytes),
		DataDir:          strings.TrimSpace(orDefault(raw.DataDir, "")),
	}
	if raw.CatalogURL != nil {
		// Mirror the derived-default trim so an explicit trailing-slash
		// catalog-url cannot double up separators downstream.
		c.CatalogURL = strings.TrimRight(*raw.CatalogURL, "/")
	} else {
		c.CatalogURL = strings.TrimRight(c.BaseURL, "/") + "/models"
	}
	if err := c.validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// PublicID returns the client-facing model ID.
func PublicID(c Config, upstreamID string) string {
	if c.ModelPrefix.Enabled {
		return c.ModelPrefix.Value + "/" + upstreamID
	}
	return upstreamID
}

func (c Config) validate() error {
	if err := validateURL("base-url", c.BaseURL, c.AllowHTTP); err != nil {
		return err
	}
	if err := validateURL("catalog-url", c.CatalogURL, c.AllowHTTP); err != nil {
		return err
	}
	// YAML api-keys are optional: credentials can be managed entirely from the
	// console page as host auth records. A catalog refresh resolves a key from
	// those records when this list is empty.
	for i, k := range c.APIKeys {
		if k.Value == "" {
			return fmt.Errorf("api-keys[%d].value: expanded to empty", i)
		}
	}
	seen := make(map[string]bool, len(c.APIKeys))
	for _, k := range c.APIKeys {
		if seen[k.Value] {
			return fmt.Errorf("api-keys: duplicate key values are not allowed")
		}
		seen[k.Value] = true
	}
	if c.Catalog.RefreshInterval < time.Minute {
		return fmt.Errorf("catalog.refresh-interval: must be at least 1m")
	}
	if c.MaxResponseBytes <= 0 {
		return fmt.Errorf("max-response-bytes: must be positive")
	}
	for name, o := range c.RouteOverrides {
		if !validProtocols[o.Protocol] {
			return fmt.Errorf("route-overrides[%s].protocol: unsupported protocol %q", name, o.Protocol)
		}
		if o.Endpoint == "" {
			return fmt.Errorf("route-overrides[%s].endpoint: must not be empty", name)
		}
		if !strings.HasPrefix(o.Endpoint, "/") {
			return fmt.Errorf("route-overrides[%s].endpoint: must start with /", name)
		}
	}
	if c.ModelPrefix.Enabled && !validPrefix(c.ModelPrefix.Value) {
		return fmt.Errorf("model-prefix.value: invalid provider-ID characters %q", c.ModelPrefix.Value)
	}
	return nil
}

func validateURL(name, raw string, allowHTTP bool) error {
	if raw == "" {
		return fmt.Errorf("%s: must not be empty", name)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s: invalid URL", name)
	}
	if u.Scheme == "" || u.Host == "" {
		// Echo only scheme://host — the configured string may embed
		// userinfo credentials (https://user:key@host).
		return fmt.Errorf("%s: invalid URL %s://%s", name, u.Scheme, u.Host)
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("%s: must not contain query, fragment, or userinfo", name)
	}
	// Scheme allowlist: https always; http only behind
	// allow-http; anything else (ftp://, file://, custom schemes) is
	// rejected at load instead of failing at transport time.
	switch u.Scheme {
	case "https":
	case "http":
		if !allowHTTP {
			return fmt.Errorf("%s: http scheme requires allow-http", name)
		}
	default:
		return fmt.Errorf("%s: unsupported scheme %q; use https", name, u.Scheme)
	}
	return nil
}

func validPrefix(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		alnum := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if i == 0 && !alnum {
			return false
		}
		if !alnum && r != '.' && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

func parseDuration(name string, p *string, def time.Duration) (time.Duration, error) {
	if p == nil {
		return def, nil
	}
	d, err := time.ParseDuration(*p)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid duration", name)
	}
	return d, nil
}

func orDefault[T any](p *T, def T) T {
	if p != nil {
		return *p
	}
	return def
}
