package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/michaelquigley/df/dl"
	"github.com/openziti/llm-gateway/agora"
	"github.com/openziti/llm-gateway/providers"
	"github.com/openziti/llm-gateway/routing"
)

type Gateway struct {
	cfg              *Config
	providers        map[providers.ProviderType]providers.Provider
	router           *providers.Router
	semanticRouter   *routing.SemanticRouter
	keyStore         *KeyStore
	share            *Share
	accesses         []*Access
	localHTTPClient  *http.Client
	openaiHTTPClient *http.Client
	agora            *agora.Subsystem
	agoraDial        func(string) (*http.Client, error)
	meters           *meters
	metricsHandler   http.Handler
}

func New(cfg *Config) (_ *Gateway, err error) {
	g := &Gateway{
		cfg:       cfg,
		providers: make(map[providers.ProviderType]providers.Provider),
	}
	defer func() {
		if err != nil {
			g.cleanup()
		}
	}()

	// bring up the agora subsystem before providers — provider wiring needs the
	// per-tunnel dial clients. any failure here is fatal at boot (iteration 1).
	if cfg.Agora != nil && cfg.Agora.Enabled {
		sub, serr := agora.NewSubsystem(agora.SubsystemOptions{
			Config: cfg.Agora,
			Defaults: agora.Defaults{
				InstanceName:    "llm-gateway",
				Description:     "OpenAI-compatible LLM gateway",
				AgentNamePrefix: "llm-gateway",
			},
			Capabilities:  agora.Derive([]string{"llm-routing"}, capabilityExtras(cfg)),
			PublishWanted: cfg.AgoraPublishEnabled(),
		})
		if serr != nil {
			return nil, serr
		}
		g.agora = sub

		// reserve each unique agora tunnel once, front-loading the control-plane
		// attachment out of the request hot path.
		dialer := sub.Dialer()
		ctx := context.Background()
		for _, name := range collectAgoraTunnels(cfg) {
			if aerr := dialer.Attach(ctx, name); aerr != nil {
				return nil, aerr
			}
		}
		g.agoraDial = dialer.HTTPClient
	}

	if err = g.initProviders(); err != nil {
		return nil, err
	}

	g.router = providers.NewRouter(g.providers)

	if cfg.Metrics != nil && cfg.Metrics.Enabled {
		m, handler, err := initMetrics()
		if err != nil {
			return nil, fmt.Errorf("failed to initialize metrics: %w", err)
		}
		g.meters = m
		g.metricsHandler = handler
		dl.Info("initialized opentelemetry metrics")
	}

	if cfg.APIKeys != nil && cfg.APIKeys.Enabled {
		// enabled with no keys must not silently mean open access.
		if len(cfg.APIKeys.Keys) == 0 {
			return nil, fmt.Errorf("api_keys.enabled requires at least one configured key")
		}
		ks, err := NewKeyStore(cfg.APIKeys.Keys)
		if err != nil {
			return nil, err
		}
		g.keyStore = ks
		dl.Infof("loaded %d API key(s)", len(cfg.APIKeys.Keys))
	}

	if cfg.Routing != nil {
		sr, err := g.initSemanticRouter(cfg.Routing)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize semantic router: %w", err)
		}
		g.semanticRouter = sr
	}

	return g, nil
}

