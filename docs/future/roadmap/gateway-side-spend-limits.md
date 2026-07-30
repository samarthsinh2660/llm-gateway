---
title: gateway-side spend limits
state: horizon
created: 2026-07-24
tags: [feature]
milestone: v0.1.x
---

Enforce a per-key spend ceiling at the gateway, so an over-budget key is capped here rather than by each client harness policing its own spend.

## Discussion

Builds on per-key-metrics: the per-key token measurement, including the streaming path, is the prerequisite — that card is the observability half, this the enforcement half. A dollar ceiling additionally needs per-model prices the gateway doesn't carry today, so a token-count ceiling (per key, per window) is the simpler first cut. Enforcing at the gateway gives one locus instead of every client policing its own spend; the request path already lets the enforcement point migrate without a client-facing contract change.
