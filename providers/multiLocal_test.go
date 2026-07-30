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
	"testing/iotest"
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
		{"context deadline exceeded", context.DeadlineExceeded, false},
		{"context deadline exceeded wrapped in url.Error", &url.Error{Op: "Get", Err: context.DeadlineExceeded}, false},
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

// bodyReadErrorTransport returns valid headers but a body that fails mid-read,
// simulating a connection dropped after headers arrive.
type bodyReadErrorTransport struct{}

func (bodyReadErrorTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(iotest.ErrReader(io.ErrUnexpectedEOF)),
	}, nil
}

func TestRoundRobinTransportFailsOverOnBodyReadError(t *testing.T) {
	var served bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served = true
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	// the flaky endpoint returns headers then a body that dies with
	// ErrUnexpectedEOF; that read failure must fail over, not reach the caller.
	m := NewMultiLocal([]EndpointOption{
		{Name: "flaky", BaseURL: "http://flaky.local", HTTPClient: &http.Client{Transport: bodyReadErrorTransport{}}},
		{Name: "live", BaseURL: srv.URL},
	})
	client := m.RoundRobinClient()

	resp, err := client.Post("http://multi.local/api/embed", "application/json", strings.NewReader(`{"input":["x"]}`))
	if err != nil {
		t.Fatalf("expected failover to succeed, got %v", err)
	}
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("body read after failover failed: %v", err)
	}
	if !served || string(got) != `{"ok":true}` {
		t.Errorf("failover did not reach the live endpoint; served=%v body=%q", served, got)
	}
}
