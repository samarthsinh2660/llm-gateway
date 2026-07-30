package routing

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/michaelquigley/df/dl"
)

// Method identifies how a routing decision was made.
type Method string

const (
	MethodExplicit   Method = "explicit"
	MethodHeuristic  Method = "heuristic"
	MethodSemantic   Method = "semantic"
	MethodClassifier Method = "classifier"
	MethodDefault    Method = "default"
	MethodCapability Method = "capability"
)

// wire contract mirror: github.com/netfoundry/sterling/internal/recipe carries
// the same constants and a matching golden-string test. changes must land in
// both repositories.
// CapabilityVocabularyV1 is Sterling's gateway-owned route vocabulary. a
// capability class is resolved against the configured route name; the
// concrete model remains local gateway policy.
const CapabilityVocabularyV1 = "sterling-classes/v1"

// CapabilityFrontierCoding is the sole class in Sterling's v1 vocabulary.
const CapabilityFrontierCoding = "frontier-coding"

// CapabilityModelPrefix marks an OpenAI model ID as a Sterling capability
// alias rather than a concrete provider model.
const CapabilityModelPrefix = "sterling-capability:"

// Decision describes the result of a routing decision.
type Decision struct {
	Route      string
	Model      string
	Method     Method
	Confidence float64
	LatencyMs  int64
	Cascade    []string
}

// RequestInfo is a provider-independent representation of a chat request.
type RequestInfo struct {
	Model     string
	Messages  []MessageInfo
	MaxTokens *int
	HasTools  bool
}

// MessageInfo is a simplified message for routing decisions.
type MessageInfo struct {
	Role    string
	Content string
}

// Embedder is the interface for generating text embeddings.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float64, error)
	EmbedBatch(ctx context.Context, texts []string) ([][]float64, error)
}

// SemanticRouter orchestrates the three-layer routing cascade.
type SemanticRouter struct {
	cfg        *RoutingConfig
	heuristics *HeuristicMatcher
	embeddings *EmbeddingMatcher
	classifier *ClassifierMatcher
	routeMap   map[string]RouteConfig
}

// validateRoutes checks the route set and its references at construction time
// and returns the name→route lookup. Each failure here is a misconfiguration
// that would otherwise surface later as a silently dead rule, a phantom
// fallback, or a matching layer that can never produce a decision.
func validateRoutes(cfg *RoutingConfig) (map[string]RouteConfig, error) {
	routeMap := make(map[string]RouteConfig, len(cfg.Routes))
	for i, r := range cfg.Routes {
		switch {
		case r.Name == "":
			return nil, fmt.Errorf("route %d has an empty name", i)
		case r.Model == "":
			return nil, fmt.Errorf("route %d ('%s') has an empty model", i, r.Name)
		}
		if _, dup := routeMap[r.Name]; dup {
			return nil, fmt.Errorf("route %d ('%s') duplicates an earlier route name", i, r.Name)
		}
		routeMap[r.Name] = r
	}

	// a reference that does not resolve is a dead rule or a phantom fallback.
	if cfg.DefaultRoute != "" {
		if _, ok := routeMap[cfg.DefaultRoute]; !ok {
			return nil, fmt.Errorf("default_route references unknown route '%s'", cfg.DefaultRoute)
		}
	}
	if cfg.Heuristics != nil && cfg.Heuristics.Enabled {
		for i, rule := range cfg.Heuristics.Rules {
			if _, ok := routeMap[rule.Route]; !ok {
				return nil, fmt.Errorf("heuristic rule %d references unknown route '%s'", i, rule.Route)
			}
		}
	}

	// a routing block with no routes can never resolve a model — every layer
	// selects into the route set — yet a configured block still advertises the
	// 'auto' model and forces model-less requests through the cascade. refuse it,
	// keeping the directed message for the matcher-specific cases.
	if len(cfg.Routes) == 0 {
		switch {
		case cfg.Semantic != nil && cfg.Semantic.Enabled:
			return nil, fmt.Errorf("routing.semantic.enabled requires at least one route")
		case cfg.Classifier != nil && cfg.Classifier.Enabled:
			return nil, fmt.Errorf("routing.classifier.enabled requires at least one route")
		default:
			return nil, fmt.Errorf("routing requires at least one route")
		}
	}

	// an enabled classifier needs a model to call. unlike the embedding matcher
	// (which embeds at startup and so fails loud on a bad model) the classifier
	// is constructed lazily, so an empty model would otherwise surface only at
	// request time as a silent fall-through to the default route.
	if cfg.Classifier != nil && cfg.Classifier.Enabled && cfg.Classifier.Model == "" {
		return nil, fmt.Errorf("routing.classifier.model is required when routing.classifier.enabled is true")
	}

	return routeMap, nil
}

