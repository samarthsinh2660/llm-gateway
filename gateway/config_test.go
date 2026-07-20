package gateway

import (
	"net/http"
	"strings"
	"testing"
)

func TestExpandEnvSecrets(t *testing.T) {
	t.Setenv("LLMGW_TEST_KEY", "sk-resolved")

	// set variables resolve into the config.
	cfg := &Config{
		Providers: &ProvidersConfig{
			OpenAI: &OpenAIConfig{APIKey: "${LLMGW_TEST_KEY}"},
		},
		APIKeys: &APIKeysConfig{
			Enabled: true,
			Keys:    []APIKeyEntry{{Name: "alice", Key: "${LLMGW_TEST_KEY}"}},
		},
	}
	if err := cfg.expandEnv(); err != nil {
		t.Fatalf("expandEnv() = %v, want nil", err)
	}
	if cfg.Providers.OpenAI.APIKey != "sk-resolved" {
		t.Errorf("provider key = %q, want resolved value", cfg.Providers.OpenAI.APIKey)
	}
	if cfg.APIKeys.Keys[0].Key != "sk-resolved" {
		t.Errorf("virtual key = %q, want resolved value", cfg.APIKeys.Keys[0].Key)
	}

	// a placeholder that resolves empty is a directed error, not silence.
	cfg = &Config{Providers: &ProvidersConfig{
		Anthropic: &AnthropicConfig{APIKey: "${LLMGW_TEST_UNSET_VAR}"},
	}}
	if err := cfg.expandEnv(); err == nil || !strings.Contains(err.Error(), "providers.anthropic.api_key") {
		t.Fatalf("expandEnv() = %v, want resolves-empty error", err)
	}

	// an enabled key entry with an empty key is a directed error.
	cfg = &Config{APIKeys: &APIKeysConfig{
		Enabled: true,
		Keys:    []APIKeyEntry{{Name: "bob"}},
	}}
	if err := cfg.expandEnv(); err == nil || !strings.Contains(err.Error(), "empty key") {
		t.Fatalf("expandEnv() = %v, want empty-key error", err)
	}

	// values left empty stay "not configured"; disabled key blocks are inert.
	cfg = &Config{
		Providers: &ProvidersConfig{OpenAI: &OpenAIConfig{}},
		APIKeys:   &APIKeysConfig{Keys: []APIKeyEntry{{Name: "carol"}}},
	}
	if err := cfg.expandEnv(); err != nil {
		t.Fatalf("expandEnv() = %v, want nil for empty/disabled", err)
	}

	// local base URLs expand like the cloud providers do — single and endpoint.
	t.Setenv("LLMGW_TEST_HOST", "http://ollama.internal:11434")
	cfg = &Config{Providers: &ProvidersConfig{Local: &LocalConfig{
		BaseURL:   "${LLMGW_TEST_HOST}",
		Endpoints: []LocalEndpointConfig{{Name: "a", BaseURL: "${LLMGW_TEST_HOST}"}},
	}}}
	if err := cfg.expandEnv(); err != nil {
		t.Fatalf("expandEnv() = %v, want nil", err)
	}
	if cfg.Providers.Local.BaseURL != "http://ollama.internal:11434" {
		t.Errorf("local base_url = %q, want resolved value", cfg.Providers.Local.BaseURL)
	}
	if cfg.Providers.Local.Endpoints[0].BaseURL != "http://ollama.internal:11434" {
		t.Errorf("endpoint base_url = %q, want resolved value", cfg.Providers.Local.Endpoints[0].BaseURL)
	}
}

func TestResolveEmbedProviderOpenAIInheritsOverlayClient(t *testing.T) {
	overlay := &http.Client{}
	g := &Gateway{
		cfg: &Config{Providers: &ProvidersConfig{
			OpenAI: &OpenAIConfig{APIKey: "sk-test"},
		}},
		openaiHTTPClient: overlay,
	}

	baseURL, apiKey, httpClient, err := g.resolveRoutingProvider("openai")
	if err != nil {
		t.Fatalf("resolveRoutingProvider() = %v, want nil", err)
	}
	if httpClient != overlay {
		t.Errorf("openai embed client must be the provider's overlay-backed client")
	}
	if baseURL != "https://api.openai.com" {
		t.Errorf("base URL = %q, want the unchanged default", baseURL)
	}
	if apiKey != "sk-test" {
		t.Errorf("api key = %q, want configured key", apiKey)
	}
}

func TestResolveEmbedProviderRefusesUnusable(t *testing.T) {
	// a keyless openai block selected as a routing provider is a directed
	// error, not a runtime 401.
	g := &Gateway{cfg: &Config{Providers: &ProvidersConfig{OpenAI: &OpenAIConfig{}}}}
	if _, _, _, err := g.resolveRoutingProvider("openai"); err == nil ||
		!strings.Contains(err.Error(), "requires providers.openai.api_key") {
		t.Fatalf("resolveRoutingProvider() = %v, want keyless-openai error", err)
	}

	// an unknown provider name is refused, not reported as unconfigured.
	g = &Gateway{cfg: &Config{Providers: &ProvidersConfig{}}}
	if _, _, _, err := g.resolveRoutingProvider("ollama"); err == nil ||
		!strings.Contains(err.Error(), "unknown routing provider 'ollama'") {
		t.Fatalf("resolveRoutingProvider() = %v, want unknown-provider error", err)
	}
}

func TestValidateProvidersOverlayRequiresAPIKey(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *Config
		wantErr string
	}{
		{
			name: "openai agora tunnel without key",
			cfg: &Config{Providers: &ProvidersConfig{
				OpenAI: &OpenAIConfig{AgoraTunnel: "egress"},
			}},
			wantErr: "providers.openai.agora_tunnel",
		},
		{
			name: "anthropic zrok token without key",
			cfg: &Config{Providers: &ProvidersConfig{
				Anthropic: &AnthropicConfig{ZrokShareToken: "abc123"},
			}},
			wantErr: "providers.anthropic.zrok_share_token",
		},
		{
			name: "openai agora tunnel with key",
			cfg: &Config{Providers: &ProvidersConfig{
				OpenAI: &OpenAIConfig{APIKey: "sk-test", AgoraTunnel: "egress"},
			}},
		},
		{
			name: "local overlay needs no key",
			cfg: &Config{Providers: &ProvidersConfig{
				Local: &LocalConfig{AgoraTunnel: "infer"},
			}},
		},
		{
			name: "top-level local agora tunnel with endpoints is refused",
			cfg: &Config{Providers: &ProvidersConfig{
				Local: &LocalConfig{
					AgoraTunnel: "infer",
					Endpoints:   []LocalEndpointConfig{{Name: "a", BaseURL: "http://x"}},
				},
			}},
			wantErr: "providers.local.agora_tunnel is ignored in multi-endpoint mode",
		},
		{
			name: "top-level local zrok token with endpoints is refused",
			cfg: &Config{Providers: &ProvidersConfig{
				Local: &LocalConfig{
					ZrokShareToken: "abc123",
					Endpoints:      []LocalEndpointConfig{{Name: "a", BaseURL: "http://x"}},
				},
			}},
			wantErr: "providers.local.zrok_share_token is ignored in multi-endpoint mode",
		},
		{
			name: "cloud providers without overlays are not validated here",
			cfg: &Config{Providers: &ProvidersConfig{
				OpenAI:    &OpenAIConfig{},
				Anthropic: &AnthropicConfig{},
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.validateProviders()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateProviders() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateProviders() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}