func (g *Gateway) initProviders() error {
	if g.cfg.Providers == nil {
		return nil
	}

	// initialize openai provider. secrets are expanded once at config load.
	if g.cfg.Providers.OpenAI != nil && g.cfg.Providers.OpenAI.APIKey != "" {
		apiKey := g.cfg.Providers.OpenAI.APIKey
		baseURL := g.cfg.Providers.OpenAI.BaseURL

		if g.cfg.Providers.OpenAI.AgoraTunnel != "" {
			// agora wins over zrok. pass baseURL through unchanged so the empty
			// case keeps the real HTTPS default and TLS rides the tunnel
			// end-to-end (cloud egress). semantic routing inherits
			// g.openaiHTTPClient, so openai embeddings/classifier ride the
			// overlay too.
			client, derr := g.agoraDial(g.cfg.Providers.OpenAI.AgoraTunnel)
			if derr != nil {
				return derr
			}
			g.openaiHTTPClient = client
			g.providers[providers.ProviderOpenAI] = providers.NewOpenAIWithClient(apiKey, baseURL, client)
			dl.Infof("initialized openai provider via agora tunnel '%s'", g.cfg.Providers.OpenAI.AgoraTunnel)
		} else if g.cfg.Providers.OpenAI.ZrokShareToken != "" {
			access, err := NewAccess(g.cfg.Providers.OpenAI.ZrokShareToken)
			if err != nil {
				return err
			}
			g.accesses = append(g.accesses, access)
			g.openaiHTTPClient = access.HTTPClient()
			g.providers[providers.ProviderOpenAI] = providers.NewOpenAIWithClient(apiKey, baseURL, g.openaiHTTPClient)
			dl.Infof("initialized openai provider via zrok share '%s'", g.cfg.Providers.OpenAI.ZrokShareToken)
		} else {
			g.providers[providers.ProviderOpenAI] = providers.NewOpenAI(apiKey, baseURL)
			if baseURL != "" {
				dl.Infof("initialized openai provider at '%s'", baseURL)
			} else {
				dl.Info("initialized openai provider")
			}
		}
	}

	// initialize anthropic provider
	if g.cfg.Providers.Anthropic != nil && g.cfg.Providers.Anthropic.APIKey != "" {
		apiKey := g.cfg.Providers.Anthropic.APIKey
		baseURL := g.cfg.Providers.Anthropic.BaseURL

		if g.cfg.Providers.Anthropic.AgoraTunnel != "" {
			// agora wins over zrok. baseURL passes through unchanged (TLS rides
			// the tunnel end-to-end for cloud egress).
			client, derr := g.agoraDial(g.cfg.Providers.Anthropic.AgoraTunnel)
			if derr != nil {
				return derr
			}
			g.providers[providers.ProviderAnthropic] = providers.NewAnthropicWithClient(apiKey, baseURL, client)
			dl.Infof("initialized anthropic provider via agora tunnel '%s'", g.cfg.Providers.Anthropic.AgoraTunnel)
		} else if g.cfg.Providers.Anthropic.ZrokShareToken != "" {
			access, err := NewAccess(g.cfg.Providers.Anthropic.ZrokShareToken)
			if err != nil {
				return err
			}
			g.accesses = append(g.accesses, access)
			g.providers[providers.ProviderAnthropic] = providers.NewAnthropicWithClient(apiKey, baseURL, access.HTTPClient())
			dl.Infof("initialized anthropic provider via zrok share '%s'", g.cfg.Providers.Anthropic.ZrokShareToken)
		} else {
			g.providers[providers.ProviderAnthropic] = providers.NewAnthropic(apiKey, baseURL)
			if baseURL != "" {
				dl.Infof("initialized anthropic provider at '%s'", baseURL)
			} else {
				dl.Info("initialized anthropic provider")
			}
		}
	}

	// initialize local provider
	if g.cfg.Providers.Local != nil {
		if len(g.cfg.Providers.Local.Endpoints) > 0 {
			if err := g.initLocalMulti(); err != nil {
				return err
			}
		} else if err := g.initLocalSingle(); err != nil {
			return err
		}
	}

	return nil
}

