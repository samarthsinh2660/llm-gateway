# Agora — Deferred Work

The layer-1 agora integration shipped bind-only serve plus dial-out over the overlay; built behavior lives in [`docs/current/agora.md`](../current/agora.md). The originating spec and work order have been retired now that the work is realized. What remains here is the forward-looking residue: things deliberately left out of the first iteration, kept so a later pass does not have to rediscover why they were skipped.

## Managed-create serve (create-or-bind)

Iteration 1 is bind-only — the gateway never provisions its serve tunnel. The operator creates and deletes the front-door tunnel with `agora tunnel create` / `agora tunnel delete`, and the bind semantics (account-scoped, with the cross-environment relaxation that landed upstream in agora v0.1.5) live in agora rather than a gateway-side create path. A managed-create mode would reintroduce the create-race, leak-on-crash, and ephemeral-grant foot-guns bind-only was chosen to delete; revisit only with a concrete reason to own tunnel provisioning in the gateway.

## Resilient degraded-mode startup

Agora-at-boot is fatal today: an unreachable controller, a failed serve bind, or a failed dial attach stops the gateway. Booting without agora and reconnecting in the background — so a transient agora outage doesn't take down a gateway also serving non-agora providers — is a deliberate later iteration, to be designed against observed behavior rather than guessed at up front.

## Steady-state reconnection policy

Layer-1 hands tunnel lifetime ownership to the gateway. A heartbeat, retry, and re-attach-after-mid-run-drop policy is unbuilt; if reconnection turns out to be needed, it is designed against real failure behavior, not speculatively.

## Relay-side TLS termination / header transforms

The egress relay is a raw TCP passthrough, so TLS is end-to-end (gateway↔upstream) and there is nothing to terminate. A relay that instead terminates TLS to inject headers, rewrite hosts, or audit payloads is a different shape and out of scope.

## Single-tunnel relay-side load balancing for local

Considered and rejected in favor of per-endpoint tunnels, which preserve the gateway's existing load-balancing semantics over the overlay. Revisit only if per-endpoint tunnel sprawl becomes an operational problem.

## UDP / non-HTTP serve modes

The gateway serves HTTP over a direct tcp-mode tunnel, advertising `TunnelHTTP` as catalog metadata. Other serve modes — udp, or tcp carrying something other than HTTP — are SDK-supported but out of scope.
