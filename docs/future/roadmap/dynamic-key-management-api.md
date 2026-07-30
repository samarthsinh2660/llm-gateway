---
title: dynamic key management api
state: horizon
created: 2026-07-24
tags: [feature, spike]
milestone: v0.1.x
---

Manage virtual API keys at runtime through an admin API... create, revoke, and list keys without editing the config file and restarting.

## Discussion

Runtime management couples to hashed storage and expiry: a key minted at runtime needs the same storage shape and lifecycle as a configured one, so those land first or alongside. The unknown the spike is for is the admin surface's own auth boundary — who may manage keys and how they authenticate — which must be distinct from the data-plane `sk-gw-*` keys it manages.