func (g *Gateway) initLocalSingle() error {
	cfg := g.cfg.Providers.Local
	if cfg.AgoraTunnel != "" {
		// agora wins over zrok. semantic routing inherits g.localHTTPClient, so
		// local embeddings/classifier ride the tunnel for free.
		client, err := g.agoraDial(cfg.AgoraTunnel)
		if err != nil {
			return fmt.Errorf("resolve agora tunnel '%s' for local provider: %w", cfg.AgoraTunnel, err)
		}
		g.localHTTPClient = client
		g.providers[providers.ProviderLocal] = providers.NewLocalWithClient(cfg.BaseURL, g.localHTTPClient)
		dl.Infof("initialized local provider via agora tunnel '%s'", cfg.AgoraTunnel)
	} else if cfg.ZrokShareToken != "" {
		access, err := NewAccess(cfg.ZrokShareToken)
		if err != nil {
			return fmt.Errorf("create zrok access for local provider: %w", err)
		}
		g.accesses = append(g.accesses, access)
		g.localHTTPClient = access.HTTPClient()
		g.providers[providers.ProviderLocal] = providers.NewLocalWithClient(cfg.BaseURL, g.localHTTPClient)
		dl.Infof("initialized local provider via zrok share '%s'", cfg.ZrokShareToken)
	} else {
		g.providers[providers.ProviderLocal] = providers.NewLocal(cfg.BaseURL)
		dl.Infof("initialized local provider at '%s'", cfg.BaseURL)
	}
	return nil
}

func (g *Gateway) initLocalMulti() error {
	cfg := g.cfg.Providers.Local
	opts := make([]providers.EndpointOption, 0, len(cfg.Endpoints))

	for _, ep := range cfg.Endpoints {
		opt := providers.EndpointOption{
			Name:    ep.Name,
			BaseURL: ep.BaseURL,
			Weight:  ep.Weight,
		}
		if ep.AgoraTunnel != "" {
			// agora wins over zrok. drop-in: roundRobinTransport selects each
			// endpoint's own client.Transport per request, so the agora client
			// (and its health checks) route over the tunnel with no extra wiring.
			client, err := g.agoraDial(ep.AgoraTunnel)
			if err != nil {
				return fmt.Errorf("agora dial for endpoint '%s': %w", ep.Name, err)
			}
			opt.HTTPClient = client
		} else if ep.ZrokShareToken != "" {
			access, err := NewAccess(ep.ZrokShareToken)
			if err != nil {
				return fmt.Errorf("failed to create zrok access for endpoint '%s': %w", ep.Name, err)
			}
			g.accesses = append(g.accesses, access)
			opt.HTTPClient = access.HTTPClient()
		}
		opts = append(opts, opt)
	}

	multi := providers.NewMultiLocal(opts)

	// start health checks
	interval := 60 * time.Second
	timeout := 30 * time.Second
	if cfg.HealthCheck != nil {
		if cfg.HealthCheck.IntervalSeconds > 0 {
			interval = time.Duration(cfg.HealthCheck.IntervalSeconds) * time.Second
		}
		if cfg.HealthCheck.TimeoutSeconds > 0 {
			timeout = time.Duration(cfg.HealthCheck.TimeoutSeconds) * time.Second
		}
	}
	multi.StartHealthChecks(interval, timeout)

	g.providers[providers.ProviderLocal] = multi

	for _, ep := range cfg.Endpoints {
		if ep.AgoraTunnel != "" {
			dl.Infof("initialized local endpoint '%s' via agora tunnel '%s'", ep.Name, ep.AgoraTunnel)
		} else if ep.ZrokShareToken != "" {
			dl.Infof("initialized local endpoint '%s' via zrok share '%s'", ep.Name, ep.ZrokShareToken)
		} else {
			dl.Infof("initialized local endpoint '%s' at '%s'", ep.Name, ep.BaseURL)
		}
	}
	dl.Infof("initialized multi-endpoint local provider with %d endpoints", len(cfg.Endpoints))

	return nil
}

