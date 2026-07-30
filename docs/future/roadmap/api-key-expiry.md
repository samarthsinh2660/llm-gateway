---
title: api key expiry
state: horizon
created: 2026-07-24
tags: [feature]
milestone: v0.1.x
---

Add an optional `expires_at` to a virtual API key so it stops authenticating past a set time. Expired keys are rejected at the auth middleware like an unknown key.
