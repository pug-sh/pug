# OpenTelemetry Instrumentation

Detailed reference for telemetry bootstrapping and the error-recording convention (`internal/deps/telemetry`). Linked from the root [`CLAUDE.md`](../../CLAUDE.md). The root **Code Style** section carries the one-line "log + record at source" rule; the full exceptions list lives here.

All telemetry is bootstrapped in `internal/deps/telemetry/`. The server initializes OpenTelemetry via `telemetry.NewOtelInterceptor(ctx)` which:

- Sets up trace, metric, and log providers exporting OTLP over gRPC (insecure, default `localhost:4317`)
- Replaces the default `slog` logger with an OTel-bridged logger — all `slog.*Context` calls are automatically correlated with the active trace
- Returns an `otelconnect.Interceptor` that is wired into every Connect RPC handler

**Instrumentation status:**

| Component      | Status                                                                                                                   |
| -------------- | ------------------------------------------------------------------------------------------------------------------------ |
| Connect RPC    | ✅ — `otelconnect.Interceptor` on all handlers                                                                           |
| slog → OTel    | ✅ — `otelslog` bridge replaces default logger; with no OTLP endpoint configured, text logs to stdout instead                        |
| PostgreSQL     | ✅ — `otelpgx` tracer on all connections                                                                                 |
| Redis          | ✅ — `redisotel` tracing + metrics on the client                                                                         |
| NATS/JetStream | Custom — `tracedJetStream` wrapper in `internal/deps/nats/otel.go`, W3C trace context propagation on publish/consume     |
| ClickHouse     | Custom — `Conn` wrapper in `internal/deps/clickhouse/clickhouse.go`, spans on Query/Exec/Select/PrepareBatch/AsyncInsert/QueryFormat/InsertFormat                        |

**ClickHouse span lifetimes:** `Query`, `PrepareBatch` and `QueryFormat` return a handle the driver fills in later, so those three spans outlive the call that started them and end on `Rows.Close`, the first of `Batch.Send`/`Abort`/`Close`, and the stream's `Close` respectively. **A caller that abandons the handle loses the span entirely**, so `defer rows.Close()` and a guaranteed batch finalizer are required for tracing, not just for releasing the pooled connection. `Batch.Append`/`AppendStruct`/`Flush` record without ending, since an append failure is batch-fatal but the `Abort` that follows reports success.

`Conn` embeds `driver.Conn` rather than implementing it (the driver adds methods in minor releases and its doc comment says to embed), so a newly added method forwards **untraced** instead of breaking the build — worth checking on a version bump. `QueryFormat`/`InsertFormat` are HTTP-only: a native DSN gets `ErrFormatNativeUnsupported` before the pool is touched.

**Configuration:** Setting an OTLP endpoint is what selects OTLP export (see *Export modes* below) — `OTEL_EXPORTER_OTLP_ENDPOINT` (the conventional collector port is `4317`) or a per-signal `OTEL_EXPORTER_OTLP_{TRACES,METRICS,LOGS}_ENDPOINT`. Also set `OTEL_SERVICE_NAME` (strongly recommended — telemetry data will lack a service identifier without it). TLS is disabled by default (`OTEL_EXPORTER_OTLP_INSECURE` defaults to `true` when unset); set `OTEL_EXPORTER_OTLP_INSECURE=false` to enable TLS for production OTLP endpoints.

**Export modes (auto-detected):**

There is no `PUG_OTEL` switch. `SetupSDK` calls `resolveOtelMode()`, which returns `otlp` when `otlpConfigured()` finds an OTLP endpoint var set, otherwise `stdout`:

| Condition | Behavior |
| --------- | -------- |
| Any of `OTEL_EXPORTER_OTLP_ENDPOINT` / `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` / `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` / `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` set (non-blank) | OTLP export via `otelslog` (requires a collector) |
| None set | Noop trace/metric/log providers; application logs go to stdout as text via a slog handler (not the OTel log pipeline), no collector required |

The mode is resolved once per process on the first `SetupSDK` call — set the endpoint var(s) before starting the server or workers. A present-but-blank endpoint (e.g. `OTEL_EXPORTER_OTLP_ENDPOINT=`) counts as unset, so a conditionally-templated empty value can't silently flip pug into exporting at a collector that isn't there. For local dev with only `make infra`, leave the endpoint unset (stdout); run `make clickstack` and set `OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4317` when exporting to HyperDX.

**Recording errors in spans:** Use `telemetry.RecordError(ctx, err)` to record an error on the current span, set the span status to `Error`, and attach stack traces.