// NewSemanticRouter creates a new SemanticRouter, embedding exemplars at startup.
func NewSemanticRouter(ctx context.Context, cfg *RoutingConfig, embedClient Embedder) (*SemanticRouter, error) {
	routeMap, err := validateRoutes(cfg)
	if err != nil {
		return nil, err
	}
	sr := &SemanticRouter{
		cfg:      cfg,
		routeMap: routeMap,
	}

	// layer 1: heuristics
	if cfg.Heuristics != nil && cfg.Heuristics.Enabled {
		sr.heuristics = NewHeuristicMatcher(cfg.Heuristics.Rules)
		dl.Info("initialized heuristic matcher")
	}

	// layer 2: embedding similarity
	if cfg.Semantic != nil && cfg.Semantic.Enabled && embedClient != nil {
		em, err := NewEmbeddingMatcher(ctx, embedClient, cfg.Routes, cfg.Semantic)
		if err != nil {
			return nil, err
		}
		sr.embeddings = em
		dl.Info("initialized embedding matcher")
	}

	// layer 3: llm classifier
	if cfg.Classifier != nil && cfg.Classifier.Enabled {
		sr.classifier = NewClassifierMatcher(cfg.Classifier, cfg.Routes, "", "")
		dl.Info("initialized classifier matcher")
	}

	return sr, nil
}

// NewSemanticRouterWithClassifier creates a SemanticRouter with explicit classifier connection details.
// if httpClient is non-nil it is used for classifier requests (e.g. for zrok transport).
func NewSemanticRouterWithClassifier(ctx context.Context, cfg *RoutingConfig, embedClient Embedder, classifierBaseURL, classifierAPIKey string, httpClient *http.Client) (*SemanticRouter, error) {
	sr, err := NewSemanticRouter(ctx, cfg, embedClient)
	if err != nil {
		return nil, err
	}

	if cfg.Classifier != nil && cfg.Classifier.Enabled {
		if httpClient != nil {
			sr.classifier = NewClassifierMatcherWithHTTPClient(cfg.Classifier, cfg.Routes, classifierBaseURL, classifierAPIKey, httpClient)
		} else {
			sr.classifier = NewClassifierMatcher(cfg.Classifier, cfg.Routes, classifierBaseURL, classifierAPIKey)
		}
		dl.Info("initialized classifier matcher with explicit provider config")
	}

	return sr, nil
}

// Enabled returns true if semantic routing is configured.
func (sr *SemanticRouter) Enabled() bool {
	return sr != nil && sr.cfg != nil
}

// AllowsExplicitModel reports the configured explicit-model policy.
func (sr *SemanticRouter) AllowsExplicitModel() bool {
	return sr != nil && sr.cfg != nil && sr.cfg.AllowExplicit()
}

