package gateway

import (
	"strings"
	"testing"

	"github.com/openziti/llm-gateway/agora"
)

func TestValidateAgoraDialRequiresEnabled(t *testing.T) {
	// (a) a per-site agora_tunnel without agora.enabled must fail fast.
	cfg := &Config{
		Providers: &ProvidersConfig{
			OpenAI: &OpenAIConfig{APIKey: "sk-test", AgoraTunnel: "openai-egress"},
		},
	}
	err := cfg.validateAgora()
	if err == nil || !strings.Contains(err.Error(), "agora.enabled") {
		t.Fatalf("expected agora.enabled precondition error, got %v", err)
	}

	// with agora.enabled it passes.
	cfg.Agora = &agora.Config{Enabled: true}
	if err := cfg.validateAgora(); err != nil {
		t.Fatalf("unexpected error with agora.enabled: %v", err)
	}
}

func TestValidateAgoraServeRequiresEnabled(t *testing.T) {
	// (b) serve.enabled without agora.enabled must fail fast (else it would
	// silently fall back to the plaintext local listener).
	cfg := &Config{
		Agora: &agora.Config{Serve: &agora.ServeConfig{Enabled: true}},
	}
	err := cfg.validateAgora()
	if err == nil || !strings.Contains(err.Error(), "agora.serve.enabled requires agora.enabled") {
		t.Fatalf("expected serve precondition error, got %v", err)
	}

	cfg.Agora.Enabled = true
	if err := cfg.validateAgora(); err != nil {
		t.Fatalf("unexpected error with agora.enabled: %v", err)
	}
}

func TestValidateAgoraExplicitPublishRequiresServe(t *testing.T) {
	// (c) explicit advertisement.publish:true with serve off is honored loudly.
	publish := true
	cfg := &Config{
		Agora: &agora.Config{
			Enabled:       true,
			Advertisement: &agora.AdvertisementConfig{Publish: &publish},
		},
	}
	err := cfg.validateAgora()
	if err == nil || !strings.Contains(err.Error(), "agora.advertisement.publish requires agora.serve.enabled") {
		t.Fatalf("expected explicit-publish precondition error, got %v", err)
	}

	// once serve is on, explicit publish is allowed.
	cfg.Agora.Serve = &agora.ServeConfig{Enabled: true}
	if err := cfg.validateAgora(); err != nil {
		t.Fatalf("unexpected error with serve enabled: %v", err)
	}

	// an explicit publish:false is an opt-out, not a request — no serve needed.
	noPublish := false
	cfg = &Config{
		Agora: &agora.Config{
			Enabled:       true,
			Advertisement: &agora.AdvertisementConfig{Publish: &noPublish},
		},
	}
	if err := cfg.validateAgora(); err != nil {
		t.Fatalf("explicit publish:false without serve must validate, got %v", err)
	}
}

func TestCollectAgoraTunnelsScoping(t *testing.T) {
	// multi-endpoint local: only endpoint tunnels are collected, deduped, and
	// the single Local.AgoraTunnel is ignored.
	cfg := &Config{
		Providers: &ProvidersConfig{
			OpenAI:    &OpenAIConfig{APIKey: "sk", AgoraTunnel: "openai-egress"},
			Anthropic: &AnthropicConfig{AgoraTunnel: "anthropic-egress"}, // no APIKey -> skipped
			Local: &LocalConfig{
				AgoraTunnel: "single-ignored",
				Endpoints: []LocalEndpointConfig{
					{Name: "a", AgoraTunnel: "local-a"},
					{Name: "b", AgoraTunnel: "local-b"},
					{Name: "c", AgoraTunnel: "local-a"}, // duplicate
				},
			},
		},
	}
	got := collectAgoraTunnels(cfg)
	want := []string{"openai-egress", "local-a", "local-b"}
	if len(got) != len(want) {
		t.Fatalf("collectAgoraTunnels = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("collectAgoraTunnels = %#v, want %#v", got, want)
		}
	}

	// single-local mode: the Local.AgoraTunnel is collected.
	single := &Config{
		Providers: &ProvidersConfig{
			Local: &LocalConfig{AgoraTunnel: "local-single"},
		},
	}
	if got := collectAgoraTunnels(single); len(got) != 1 || got[0] != "local-single" {
		t.Fatalf("single-local collectAgoraTunnels = %#v, want [local-single]", got)
	}
}
