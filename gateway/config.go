package gateway

import (
	"fmt"
	"os"
	"strings"

	"github.com/michaelquigley/df/dd"
	"github.com/openziti/llm-gateway/agora"
	"github.com/openziti/llm-gateway/routing"
)

type Config struct {
	Listen    string
	Zrok      *ZrokConfig
	Agora     *agora.Config
	Providers *ProvidersConfig
	Routing   *routing.RoutingConfig
	Metrics   *MetricsConfig
	APIKeys   *APIKeysConfig
	Tracing   *TracingConfig
}

type TracingConfig struct {
	Enabled          bool
	MaxContentLength int // max characters per message content (default: 200)
}

type ZrokConfig struct {
	Share *ZrokShareConfig
}

type ZrokShareConfig struct {
	Enabled bool
	Mode    string // public or private (default: private)
	Token   string // existing persistent share token (private shares only)
}

type ProvidersConfig struct {
	OpenAI    *OpenAIConfig
	Anthropic *AnthropicConfig
	Local     *LocalConfig
}

type OpenAIConfig struct {
	APIKey         string
	BaseURL        string
	ZrokShareToken string
	AgoraTunnel    string
}

type AnthropicConfig struct {
	APIKey         string
	BaseURL        string
	ZrokShareToken string
	AgoraTunnel    string
}

type LocalConfig struct {
	BaseURL        string
	ZrokShareToken string
	AgoraTunnel    string
	Endpoints      []LocalEndpointConfig
	HealthCheck    *HealthCheckConfig
}

type LocalEndpointConfig struct {
	Name           string
	BaseURL        string
	ZrokShareToken string
	AgoraTunnel    string
	Weight         int
}

type HealthCheckConfig struct {
	IntervalSeconds int
	TimeoutSeconds  int
}

type MetricsConfig struct {
	Enabled bool
}

type APIKeysConfig struct {
	Enabled bool
	Keys    []APIKeyEntry
}

type APIKeyEntry struct {
	Name          string
	Key           string
	AllowedModels []string
	AllowedRoutes []string
}

