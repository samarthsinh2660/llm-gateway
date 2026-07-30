---
title: key storage
state: researching
created: 2026-07-24
tags: [enhancement, spike]
milestone: v0.1.x
---

Move virtual API keys out of the plaintext config into a dedicated key store, held as hashes rather than recoverable secrets.

## Discussion

Two aspects of one goal: a dedicated store decouples keys from the main config, and hashing (e.g. SHA-256, incoming tokens hashed before lookup) means that store holds no recoverable secret. The hashing tradeoff stands — a key can't be read back once stored. The spike is the backend: a YAML file, a SQLite database, or a pluggable strategy the gateway adapts to other data sources; the choice and the abstraction boundary are the open question. A mutable store is also what dynamic-key-management needs — runtime create/revoke/list has nowhere to write against a static config.