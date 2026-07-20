package providers

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestIsNetworkError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"eof", io.EOF, true},
		{"unexpected eof wrapped", fmt.Errorf("read: %w", io.ErrUnexpectedEOF), true},
		{"net timeout wrapped in url.Error", &url.Error{Op: "Get", Err: netTimeout{}}, true},
		{"context canceled", context.Canceled, false},
		{"context canceled wrapped in url.Error", &url.Error{Op: "Get", Err: context.Canceled}, false},
		{"application error", fmt.Errorf("model not found"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isNetworkError(tt.err); got != tt.want {
				t.Errorf("isNetworkError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// netTimeout is a minimal net.Error for exercising the transport-failure branch.
type netTimeout struct{}

func (netTimeout) Error() string   { return "i/o timeout" }
func (netTimeout) Timeout() bool   { return true }
func (netTimeout) Temporary() bool { return true }

var _ net.Error = netTimeout{}

func TestRoundRobinTransportReplaysFullBodyOnFailover(t *testing.T) {
	var received []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// the dead endpoint refuses connections; the round-robin starts there and
	// must fail over to the live endpoint with the body intact.
	m := NewMultiLocal([]EndpointOption{
		{Name: "dead", BaseURL: "http://127.0.0.1:1"},
		{Name: "live", BaseURL: srv.URL},
	})
	client := m.RoundRobinClient()

	body := `{"model":"test","input":["hello world"]}`
	resp, err := client.Post("http://multi.local/api/embed", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("failover round trip failed: %v", err)
	}
	resp.Body.Close()

	if string(received) != body {
		t.Errorf("failover delivered body %q, want %q", received, body)
	}
}
