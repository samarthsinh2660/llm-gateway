---
title: per-key rate limiting
state: horizon
created: 2026-07-24
tags: [feature]
milestone: v0.1.x
---

Rate-limit requests per virtual API key, so a single key can't exhaust a backend on behalf of everyone. Shape (fixed window, token bucket, per-model vs per-key) is open.