Pair `slog.ErrorContext` with `telemetry.RecordError` **at the layer that detects the error** — typically the executor, service, worker, or query helper where the error first surfaces. Downstream layers (handlers, wrappers) must NOT re-log or re-record the same error: `slog.ErrorContext` would emit a duplicate log line, and `telemetry.RecordError` would attach a duplicate event to the same span. Handlers that propagate an already-recorded error should only translate it to the appropriate `connect.NewError(...)` and return.

The `recorderr` analyzer in `internal/lint` gates one direction of this: an `slog.ErrorContext` call carrying an error must have a `telemetry.RecordError` (or `RecordErrorOnSpan`) on the same path, or a `// puglint:exempt` marker naming why — by convention, since the marker is matched by substring and the reason is not enforced. It is scoped out of `package main` and `func init()`, which run outside any span, and it ignores ERROR-level logs that carry no error at all — so dropping the error from a log is also a way to silence it, which is worse than the violation. The other direction — a downstream layer *re-*logging or *re-*recording an error the source already handled — is still code review only.

Exceptions:

- **Client-input errors** (`CodeInvalidArgument`, `CodeUnauthenticated`, etc.) do not need `RecordError`. The default treatment is `slog.WarnContext` at the boundary that detects them, but log level and location vary by case:
  - **Auth extraction failures** (`MustGetPrincipal*`) — log at `slog.DebugContext` at the source (`internal/app/server/rpc/auth.go`). Auth-extraction is high-volume probe noise (every unauthenticated request hits it), so Debug keeps the noise floor low. The handler boundary skips the log entirely and only translates to `connect.NewError(connect.CodeUnauthenticated, ...)`.
  - **`Build*Query` validation errors with client-supplied free-form input** (`BuildTrendsQuery`, `BuildSegmentationQuery`, `BuildFunnelTimingQuery`, `BuildFunnelCountsQuery`, `BuildRetentionQuery`, `BuildSegmentUsersQuery`) — log at `slog.WarnContext` at the boundary. Other `Build*Query` callers in `internal/core/insights/service.go` (filter-schema and property-values builders) take only `projectID` plus a validated `eventKind`/`propertyKey`; their `Build()` failures are programmer-error / proto-enum drift, not client input, so they log + record at source like internal errors.
  - **Other client-input validators** vary based on whether the failure carries diagnostic value:
    - `events.ErrInvalidFilter` — log at `slog.WarnContext` at the boundary (carries which property/operator the client got wrong).
    - `coreevents.ValidateExternalEvents`, `events.DecodeEventCursor` — no log at all at the boundary; the handler just translates to `CodeInvalidArgument`. The rejection itself is the diagnostic (malformed page tokens, batch-dedup mismatches), and the request body is already in the access log.
- **Defer-rollback / cleanup failures** (e.g. `tx.Rollback`, `rows.Close`) should pair slog + RecordError at the deferred site since no caller can see them.
- **Wrapper disposition logs.** A wrapper that emits its own log for a wrapper-specific decision (e.g. the NATS worker's "terminating poison message" / "message processing failed" lines) MAY include the underlying processor error as a `slogx.Error(err)` attribute. That log line is a *different fact* (the disposition the wrapper decided on, plus wrapper-only metadata like stream/consumer) than the processor's source log, so it is not a duplicate. The wrapper must still skip `telemetry.RecordError` on the original error — the processor already recorded it.
- **Already recorded downstream.** A log whose error was recorded on a child span by an instrumented client (`chdb.Conn.Query`, `otelpgx`'s batch/query spans) adds context — the window, the failing cell — without re-recording. These carry `// puglint:exempt`. otelpgx bails unless a recording ancestor span exists; chdb starts a root span either way.
- **The unclassified-error backstop.** `sanitizeError`'s final branch (`internal/app/server/rpc/error.go`) logs *and* records, which looks like a wrapper re-recording but is not: it runs only for an error that is neither an `*apperr.Error` nor a `*connect.Error` nor a context error, so no layer claimed it and none recorded it. otelconnect sets span status but never attaches an exception event, so without the record the span carries no cause at all. A duplicate here is cheaper than a blind spot on the one path that exists because the convention was not followed.
- **Pure-passthrough services.** When a service method is a one-line wrapper around a generated `dbread`/`dbwrite` query (no business logic, no enrichment to add), the *handler* is effectively the lowest layer with meaningful context (project_id, customer_id, etc.) — logging the DB error at the handler is acceptable in that case. Services with non-trivial logic (e.g. transactions, orchestration of multiple writes, cross-cutting validation) must log + record at source like everyone else.