// Run serves the gateway handler over every enabled transport at once — the
// plain local listener, a zrok share, and an agora tunnel are independent
// listeners and may run together. The local listener is opt-in (an explicit
// cfg.Listen) or the fallback when no overlay serves, so a credential-firewall
// deployment can stay private-only (agora serve, no local port).
func (g *Gateway) Run() error {
	handler := g.newHandler()

	// setup graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer g.cleanup()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		dl.Info("received shutdown signal")
		cancel()
	}()

	zrokEnabled := g.cfg.Zrok != nil && g.cfg.Zrok.Share != nil && g.cfg.Zrok.Share.Enabled
	agoraEnabled := g.cfg.AgoraServeEnabled()

	type boundServer struct {
		server   *http.Server
		listener net.Listener
		label    string
	}
	var bound []boundServer

	// local tcp — opt-in (explicit listen) or fallback (no overlay serves).
	if g.cfg.Listen != "" || (!zrokEnabled && !agoraEnabled) {
		addr := g.cfg.Listen
		if addr == "" {
			addr = ":8080"
		}
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("listen on '%s': %w", addr, err)
		}
		dl.Infof("listening on '%s'", addr)
		bound = append(bound, boundServer{&http.Server{Handler: handler}, listener, "local"})
	}

	// zrok share.
	if zrokEnabled {
		var share *Share
		var err error
		if g.cfg.Zrok.Share.Token != "" {
			// use existing persistent share (private only). an explicit
			// conflicting mode must not be silently overridden.
			if mode := g.cfg.Zrok.Share.Mode; mode != "" && mode != "private" {
				return fmt.Errorf("zrok.share.mode '%s' conflicts with a persistent share token; persistent shares are always private", mode)
			}
			share, err = NewShareFromToken(g.cfg.Zrok.Share.Token)
		} else {
			share, err = NewShare(g.cfg.Zrok.Share.Mode)
		}
		if err != nil {
			return err
		}
		g.share = share
		dl.Infof("serving via zrok share '%s'", share.Token())
		bound = append(bound, boundServer{&http.Server{Handler: handler}, share.Listener(), "zrok"})
	}

	// agora tunnel (bind-only).
	if agoraEnabled {
		serve, err := g.agora.Serve(ctx)
		if err != nil {
			return err
		}
		dl.Infof("serving via agora tunnel '%s'", g.agora.ServeTunnelName())
		bound = append(bound, boundServer{&http.Server{Handler: handler}, serve.Listener(), "agora"})
	}

	if len(bound) == 0 {
		return fmt.Errorf("no gateway listeners are configured")
	}

	// size errCh to the listener count so no serve goroutine blocks on send
	// during shutdown (the gateway can have three listeners).
	servers := make([]*http.Server, 0, len(bound))
	errCh := make(chan error, len(bound))
	for _, b := range bound {
		servers = append(servers, b.server)
		serveHTTP(b.server, b.listener, errCh, b.label)
	}

	// publish only after the listeners are live; a publish failure tears down all.
	if g.cfg.AgoraPublishEnabled() {
		if err := g.agora.StartPublishing(ctx); err != nil {
			shutdownHTTPServers(servers)
			return err
		}
	}

	select {
	case <-ctx.Done():
		dl.Info("shutting down servers")
		return shutdownHTTPServers(servers)
	case err := <-errCh:
		shutdownHTTPServers(servers)
		return err
	}
}

// serveHTTP serves one listener on its own goroutine, translating the expected
// graceful-shutdown errors to nil and routing real failures to errCh.
func serveHTTP(server *http.Server, listener net.Listener, errCh chan<- error, label string) {
	go func() {
		err := server.Serve(listener)
		if err == nil || errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
			errCh <- nil
			return
		}
		errCh <- fmt.Errorf("%s listener failed: %w", label, err)
	}()
}

