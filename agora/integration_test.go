package agora

import (
	"strings"
	"testing"
)

func TestResolveConfigExpansion(t *testing.T) {
	t.Setenv("AGORA_TEST_TUNNEL", "front-door")

	// a set variable resolves into the config.
	cfg := &Config{
		Enabled: true,
		Serve:   &ServeConfig{Tunnel: "${AGORA_TEST_TUNNEL}"},
	}
	if err := ResolveConfig(cfg); err != nil {
		t.Fatalf("ResolveConfig() = %v, want nil", err)
	}
	if cfg.Serve.Tunnel != "front-door" {
		t.Errorf("serve tunnel = %q, want resolved value", cfg.Serve.Tunnel)
	}

	// a placeholder that resolves empty is a directed error, not a silent
	// fall-through to the field's default.
	cfg = &Config{
		Enabled: true,
		EnvRoot: "${AGORA_TEST_UNSET_VAR}",
	}
	if err := ResolveConfig(cfg); err == nil || !strings.Contains(err.Error(), "agora.env_root resolves empty") {
		t.Fatalf("ResolveConfig() = %v, want resolves-empty error", err)
	}

	// values left empty stay unset and take their documented defaults.
	cfg = &Config{Enabled: true, Serve: &ServeConfig{}}
	if err := ResolveConfig(cfg); err != nil {
		t.Fatalf("ResolveConfig() = %v, want nil for empty fields", err)
	}
}
