package gateway

import (
	"net/http"
	"sort"
)

// Handler returns the gateway's HTTP handler so a host process can mount the
// gateway routes (/v1/chat/completions, /v1/models, /health) on its own server
// instead of calling Run. UNIUN embeds the gateway this way.
func (g *Gateway) Handler() http.Handler {
	return g.newHandler()
}

// Providers returns the sorted names of the providers that initialized, for the
// host's health and startup reporting.
func (g *Gateway) Providers() []string {
	names := make([]string, 0, len(g.providers))
	for pt := range g.providers {
		names = append(names, string(pt))
	}
	sort.Strings(names)
	return names
}
