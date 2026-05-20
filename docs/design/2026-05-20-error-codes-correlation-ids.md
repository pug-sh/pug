# Design: Error Codes + Correlation IDs

- **Status:** Draft (approved in brainstorming, pending spec review)
- **Date:** 2026-05-20
- **Branch:** `feat/error-ids`

## Problem

When an RPC fails, the client receives a deliberately sanitized message (e.g.
`failed to list profiles`) while the real error is logged + recorded server-side.
That is correct for confidentiality, but it leaves **no shared key** between what
the client/user saw and the log/trace line that explains it. Triaging a reported
bug means guessing from a timestamp and a generic message.

Separately, there is no stable, documentable identifier for an error *class* —
useful for an open-source project where users self-diagnose, file issues, and
branch on errors programmatically.

## Goals

1. Every client-facing error carries a **stable code** (`reason`) — what *kind* of
   error it is — and a **per-request correlation id** (`error_id`) — *which*
   occurrence.
2. A reported `error_id` maps directly to the exact server log/trace line.
3. Codes are stable, searchable, and documentable for OSS consumers.
4. Preserve the existing rules: sanitize client messages, never leak internal
   error strings, log at source.
5. Works with OpenTelemetry **disabled** (the `.70` test deploy runs OTel off).

## Non-goals (YAGNI)

- No header/trailer transport — codes/ids ride in the typed Connect error detail only.
- No per-*error* ids — one id per request (a failed unary/stream returns exactly one).
- No proto `enum` for codes — string constants in a Go registry (avoids per-code regen).
- No i18n / message translation in this iteration.
- No big-bang migration of all ~292 `connect.NewError` sites — central baseline + incremental opt-in.

## Background (current state)

- `internal/app/server/rpc/error.go` — `ErrorInterceptor` / `sanitizeError` already
  funnels **every** outgoing error. `*connect.Error` is passed through; leaked
  non-connect errors are logged as "unhandled error" and replaced with a generic
  internal error. **This is the single seam we extend.**
- Interceptor chain (`internal/app/server/server.go`): otel → logging → validate →
  error → principal. We add a correlation interceptor at the front.
- slog is bridged to OTel in `internal/deps/telemetry`; we wrap the handler to stamp
  per-request attributes.
- `github.com/rs/xid` is already a dependency — compact, k-sortable, URL-safe ids.
  No new dependency required.
- ~292 `connect.NewError` sites with free-form sanitized messages; no code catalog exists.

## Wire contract

New `proto/common/v1/error.proto` (per the `common/v1` cross-boundary convention),
attached as a Connect error detail. Modeled on `google.rpc.ErrorInfo` (AIP-193):

```proto
syntax = "proto3";
package common.v1;

// ErrorInfo is attached as a Connect error detail on every RPC error.
message ErrorInfo {
  // Stable, machine-readable code, UPPER_SNAKE_CASE, e.g. "PROFILE_NOT_FOUND".
  // Immutable once shipped: never rename, only add. Generic fallbacks mirror the
  // Connect code (INTERNAL, NOT_FOUND, INVALID_ARGUMENT, ...).
  string reason = 1;
  // Namespace for the reason (e.g. "pug.sh"); keeps reasons short and groupable.
  string domain = 2;
  // Per-request correlation id (xid). Always present, even with OTel disabled.
  string error_id = 3;
  // OTel trace id when a valid span is active; "" otherwise.
  string trace_id = 4;
}
```

On the wire (Connect JSON):

```json
{
  "code": "internal",
  "message": "failed to list profiles",
  "details": [{
    "type": "common.v1.ErrorInfo",
    "value": { "reason": "INTERNAL", "domain": "pug.sh",
               "error_id": "cv9k2m...", "trace_id": "" }
  }]
}
```

## Components

### 1. CorrelationInterceptor (new)

`internal/app/server/rpc/correlation_interceptor.go`. Registered **first** in the
chain. Mints one `xid` per request and stores it in context
(`ctx = withErrorID(ctx, xid.New().String())`). Unary + streaming. Independent of
OTel, so the id always exists.

### 2. slog attribute handler (wrap existing bridge)