func LoadConfig(path string) (*Config, error) {
	cfg := &Config{}
	if err := dd.MergeYAMLFile(cfg, path); err != nil {
		return nil, err
	}
	// resolve agora env vars + integration file before any agora field is read.
	if err := agora.ResolveConfig(cfg.Agora); err != nil {
		return nil, err
	}
	if err := cfg.expandEnv(); err != nil {
		return nil, err
	}
	cfg.normalize()
	if err := cfg.validateAgora(); err != nil {
		return nil, err
	}
	if err := cfg.validateProviders(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// expandEnv resolves ${VAR} references in the config's secret-bearing string
// fields once, at load, so every downstream gate reads the same expanded
// values. a value written non-empty that resolves empty is a directed error
// (an unset variable); a value left empty stays "not configured".
func (c *Config) expandEnv() error {
	expand := func(field string, value *string) error {
		if *value == "" {
			return nil
		}
		expanded := os.ExpandEnv(*value)
		if expanded == "" {
			return fmt.Errorf("%s resolves empty (unset environment variable?)", field)
		}
		*value = expanded
		return nil
	}

	if c.Providers != nil {
		if p := c.Providers.OpenAI; p != nil {
			if err := expand("providers.openai.api_key", &p.APIKey); err != nil {
				return err
			}
			if err := expand("providers.openai.base_url", &p.BaseURL); err != nil {
				return err
			}
		}
		if p := c.Providers.Anthropic; p != nil {
			if err := expand("providers.anthropic.api_key", &p.APIKey); err != nil {
				return err
			}
			if err := expand("providers.anthropic.base_url", &p.BaseURL); err != nil {
				return err
			}
		}
		if l := c.Providers.Local; l != nil {
			if err := expand("providers.local.base_url", &l.BaseURL); err != nil {
				return err
			}
			for i := range l.Endpoints {
				field := fmt.Sprintf("providers.local.endpoints[%d].base_url", i)
				if err := expand(field, &l.Endpoints[i].BaseURL); err != nil {
					return err
				}
			}
		}
	}

	if c.APIKeys != nil && c.APIKeys.Enabled {
		for i := range c.APIKeys.Keys {
			entry := &c.APIKeys.Keys[i]
			field := fmt.Sprintf("api_keys.keys[%d] ('%s')", i, entry.Name)
			if entry.Key == "" {
				return fmt.Errorf("%s has an empty key", field)
			}
			if err := expand(field+" key", &entry.Key); err != nil {
				return err
			}
		}
	}

	return nil
}

// normalize trims whitespace from agora tunnel names so the provider init
// gates and collectAgoraTunnels read identical values.
func (c *Config) normalize() {
	if c.Providers == nil {
		return
	}
	if p := c.Providers.OpenAI; p != nil {
		p.AgoraTunnel = strings.TrimSpace(p.AgoraTunnel)
	}
	if p := c.Providers.Anthropic; p != nil {
		p.AgoraTunnel = strings.TrimSpace(p.AgoraTunnel)
	}
	if l := c.Providers.Local; l != nil {
		l.AgoraTunnel = strings.TrimSpace(l.AgoraTunnel)
		for i := range l.Endpoints {
			l.Endpoints[i].AgoraTunnel = strings.TrimSpace(l.Endpoints[i].AgoraTunnel)
		}
	}
}

// validateProviders enforces that an explicitly configured overlay transport on
// a cloud provider can actually be honored: the provider only initializes with
// an API key, so an overlay without one would silently evaporate. runs after
// expandEnv, so the key values here are already resolved.
func (c *Config) validateProviders() error {
	if c.Providers == nil {
		return nil
	}
	check := func(name, apiKey, zrokToken, agoraTunnel string) error {
		if apiKey != "" {
			return nil
		}
		if agoraTunnel != "" {
			return fmt.Errorf("providers.%s.agora_tunnel is set but providers.%s.api_key is empty", name, name)
		}
		if zrokToken != "" {
			return fmt.Errorf("providers.%s.zrok_share_token is set but providers.%s.api_key is empty", name, name)
		}
		return nil
	}
	if p := c.Providers.OpenAI; p != nil {
		if err := check("openai", p.APIKey, p.ZrokShareToken, p.AgoraTunnel); err != nil {
			return err
		}
	}
	if p := c.Providers.Anthropic; p != nil {
		if err := check("anthropic", p.APIKey, p.ZrokShareToken, p.AgoraTunnel); err != nil {
			return err
		}
	}
	// multi-endpoint mode reads only per-endpoint transports; a top-level
	// overlay on the local block would be silently ignored.
	if l := c.Providers.Local; l != nil && len(l.Endpoints) > 0 {
		if l.AgoraTunnel != "" {
			return fmt.Errorf("providers.local.agora_tunnel is ignored in multi-endpoint mode; move it onto an endpoint")
		}
		if l.ZrokShareToken != "" {
			return fmt.Errorf("providers.local.zrok_share_token is ignored in multi-endpoint mode; move it onto an endpoint")
		}
	}
	return nil
}

// AgoraServeEnabled reports whether the gateway should serve its handler over
// an agora tunnel.
func (c *Config) AgoraServeEnabled() bool {
	return c != nil && c.Agora != nil && c.Agora.Enabled && agora.ServeEnabled(c.Agora)
}

// AgoraPublishEnabled reports whether the gateway should publish a catalog
// advertisement. Publishing requires serving in this iteration: a dial-only
// gateway never publishes an advertisement whose name points at a front-door
// tunnel it does not bind.
func (c *Config) AgoraPublishEnabled() bool {
	return c.AgoraServeEnabled() && agora.AdvertisementPublish(c.Agora)
}

// collectAgoraTunnels returns the unique, trimmed agora_tunnel names for only
// the providers and endpoints initProviders will actually initialize — no
// phantom attachments. OpenAI/Anthropic count only when configured (the same
// APIKey gate initProviders uses); Local contributes its endpoint tunnels in
// multi mode and its single tunnel only in single mode.
func collectAgoraTunnels(cfg *Config) []string {
	if cfg == nil || cfg.Providers == nil {
		return nil
	}
	seen := map[string]struct{}{}
	var tunnels []string
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		tunnels = append(tunnels, name)
	}

	if p := cfg.Providers.OpenAI; p != nil && p.APIKey != "" {
		add(p.AgoraTunnel)
	}
	if p := cfg.Providers.Anthropic; p != nil && p.APIKey != "" {
		add(p.AgoraTunnel)
	}
	if l := cfg.Providers.Local; l != nil {
		if len(l.Endpoints) > 0 {
			for _, ep := range l.Endpoints {
				add(ep.AgoraTunnel)
			}
		} else {
			add(l.AgoraTunnel)
		}
	}
	return tunnels
}

// validateAgora enforces the fail-fast preconditions that keep per-site
// agora_tunnel values and agora.serve.enabled meaningful: each requires the
// agora subsystem, and an explicit publish request requires serving.
func (c *Config) validateAgora() error {
	// (a) dial side — a per-site agora_tunnel is meaningless without the subsystem.
	if len(collectAgoraTunnels(c)) > 0 && (c.Agora == nil || !c.Agora.Enabled) {
		return fmt.Errorf("agora_tunnel set on a provider/endpoint requires agora.enabled: true")
	}
	// (b) serve side (symmetric) — serve.enabled without enabled would silently
	// fall back to the plaintext local listener.
	if c.Agora != nil && c.Agora.Serve != nil && c.Agora.Serve.Enabled && !c.Agora.Enabled {
		return fmt.Errorf("agora.serve.enabled requires agora.enabled: true")
	}
	// (c) explicit publish: true without serve — honor the request loudly, not
	// silently. an explicit false is an opt-out and needs no serve.
	if agora.PublishExplicit(c.Agora) && agora.AdvertisementPublish(c.Agora) && !c.AgoraServeEnabled() {
		return fmt.Errorf("agora.advertisement.publish requires agora.serve.enabled in this iteration")
	}
	return nil
}
