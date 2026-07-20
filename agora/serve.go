package agora

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/openziti/agora/sdk/agent/tunnel"
)

// Serve is a handle to a bind-only Agora serve listener, parallel to the
// gateway's zrok Share. the front-door tunnel is operator-provisioned: the
// gateway binds to a tunnel its account already owns and never creates or
// deletes one. Close closes the listener only; the operator-owned tunnel is
// left intact.
type Serve struct {
	sub      *Subsystem
	listener net.Listener
	closed   bool
}

// Serve resolves the operator-provisioned serve tunnel and binds to it. The
// tunnel must already exist as a direct, tcp-mode tunnel; the gateway never
// creates it. any resolution/listen failure is fatal (iteration 1).
func (s *Subsystem) Serve(ctx context.Context) (*Serve, error) {
	if s == nil {
		return nil, fmt.Errorf("agora subsystem is nil")
	}
	name := s.ServeTunnelName()
	if name == "" {
		return nil, fmt.Errorf("agora serve tunnel name is unresolved")
	}

	// validate the tunnel exists and is tcp-mode before binding. Listen itself
	// accepts http-mode tunnels, so the mode check is explicit. requireTCPMode
	// resolves via GetTunnel, so an unprovisioned tunnel fails here with a
	// wrapped ErrNotFound.
	if err := s.requireTCPMode(ctx, name); err != nil {
		return nil, err
	}

	listener, err := s.ops.Listen(ctx, s.agent, name)
	if err != nil {
		if errors.Is(err, tunnel.ErrNotFound) {
			// rare race: requireTCPMode resolved the tunnel, then it was deleted
			// before Listen. same directed wording as requireTCPMode.
			return nil, fmt.Errorf("agora serve tunnel '%s' is not provisioned; provision it operator-side (bind-only): %w", name, err)
		}
		return nil, fmt.Errorf("listen on agora tunnel '%s': %w", name, err)
	}

	s.log.Infof("agora serve bound to tunnel '%s'", name)
	sv := &Serve{sub: s, listener: listener}
	s.serve = sv
	return sv, nil
}

// requireTCPMode fails fast when binding to a tunnel that does not exist or
// whose mode is not TCP. Listen does not enforce TCP-mode (it accepts http-mode
// tunnels), so the check is explicit.
func (s *Subsystem) requireTCPMode(ctx context.Context, name string) error {
	existing, err := s.ops.GetTunnel(ctx, s.agent, name)
	if err != nil {
		if errors.Is(err, tunnel.ErrNotFound) {
			return fmt.Errorf("agora serve tunnel '%s' is not provisioned; provision it operator-side (bind-only): %w", name, err)
		}
		return fmt.Errorf("resolve agora serve tunnel '%s': %w", name, err)
	}
	if existing == nil {
		return fmt.Errorf("agora serve tunnel '%s' could not be resolved", name)
	}
	if !strings.EqualFold(string(existing.Mode), string(tunnel.ModeTCP)) {
		return fmt.Errorf("agora serve tunnel '%s' exists with mode '%s', expected tcp", name, existing.Mode)
	}
	return nil
}

// Listener returns the net.Listener the gateway's HTTP handler binds to.
func (sv *Serve) Listener() net.Listener {
	if sv == nil {
		return nil
	}
	return sv.listener
}

// Close closes the listener only — bind-only never deletes the operator-owned
// tunnel.
func (sv *Serve) Close(ctx context.Context) error {
	if sv == nil || sv.closed {
		return nil
	}
	sv.closed = true
	if sv.listener != nil {
		if err := sv.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			return err
		}
	}
	return nil
}