In `internal/deps/telemetry`, wrap the slog handler so every record pulls the
`error_id` from context (and the active span's `trace_id` when valid) and adds them
as attributes. Effect: all ~292 existing `slog.*Context` sites correlate with **zero
edits**.

### 3. sanitizeError becomes the single attach point (extend existing)

For every outgoing error, `sanitizeError` attaches exactly one `ErrorInfo`:

| Incoming error | reason | logging |
| --- | --- | --- |
| `*apperr.Error` (helper, carries a tagged reason) | the tagged reason | none here (logged at source) |
| plain `*connect.Error` | generic fallback mapped from `connect.Code` | none here |
| `ctx.Err()` (cancel/deadline) | passthrough, no detail | none |
| leaked non-connect error | `INTERNAL` | log "unhandled error" (existing behavior) |

`error_id`, `trace_id`, and `domain` are always sourced **in the interceptor** (id/trace
from context, domain from a constant) — one place, so the response id always matches the
logged id.

### 4. apperr helper + code registry (new)

`internal/apperr`:

- `apperr.Err(code connect.Code, reason, msg string) error` — returns an
  `*apperr.Error` carrying `{code, reason, msg}`. The interceptor turns it into a
  `*connect.Error` and attaches the `ErrorInfo`. Logging stays explicit at the source
  (unchanged convention). Handlers that keep calling plain `connect.NewError` still get
  an id + a generic reason — no forced migration.
- `internal/apperr/codes` — UPPER_SNAKE string constants, one per error class
  (`ProfileNotFound = "PROFILE_NOT_FOUND"`). Generic fallbacks (`INTERNAL`,
  `UNAUTHENTICATED`, `INVALID_ARGUMENT`, `NOT_FOUND`, `PERMISSION_DENIED`, …) mirror the
  Connect codes.
- A registry test enforces **uniqueness** and the format `^[A-Z][A-Z0-9_]+$`.

## Correlation ID

`rs/xid` — 20-char, k-sortable, URL-safe, collision-resistant, no new dep. Chosen over
a raw OTel trace id because it must exist even when OTel export is off (`.70`). When a
valid span *is* active, its `trace_id` is included as a bonus field for jumping straight
to a trace.

## Code catalog rules

- **Casing/format:** UPPER_SNAKE, `^[A-Z][A-Z0-9_]+$`. Matches gRPC canonical codes and
  `google.rpc.ErrorInfo.reason`.
- **Namespacing:** via the `domain` field, not dots in the string.
- **Stability:** immutable once shipped — never rename, only add (treat like enum value
  names). Gives numeric-style stability without the opacity.
- **Granularity / leakage:** a reason names **client-facing meaning, never internal
  cause**. 500s stay generic (`INTERNAL`). This keeps the SDK/untrusted boundary safe and
  honors the "don't leak internals" rule — the `error_id` carries the debugging power, the
  reason carries only intentional, public semantics.

## Data flow

1. Request enters → CorrelationInterceptor mints `xid` into ctx.
2. Any `slog.*Context` in handler/service/worker auto-gets `error_id` (+ `trace_id`).
3. Handler returns `apperr.Err(...)`, a plain `connect.NewError`, or a leaked error.
4. `sanitizeError` attaches the `ErrorInfo` (reason from helper-or-fallback; id/trace from ctx).
5. Client gets `{code, message, details:[ErrorInfo]}`. User reports `error_id` →
   `grep error_id=<id>` → the exact source log line for that request.

## Security / confidentiality

No contradiction with "don't show actual errors to client":
- `error_id` is a random pointer — reveals nothing; the real error stays in logs.
- `reason` is a curated, intentional public identifier — not a raw internal string.
- 500s expose only the generic `INTERNAL` reason. Specific reasons exist only where the
  meaning is safe to expose to the (possibly untrusted) caller.

## Rollout

1. **Mechanism + free baseline:** proto, CorrelationInterceptor, slog wrap, extended
   `sanitizeError`, `apperr` package, registry + test. Every error now gets an id + a
   generic reason with no per-site changes.
2. **Reference adoption:** tag specific reasons in one service (profiles or orgs) as the
   worked example for contributors.
3. **Incremental:** add reasons elsewhere as touched. Optionally generate `ERRORS.md`
   from the registry for OSS docs.

## Testing

- Unit: CorrelationInterceptor sets an id; `sanitizeError` attaches `ErrorInfo` with the
  ctx id even when OTel is off; `apperr.Err` carries the reason; generic fallback mapping
  per Connect code; ctx-cancel path attaches no detail.
- Registry: uniqueness + format test over all constants.
- Integration: an erroring RPC round-trips the `ErrorInfo` detail (reason + id) through a
  real Connect client.

## Decisions (from brainstorming)

| Decision | Choice | Why |
| --- | --- | --- |
| Flavor | Code **and** correlation id | Code = what kind; id = which occurrence (Stripe-style) |
| Transport | Connect error **details** only | Idiomatic typed contract for the proto-first stack |
| ID model | **Per-request** xid in ctx + slog | Zero threading; auto-correlates all logs; OTel-independent |
| Code scheme | **UPPER_SNAKE** `reason` + `domain` | gRPC / `google.rpc.ErrorInfo` standard; self-documenting |
| Message type | Custom `common.v1.ErrorInfo` modeled on `google.rpc.ErrorInfo` | Standard shape, minimal machinery (no googleapis dep) |
| ID library | `rs/xid` | Already a dependency; sortable, compact, URL-safe |

## Open questions / future

- Optional ergonomic variant `apperr.Internal(ctx, reason, msg, err)` that logs + records +
  returns in one call (still at-source).
- Generate `ERRORS.md` from the registry.
- Optional `metadata map<string,string>` on `ErrorInfo` (e.g. offending field for validation).
- Whether worker-side failures (NATS) should mint/log an id too (no client to return it to,
  but useful for log correlation).
- Possible later move to the real `google.rpc.ErrorInfo` + `google.rpc.RequestInfo` if
  full ecosystem interop becomes valuable.
