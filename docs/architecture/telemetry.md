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
| ClickHouse     | Custom — `Conn` wrapper in `internal/deps/clickhouse/clickhouse.go`, spans on Query/Exec/Select/PrepareBatch/AsyncInsert |

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

This convention is enforced by code review, not by tests — no slog/span assertions exist in CI, so a regression that re-introduces duplicate logging or drops source-layer instrumentation will not fail the build.

Exceptions:

- **Client-input errors** (`CodeInvalidArgument`, `CodeUnauthenticated`, etc.) do not need `RecordError`. The default treatment is `slog.WarnContext` at the boundary that detects them, but log level and location vary by case:
  - **Auth extraction failures** (`MustGetPrincipal*`) — log at `slog.DebugContext` at the source (`internal/app/server/rpc/auth.go`). Auth-extraction is high-volume probe noise (every unauthenticated request hits it), so Debug keeps the noise floor low. The handler boundary skips the log entirely and only translates to `connect.NewError(connect.CodeUnauthenticated, ...)`.
  - **`Build*Query` validation errors with client-supplied free-form input** (`BuildTrendsQuery`, `BuildSegmentationQuery`, `BuildFunnelTimingQuery`, `BuildFunnelCountsQuery`, `BuildRetentionQuery`, `BuildSegmentUsersQuery`) — log at `slog.WarnContext` at the boundary. Other `Build*Query` callers in `internal/core/insights/service.go` (filter-schema and property-values builders) take only `projectID` plus a validated `eventKind`/`propertyKey`; their `Build()` failures are programmer-error / proto-enum drift, not client input, so they log + record at source like internal errors.
  - **Other client-input validators** vary based on whether the failure carries diagnostic value:
    - `events.ErrInvalidFilter` — log at `slog.WarnContext` at the boundary (carries which property/operator the client got wrong).
    - `coreevents.ValidateExternalEvents`, `events.DecodeEventCursor` — no log at all at the boundary; the handler just translates to `CodeInvalidArgument`. The rejection itself is the diagnostic (malformed page tokens, batch-dedup mismatches), and the request body is already in the access log.
- **Defer-rollback / cleanup failures** (e.g. `tx.Rollback`, `rows.Close`) should pair slog + RecordError at the deferred site since no caller can see them.
- **Wrapper disposition logs.** A wrapper that emits its own log for a wrapper-specific decision (e.g. the NATS worker's "terminating poison message" / "message processing failed" lines) MAY include the underlying processor error as a `slogx.Error(err)` attribute. That log line is a *different fact* (the disposition the wrapper decided on, plus wrapper-only metadata like stream/consumer) than the processor's source log, so it is not a duplicate. The wrapper must still skip `telemetry.RecordError` on the original error — the processor already recorded it.
- **Pure-passthrough services.** When a service method is a one-line wrapper around a generated `dbread`/`dbwrite` query (no business logic, no enrichment to add), the *handler* is effectively the lowest layer with meaningful context (project_id, customer_id, etc.) — logging the DB error at the handler is acceptable in that case. Services with non-trivial logic (e.g. transactions, orchestration of multiple writes, cross-cutting validation) must log + record at source like everyone else.
