# Agora Integration

[Agora](https://openziti.io) is an overlay network built on [OpenZiti](https://openziti.io). The gateway uses it the same two ways it uses [zrok](zrok.md), and the two transports sit side by side as peers:

1. **Serving** -- exposing the gateway itself over an Agora tunnel so clients reach it privately over the overlay, no public IP or open port.
2. **Dialing** -- reaching a backend provider (OpenAI, Anthropic, local/self-hosted) that lives behind an Agora tunnel instead of over direct HTTP.

The two sides are independent. A credential-firewall deployment serves over Agora and dials the cloud directly; a gateway fronting private inference dials over Agora and need not serve over it; a deployment can do both. Agora and zrok can also run at the same time -- the gateway serves the same handler over every enabled listener concurrently.

The machine running the gateway must have an enrolled Agora environment.

## Prerequisites

Agora mode requires:

- An enrolled Agora environment on the host running the gateway.
- A reachable Agora controller and a Ziti fabric reachable by the Agora runtime.

The enrolled environment is the source of truth for the controller endpoint. `agora.api_endpoint` is an optional validate-only cross-check: when set, the gateway compares it with the endpoint in the enrolled environment and exits if they differ. When unset, the enrolled endpoint is used as-is.

**Startup is fatal on failure.** If the controller is unreachable, or a serve bind or a dial attach fails at boot, the gateway does **not** start. There is no degraded-mode startup in this iteration -- an Agora misconfiguration surfaces loudly rather than silently dropping the overlay.

## Configuration

Agora is configured under a top-level `agora:` block, plus an `agora_tunnel` field on any provider or endpoint you want to dial over the overlay.

```yaml
agora:
  enabled: true
  integration_file: ""          # optional; see "Integration File"

  api_endpoint: ""              # optional validate-only cross-check
  env_root: ""                  # optional; SDK default or AGORA_ENV_ROOT may apply

  instance_name: "llm-gateway"  # identity + default serve-tunnel name
  description: "OpenAI-compatible LLM gateway"

  serve:
    enabled: true               # serve the gateway over an Agora tunnel
    tunnel: "llm-gateway"        # bind-target tunnel name; default: instance_name

  advertisement:
    publish: true               # default true when agora.enabled is true
    workgroup_ids:
      - wg_abcdefghijkl          # required when publishing
    contract_id: con_abcdefghijkl
    capabilities: []            # derived from config when empty
```

`agora.enabled: true` is required whenever any `agora_tunnel` is set on a provider/endpoint or `agora.serve.enabled: true` is set; the gateway fails fast at startup otherwise (`agora_tunnel set on a provider/endpoint requires agora.enabled: true`, or `agora.serve.enabled requires agora.enabled: true`).

## Integration File

The integration file is a partial `agora:` block, normally produced by provisioning or demo-bootstrap tooling. It carries provisioned environment and catalog IDs while the main config keeps operator choices.

```yaml
api_endpoint: "http://127.0.0.1:18081"
env_root: "/home/example/.agora/envs/llm-gateway@org"
advertisement:
  workgroup_ids:
    - wg_abcdefghijkl
  contract_id: con_abcdefghijkl
```

These fields merge into the main config only when the same field is unset there: `api_endpoint`, `env_root`, `advertisement.workgroup_ids` (only when no inline IDs), and `advertisement.contract_id` (only when no inline ID). Values in the main config win over the integration file.

## Advertisement

When `agora.enabled: true`, advertisement publishing defaults to enabled. Publishing requires workgroup scope IDs (controller-enforced), so `advertisement.publish` behaves as a tri-state:

- **Unset (default-on):** publishes when workgroup IDs are available; when none are configured, the gateway logs a notice and runs serve-only instead of failing.
- **Explicit `true`:** missing workgroup IDs are a hard error.
- **Explicit `false`:** never publishes.

**Publishing requires serving.** In this iteration a dial-only gateway never publishes -- the advertisement name is the front-door tunnel clients dial, and a gateway that does not bind that tunnel must not advertise it. An explicit `advertisement.publish: true` with `agora.serve.enabled: false` is a directed startup error (`agora.advertisement.publish requires agora.serve.enabled in this iteration`) rather than a silently dead catalog card.

When publishing is enabled, `advertisement.capabilities` (when empty) is derived from the configured providers and routing:

| Condition | Capability |
|---|---|
| Always | `llm-routing` |
| OpenAI configured (API key set) | `openai` |
| Anthropic configured (API key set) | `anthropic` |
| Local provider configured | `local` |
| Semantic matching or the classifier enabled | `semantic-routing` |
| Agora serve enabled | `agora-serve` |

The advertisement labels the tunnel mode `http` -- discovery metadata describing what a client speaks, not a transport switch.

## Serving Over Agora

Set `agora.serve.enabled: true` to serve the gateway over an Agora tunnel. The gateway binds its existing HTTP handler directly to the `net.Listener` the SDK's thin `Listen` primitive returns -- no loopback hop, the security boundary is the fabric exactly as it is for zrok.

**Serving is bind-only.** The front-door tunnel is **operator-provisioned**; the gateway only binds to it and never creates or deletes it. Provision it out-of-band as a **direct, tcp-mode** tunnel:

```bash
agora tunnel create llm-gateway        # direct tcp-mode tunnel, in the gateway's account
# ... run the gateway with agora.serve.enabled: true, serve.tunnel: llm-gateway ...
agora tunnel delete llm-gateway        # tears it down when you're done
```

A tunnel under the serve name whose mode is not TCP, or that is not provisioned at all, is a hard startup error rather than a silent fallback.

Bind-only is deliberate: a gateway-side create-or-bind path was rejected because provisioning in the gateway reintroduces a create race between instances, a tunnel leak when a creating process crashes before its delete, and an ephemeral-grant shape where access only works because the creator happened to be the binder. Owning the front-door tunnel's lifecycle operator-side deletes all three, leaving the gateway one testable obligation: bind what it was given or refuse loudly.

**Bind is account-scoped; grants are separate.** The gateway binds a tunnel its **account** owns -- binding is conferred by account ownership, not by grants. Grants are for the **clients/dialers** that should be allowed to reach the served tunnel; document and apply them as client access, not as bind permission. (Current Agora additionally requires the tunnel to live in the gateway's own enrolled environment; an upcoming Agora update relaxes that to any account-owned environment, served one at a time.)

The gateway serves HTTP over the direct tcp-mode tunnel and advertises `tunnel_mode: http` as catalog metadata -- the mode (tcp) and the catalog label (http) are deliberately distinct.

## Dialing Providers via Agora

Any provider or endpoint can be reached over an Agora tunnel by setting `agora_tunnel` on its config. At startup the gateway **attaches** each unique tunnel once -- a control-plane reservation kept out of the request hot path -- and hands each provider a shared HTTP client whose `DialContext` returns a raw `net.Conn` from the SDK's `Dial` primitive. The attachment is released once, at process shutdown.

```yaml
providers:
  local:
    agora_tunnel: "local-inference"

  open_ai:
    api_key: "${OPENAI_API_KEY}"
    agora_tunnel: "openai-egress"
```

The far end of a dial tunnel is **not** part of the gateway -- Agora ships it. Run `agora tunnel serve <name> --mode http --backend <addr>` next to the backend; it provisions a tunnel and forwards it to a local `--backend`. The gateway dials the tunnel; the tool answers it.

**The base URL passes through unchanged** -- the Agora branch never rewrites it, exactly like the zrok branch. Two cases follow from that one rule:

- **Opaque / local backend** -- the upstream ignores the host, so any stable base URL works (the provider's default is fine; an operator may set a cosmetic `http://<tunnel-name>` if preferred). Plain HTTP rides the tunnel.
- **Cloud egress** -- keep the real `https://...` base URL (or leave it empty so OpenAI/Anthropic default to their real HTTPS endpoint). The provider's transport originates TLS *over* the Agora connection with correct SNI and `Host`, and `agora tunnel serve` is a raw TCP passthrough to the upstream. **TLS is end-to-end**, gateway to upstream -- the relay sees only ciphertext.

### Transport precedence: agora wins

When a provider or endpoint sets **both** `agora_tunnel` and `zrok_share_token`, the gateway dials it over **Agora**. This is a documented rule, not a silent pick. (Serving over zrok and over Agora are independent listeners and may both run.)

### Multi-endpoint

Each endpoint independently chooses Agora, zrok, or direct HTTP:

```yaml
providers:
  local:
    endpoints:
      - name: gpu-a
        agora_tunnel: "infer-a"
      - name: gpu-b
        agora_tunnel: "infer-b"
      - name: gpu-c
        zrok_share_token: "gpu-c-token"
```

The round-robin load balancer selects each endpoint's own HTTP transport per request, so per-endpoint Agora clients slot in with no extra wiring. Per-endpoint health checks ride each endpoint's own client, so an Agora-tunneled endpoint's health reflects real tunnel reachability -- a down tunnel marks the endpoint unhealthy and rotates it out.

### Semantic routing

When semantic routing uses `provider: local`, the embedding and classifier layers inherit the resolved local HTTP client, so they ride the Agora transport automatically with no extra config. The **OpenAI** semantic-routing path -- both embeddings *and* the classifier -- does **not** inherit a provider transport in this iteration (zrok has the identical gap), so OpenAI semantic routing over Agora is out of scope.

## Scenarios

**Credential firewall (primary cloud pattern).** Serve over Agora, hold the real provider keys, vend virtual keys, and dial the cloud directly. Agents reach the gateway privately over the overlay; the real keys never leave it. Omit `listen` so no plaintext local port is opened.

```yaml
# no top-level `listen:` -> private-only (overlay serve opens the only listener)
agora:
  enabled: true
  serve:
    enabled: true
    tunnel: "llm-gateway"

providers:
  open_ai:
    api_key: "${OPENAI_API_KEY}"      # real key, dialed directly over the internet
```

Setting `listen` opts back into a local TCP listener alongside the overlay.

**Local inference over Agora (primary dial pattern).** Each local endpoint is exposed by its own `agora tunnel serve --mode http --backend http://localhost:11434` next to the inference server and named in a `LocalEndpointConfig.agora_tunnel`. Round-robin balances across the overlay tunnels; the embedding matcher and classifier reach local inference through the same rotating transport for free.

**Cloud egress over Agora (secondary dial pattern).** `providers.open_ai.agora_tunnel: openai-egress`, where `openai-egress` is an `agora tunnel serve` raw TCP passthrough to `api.openai.com:443`. The provider keeps its real `https://api.openai.com` base URL; TLS rides the tunnel end-to-end and the relay sees only ciphertext. Used when the gateway has no direct internet route and must exit through a controlled relay.

## Lifecycle

Agora startup follows this order:

1. Load config; resolve env vars and the integration file; validate.
2. Construct the runtime-less Agora subsystem (one `*agent.Agent`, no embedded runtime).
3. Attach each unique dial tunnel.
4. Initialize providers (Agora-dialed providers get their shared clients).
5. Bind the serve tunnel's `net.Listener` and serve the handler over it (alongside any zrok and local listeners).
6. Publish the advertisement when enabled, under the resolved serve-tunnel name.

On shutdown the subsystem retracts the advertisement, closes the serve listener (no delete -- bind-only), detaches every dial attachment, and closes the Agora agent, continuing even if one step fails. The thin primitives carry no heartbeat or active healing: a revoked tunnel surfaces as a `net.Listener` or `net.Conn` error, matching zrok's posture.

## Manual Smoke

Run these against a live Agora controller and Ziti fabric:

| Scenario | Expected observation |
|---|---|
| Dial: `agora tunnel serve infer --mode http --backend http://localhost:11434`, set `local.endpoints[].agora_tunnel: infer` | a chat completion round-trips over the overlay |
| Serve: `agora tunnel create llm-gateway` (direct tcp-mode, gateway's account), grant the clients, set `agora.serve.enabled: true` / `serve.tunnel: llm-gateway` | the handler is reachable over Agora; `agora tunnel delete llm-gateway` tears it down |
| Catalog: serve config with `advertisement.workgroup_ids` set (inline or via integration file) | the `llm-gateway` advertisement appears in the catalog (without workgroup IDs, publishing is intentionally skipped) |
| Dual listener: zrok share and Agora serve both enabled | the zrok share token and the Agora tunnel both respond |
