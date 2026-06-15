# Upstream Cherry-Pick Tracking

This file tracks upstream commits that are cherry-picked, rewritten, partially
absorbed, deferred, or skipped on this repository's local integration branch.

Current local branch: `dev`

Last reviewed: 2026-06-14

## Status Legend

| Status | Meaning |
| --- | --- |
| `pending` | Should be absorbed, but no local rewrite has been committed yet. |
| `pending-partial` | Only part of the upstream patch should be absorbed. |
| `deferred` | Useful, but not urgent or too broad for the current batch. |
| `skipped` | Intentionally not absorbing as-is. |
| `superseded` | Local code already has a better or equivalent implementation. |
| `rewritten` | Logic has been absorbed into local commit(s) with different hashes. |
| `partial` | Only selected logic from the upstream commit has been absorbed. |

## airgate-openai

### Absorbed Rewrites

| Upstream commit | Local commit | Status | Notes |
| --- | --- | --- | --- |
| `8bdfcec` | `2b94857`, `25a97b3` | `rewritten` | Split into two local commits: disabling total timeout for true token streams, then adding first-byte and read-idle guards. |
| `8f90174` | `3298ef2` | `partial` | Only absorbed robust SSE parsing for OAuth usage probing. OAuth token validation and `assets.get_bytes` capability were intentionally left out. |
| `7a8ef85` | `4a1ee26` | `rewritten` | `/v1/messages` / Anthropic stream aborts now return partial usage so Core can record costs. |

### Pending Upstream Commits

| Upstream commit | Local commit | Status | Notes |
| --- | --- | --- | --- |
| `80cb87e` | - | `deferred` | Image generation usage cost reporting. Overlaps with local image pricing and sales-rate display changes. |
| `0813835` | - | `deferred` | Guard image usage cost after base pricing. Evaluate with the image billing group. |
| `5a880d9` | - | `deferred` | Image token cost unit normalization. Evaluate with the image billing group. |
| `8d7e0d2` | - | `deferred` | Count responses image outputs for billing. Evaluate with the image billing group. |
| `ec72e45` | - | `deferred` | Keep responses image generation output visible. Evaluate with image response behavior. |
| `2bfe9fa` | - | `deferred` | Normalize responses image generation output. Evaluate with image response behavior. |
| `8edf353` | - | `deferred` | Remove retired Codex models. Needs model compatibility review. |
| `7d4511c` | - | `pending` | Skip forced instructions for responses image generation. Small targeted image-generation correctness fix. |
| `a26ee17` | - | `pending` | Route OAuth image generation through WebSocket. Relevant if OAuth image generation is enabled. |
| `34bb77c` | - | `pending` | Route image-only OpenAI accounts. Targeted routing fix. |
| `6c27ab7` | - | `deferred` | Route metadata-only and model family metadata. Should be handled with Core metadata-declaration work. |
| `c3a57c2` | - | `deferred` | `/v1/messages*` declares `error_format=anthropic`. Should be handled with Core error-format metadata work. |
| `d038a8c` | - | `deferred` | Declare `scheduling_model_map` route metadata. Should be handled with Core scheduling metadata work. |
| `dd55422` | - | `pending` | Removes unused `splitSSELines` after partial `8f90174` absorption. Safe cleanup after confirming no remaining references. |

### Documentation / Tooling

| Upstream commit | Local commit | Status | Notes |
| --- | --- | --- | --- |
| `130a6ad` | - | `deferred` | Commit-msg hook and CLAUDE.md changes. Not needed for runtime reliability. |
| `51451a7` | - | `deferred` | CLAUDE.md documentation cleanup. |