// shutdownHTTPServers gracefully shuts down every server, joining any errors.
func shutdownHTTPServers(servers []*http.Server) error {
	var errs []error
	for _, server := range servers {
		if err := server.Shutdown(context.Background()); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (g *Gateway) cleanup() {
	for _, p := range g.providers {
		if c, ok := p.(io.Closer); ok {
			c.Close()
		}
	}
	if g.share != nil {
		g.share.Close()
	}
	for _, access := range g.accesses {
		access.Close()
	}
	if g.agora != nil {
		// retracts the advertisement, closes the serve listener (no delete —
		// bind-only), detaches every dial tunnel, and closes the agent.
		if err := g.agora.Close(); err != nil {
			dl.Warnf("error closing agora subsystem: %v", err)
		}
	}
}

func (g *Gateway) initSemanticRouter(cfg *routing.RoutingConfig) (*routing.SemanticRouter, error) {
	var embedClient routing.Embedder

	// initialize embedding client if semantic matching is enabled
	if cfg.Semantic != nil && cfg.Semantic.Enabled {
		baseURL, apiKey, httpClient, err := g.resolveRoutingProvider(cfg.Semantic.Provider)
		if err != nil {
			return nil, fmt.Errorf("embedding provider: %w", err)
		}
		if httpClient != nil {
			embedClient = routing.NewEmbedClientWithHTTPClient(cfg.Semantic.Provider, cfg.Semantic.Model, baseURL, apiKey, httpClient)
		} else {
			embedClient = routing.NewEmbedClient(cfg.Semantic.Provider, cfg.Semantic.Model, baseURL, apiKey)
		}
	}

	// resolve classifier provider connection details
	var classifierBaseURL, classifierAPIKey string
	var classifierHTTPClient *http.Client
	if cfg.Classifier != nil && cfg.Classifier.Enabled {
		var err error
		classifierBaseURL, classifierAPIKey, classifierHTTPClient, err = g.resolveRoutingProvider(cfg.Classifier.Provider)
		if err != nil {
			return nil, fmt.Errorf("classifier provider: %w", err)
		}
	}

	ctx := context.Background()
	return routing.NewSemanticRouterWithClassifier(ctx, cfg, embedClient, classifierBaseURL, classifierAPIKey, classifierHTTPClient)
}

// resolveRoutingProvider looks up connection details from provider config. it
// owns the configured-and-usable gate: a missing block, a keyless openai
// block, and an unknown provider name are all directed errors rather than
// values that fail later at runtime.
func (g *Gateway) resolveRoutingProvider(provider string) (baseURL, apiKey string, httpClient *http.Client, err error) {
	if g.cfg.Providers == nil {
		return "", "", nil, fmt.Errorf("routing provider '%s' not configured", provider)
	}

	switch provider {
	case "local":
		if g.cfg.Providers.Local == nil {
			return "", "", nil, fmt.Errorf("routing provider 'local' not configured")
		}
		if multi, ok := g.providers[providers.ProviderLocal].(*providers.MultiLocal); ok {
			baseURL = multi.PrimaryBaseURL()
			httpClient = multi.RoundRobinClient()
		} else {
			baseURL = g.cfg.Providers.Local.BaseURL
			if baseURL == "" {
				baseURL = "http://localhost:11434"
			}
			httpClient = g.localHTTPClient
		}
	case "openai":
		if g.cfg.Providers.OpenAI == nil {
			return "", "", nil, fmt.Errorf("routing provider 'openai' not configured")
		}
		if g.cfg.Providers.OpenAI.APIKey == "" {
			return "", "", nil, fmt.Errorf("routing provider 'openai' requires providers.openai.api_key")
		}
		apiKey = g.cfg.Providers.OpenAI.APIKey
		baseURL = g.cfg.Providers.OpenAI.BaseURL
		if baseURL == "" {
			baseURL = "https://api.openai.com"
		}
		// overlay-backed when the provider is; base URL stays unchanged so
		// TLS rides the overlay end-to-end.
		httpClient = g.openaiHTTPClient
	default:
		return "", "", nil, fmt.Errorf("unknown routing provider '%s' (expected 'local' or 'openai')", provider)
	}

	return baseURL, apiKey, httpClient, nil
}