// Route performs the routing cascade and returns a decision.
func (sr *SemanticRouter) Route(ctx context.Context, info *RequestInfo) (*Decision, error) {
	start := time.Now()
	var cascade []string

	// explicit model passthrough
	if info.Model != "" && sr.cfg.AllowExplicit() {
		return &Decision{
			Model:      info.Model,
			Method:     MethodExplicit,
			Confidence: 1.0,
			LatencyMs:  time.Since(start).Milliseconds(),
			Cascade:    []string{fmt.Sprintf("explicit:%s", info.Model)},
		}, nil
	}

	// layer 1: heuristics
	if sr.heuristics != nil {
		if route := sr.heuristics.Match(info); route != "" {
			cascade = append(cascade, fmt.Sprintf("heuristic:%s", route))
			if rc, ok := sr.routeMap[route]; ok {
				return &Decision{
					Route:      route,
					Model:      rc.Model,
					Method:     MethodHeuristic,
					Confidence: 1.0,
					LatencyMs:  time.Since(start).Milliseconds(),
					Cascade:    cascade,
				}, nil
			}
		} else {
			cascade = append(cascade, "heuristic:no_match")
		}
	}

	// layer 2: embedding similarity
	if sr.embeddings != nil {
		route, confidence, err := sr.embeddings.Match(ctx, info)
		if err != nil {
			dl.Errorf("embedding match error: %v", err)
			cascade = append(cascade, "semantic:error")
		} else if route != "" {
			// check if confident enough
			threshold := sr.cfg.Semantic.Threshold
			ambiguous := sr.cfg.Semantic.AmbiguousThreshold

			if confidence >= threshold {
				cascade = append(cascade, fmt.Sprintf("semantic:%s:%.2f", route, confidence))
				if rc, ok := sr.routeMap[route]; ok {
					return &Decision{
						Route:      route,
						Model:      rc.Model,
						Method:     MethodSemantic,
						Confidence: confidence,
						LatencyMs:  time.Since(start).Milliseconds(),
						Cascade:    cascade,
					}, nil
				}
			}

			// ambiguous: escalate to classifier if available
			if confidence >= ambiguous && sr.classifier != nil {
				cascade = append(cascade, fmt.Sprintf("semantic:%s:%.2f:ambiguous", route, confidence))
				cRoute, cConf, cErr := sr.classifier.Classify(ctx, info)
				if cErr != nil {
					dl.Errorf("classifier error: %v", cErr)
					cascade = append(cascade, "classifier:error")
				} else if cRoute != "" && cConf >= sr.cfg.Classifier.ConfidenceThreshold {
					cascade = append(cascade, fmt.Sprintf("classifier:%s:%.2f", cRoute, cConf))
					if rc, ok := sr.routeMap[cRoute]; ok {
						return &Decision{
							Route:      cRoute,
							Model:      rc.Model,
							Method:     MethodClassifier,
							Confidence: cConf,
							LatencyMs:  time.Since(start).Milliseconds(),
							Cascade:    cascade,
						}, nil
					}
				} else if cRoute != "" {
					// record the candidate the classifier declined on, for tuning.
					cascade = append(cascade, fmt.Sprintf("classifier:%s:%.2f:no_match", cRoute, cConf))
				} else {
					cascade = append(cascade, "classifier:no_match")
				}
			} else {
				// below the ambiguous window (or no classifier): keep the
				// candidate and score that caused the decline.
				cascade = append(cascade, fmt.Sprintf("semantic:%s:%.2f:no_match", route, confidence))
			}
		} else {
			cascade = append(cascade, "semantic:no_match")
		}
	} else if sr.classifier != nil {
		// no embeddings configured, try classifier directly
		route, confidence, err := sr.classifier.Classify(ctx, info)
		if err != nil {
			dl.Errorf("classifier error: %v", err)
			cascade = append(cascade, "classifier:error")
		} else if route != "" && confidence >= sr.cfg.Classifier.ConfidenceThreshold {
			cascade = append(cascade, fmt.Sprintf("classifier:%s:%.2f", route, confidence))
			if rc, ok := sr.routeMap[route]; ok {
				return &Decision{
					Route:      route,
					Model:      rc.Model,
					Method:     MethodClassifier,
					Confidence: confidence,
					LatencyMs:  time.Since(start).Milliseconds(),
					Cascade:    cascade,
				}, nil
			}
		} else if route != "" {
			// record the candidate the classifier declined on, for tuning.
			cascade = append(cascade, fmt.Sprintf("classifier:%s:%.2f:no_match", route, confidence))
		} else {
			cascade = append(cascade, "classifier:no_match")
		}
	}

	// default route
	if sr.cfg.DefaultRoute != "" {
		if rc, ok := sr.routeMap[sr.cfg.DefaultRoute]; ok {
			cascade = append(cascade, fmt.Sprintf("default:%s", sr.cfg.DefaultRoute))
			return &Decision{
				Route:      sr.cfg.DefaultRoute,
				Model:      rc.Model,
				Method:     MethodDefault,
				Confidence: 0,
				LatencyMs:  time.Since(start).Milliseconds(),
				Cascade:    cascade,
			}, nil
		}
	}

	// absolute fallback: use first route
	if len(sr.cfg.Routes) > 0 {
		rc := sr.cfg.Routes[0]
		cascade = append(cascade, fmt.Sprintf("default:%s", rc.Name))
		return &Decision{
			Route:      rc.Name,
			Model:      rc.Model,
			Method:     MethodDefault,
			Confidence: 0,
			LatencyMs:  time.Since(start).Milliseconds(),
			Cascade:    cascade,
		}, nil
	}

	cascade = append(cascade, "default")
	return &Decision{
		Method:    MethodDefault,
		LatencyMs: time.Since(start).Milliseconds(),
		Cascade:   cascade,
	}, nil
}

// ResolveCapability resolves a signed Sterling capability coordinate without
// running the request-routing cascade. capability classes name gateway-owned
// routes, so the result is deterministic for the gateway configuration at the
// time of resolution.
func (sr *SemanticRouter) ResolveCapability(vocabulary, class string) (*Decision, error) {
	if vocabulary != CapabilityVocabularyV1 {
		return nil, fmt.Errorf("unsupported capability vocabulary %q", vocabulary)
	}
	if class != CapabilityFrontierCoding {
		return nil, fmt.Errorf("unknown capability class %q in vocabulary %q", class, vocabulary)
	}
	route, ok := sr.routeMap[class]
	if !ok {
		return nil, fmt.Errorf("unknown capability class %q", class)
	}
	return &Decision{
		Route:      class,
		Model:      route.Model,
		Method:     MethodCapability,
		Confidence: 1,
		Cascade:    []string{"capability:" + class},
	}, nil
}

// IsCapabilityModel reports whether model uses Sterling's capability-alias
// namespace.
func IsCapabilityModel(model string) bool {
	return strings.HasPrefix(model, CapabilityModelPrefix)
}

// ResolveCapabilityModel parses and resolves a capability carried in the
// OpenAI model field. the alias shape is
// sterling-capability:<vocabulary>/<class>. Sterling's signed vocabulary
// grammar is exactly <segment>/v<N>, so the complete alias has three
// slash-separated parts; changing that grammar requires coordinated changes
// to Sterling's builder and this parser.
func (sr *SemanticRouter) ResolveCapabilityModel(model string) (*Decision, error) {
	value, ok := strings.CutPrefix(model, CapabilityModelPrefix)
	if !ok {
		return nil, fmt.Errorf("model %q is not a capability alias", model)
	}
	parts := strings.Split(value, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return nil, fmt.Errorf("malformed capability model %q", model)
	}
	return sr.ResolveCapability(parts[0]+"/"+parts[1], parts[2])
}
