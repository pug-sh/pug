# User Flow Insight — Implementation Plan

Revised plan incorporating all review findings. Supersedes earlier drafts of this document.
Linked from [`CLAUDE.md`](../../CLAUDE.md) and [`insights.md`](insights.md).

> **Implementation status (shipped).** This document is the original plan; the
> feature is implemented. Where the shipped code diverges from the pseudo-code
> below, the code is authoritative. Notable divergences:
> - **`UserFlowNode` carries `is_others` (bool), not `label`.** The redundant
>   `label` (always equal to `id`) was dropped; the synthetic overflow bucket is
>   identified structurally by `is_others`.
> - **The overflow bucket is a distinct internal identity, not just the string
>   `"$others"`.** `GroupUserFlowResult` keys nodes by an internal `nodeRef{id,
>   others}` so a real node literally named `"$others"` never merges with the
>   bucket; the bucket's emitted id is disambiguated if a real node already uses
>   `"$others"`. See §4.3.
> - **Default resolution lives in `resolveUserFlowParams`** (called at the top of
>   `BuildUserFlowQuery`), which returns a `userFlowResolved`; `buildUserFlowQuery`
>   takes that resolved struct. The session grouping helpers are parameterless and
>   session-specific (`userFlowSessionGroupKeyColumn`, `userFlowNonEmptySessionKeyCond`)
>   since only `GROUP_BY_SESSION` exists today.

---

## 1. Overview

User flow is a **graph insight**: given a time window, how do sessions or users move between named states? The result is explicit **nodes + links** ready for Sankey rendering.

### Contrast with funnel

| | Funnel | User flow |
| --- | --- | --- |
| Input | Ordered `events[]` steps (predefined by caller) | Discovered transitions (emergent from data) |
| Output | `FunnelStep[]` per series | `UserFlowNode[]` + `UserFlowLink[]` |
| Paths | Single chain | Many-to-many graph |
| Weight | Stage conversion count | Unique sessions/users per edge |
| Chart | Bar / table / KPI | Sankey |

Do **not** derive Sankey output from `FunnelResult` — different data model, wrong semantics.

### Edge weight semantics (explicit)

`UserFlowLink.value` = count of **distinct sessions or users** that traversed the source→target edge at least once in the time window. A single session may traverse the same edge multiple times; it counts once. This is the natural weight for Sankey — volume moving through an edge, not path frequency.

---

## 2. Resolved design decisions

Every item that was ambiguous or incorrect in the draft, resolved here with rationale.

| Issue | Decision | Rationale |
| --- | --- | --- |
| `GROUP_BY_UNSPECIFIED` fallback | Only `GROUP_BY_SESSION` exists; the builder always groups by session_id via the parameterless `userFlowSessionGroupKeyColumn` (shipped: no `defaultUserFlowGroupBy` constant) | A future `GROUP_BY_USER` must add its own grouping column, surfaced by the session-specific helper name |
| `$others` self-loop after remap | Degenerate link removal runs **after** `$others` remapping | Remapping two low-volume nodes both to `$others` produces a `$others→$others` self-loop; only post-remap removal catches it |
| `max_hops` = nodes or edges? | **Edges**. A `max_hops = 5` flow touches at most 6 nodes. Documented in proto comment. | Off-by-one here is consistent across all sessions; must be unambiguous |
| `scope` filter semantics | Filters **which events are eligible to become nodes** (event-level), not which sessions are included (session-level). Sessions with zero eligible events emit no transitions and produce no rows. | The two interpretations give very different graphs; event-level is more useful |
| Zero-value links | `gte = 1` on `UserFlowLink.value`; `GroupUserFlowResult` drops zero-value links before building response | Zero-value edges are noise and waste response budget |
| PR 2 + PR 3 split | Merged into single PR — builder + executor + grouping + unit tests ship together | Original split left `ExecuteQuery` calling a not-yet-merged `GroupUserFlowResult`; broken integration |
| `groupArray(N)` before sort | Sort full `groupArray(...)`, then `arraySlice` to `max_hops + 1` | Limited `groupArray(max_size)` before sort retains arbitrary rows, not earliest by time — see §4.2, §14.6 |
| `GroupUserFlowResult` error return | **Dropped.** Function is pure Go with no I/O; it has no concrete failure mode. Returns `*UserFlowResult` only — always non-nil. | A signature that cannot actually error should not advertise one; removes double-logging ambiguity in dispatch |
| Rollup fast path | **No rollup in v1.** Always raw `events` via `BuildUserFlowQuery`. Do not wire `canUseEventRollup` or `canUseSessionRollup`. | Existing MVs store wrong grain (day×kind×dim counts, session metric states); user flow needs ordered paths → transition pairs. See §15. |

---

## 3. Proto contract

**File:** `proto/shared/insights/v1/insights.proto`

### 3.1 Insight type enum

Add at next free ordinal; do not renumber:

```protobuf
enum InsightType {
  INSIGHT_TYPE_UNSPECIFIED  = 0;
  INSIGHT_TYPE_TRENDS       = 1;
  INSIGHT_TYPE_SEGMENTATION = 2;
  INSIGHT_TYPE_FUNNEL       = 3;
  INSIGHT_TYPE_RETENTION    = 4;
  INSIGHT_TYPE_USER_FLOW    = 5;
}
```

### 3.2 `UserFlowQuery` message

```protobuf
message UserFlowQuery {
  // Which attribute names the node.
  enum NodeKind {
    NODE_KIND_UNSPECIFIED = 0;
    NODE_KIND_EVENT_KIND  = 1;  // events.kind
    NODE_KIND_PROPERTY    = 2;  // arbitrary event property value
  }
  NodeKind node_kind = 1;

  // Required when node_kind == NODE_KIND_PROPERTY.
  // Accepts bare name ("page") or $ prefix ("$url").
  // Validated pattern: ^\$?[a-zA-Z0-9_.-]+$
  string node_property = 2;

  // Traversal unit. Only GROUP_BY_SESSION is implemented; the builder always
  // groups by session_id (see userFlowSessionGroupKeyColumn). A future
  // GROUP_BY_USER must add its own grouping column.
  enum GroupBy {
    GROUP_BY_UNSPECIFIED = 0;
    GROUP_BY_SESSION     = 1;  // session_id — v1 default
  }
  GroupBy group_by = 3;

  // Event-level filter: restricts which events are eligible to become nodes.
  // This is NOT a session filter — sessions are not excluded based on scope.
  // Sessions whose eligible events produce no consecutive pairs emit no transitions.
  common.v1.EventFilter scope = 4;

  // Maximum transitions (edges) to collect per session/user.
  // A max_hops = N flow touches at most N+1 nodes.
  // Default 5 in builder. CEL enforced max: 10.
  int32 max_hops  = 5;

  // Top-N nodes to retain before $others collapse.
  // Default 20. CEL enforced max: 50. CEL enforced min (when set): 2.
  int32 max_nodes = 6;

  // Maximum links in response (sorted by value desc after $others collapse).
  // Default 100. CEL enforced max: 500.
  int32 max_links = 7;
}
```

Add to `InsightQuerySpec`:

```protobuf
UserFlowQuery user_flow = 10;  // field 10; session = 9
```

### 3.3 Response messages

```protobuf
message UserFlowNode {
  string id        = 1;  // stable key; used as source/target in links
  // is_others marks the synthetic overflow bucket (the node pruned-away nodes
  // collapse into). Real nodes have is_others=false. Clients identify the bucket
  // by this flag, NOT by matching id against "$others" — a real event kind or
  // property value can legitimately be the literal "$others".
  bool   is_others = 2;
}

message UserFlowLink {
  string source = 1;
  string target = 2;
  // Distinct sessions/users crossing this edge. Always >= 1 in a valid response.
  int64  value  = 3 [(buf.validate.field).int64.gte = 1];
}

message UserFlowResult {
  repeated UserFlowNode nodes = 1;
  repeated UserFlowLink links = 2;
}
```

Add to `QueryResponse.result` oneof:

```protobuf
UserFlowResult user_flow = 6;
```

### 3.4 CEL validation rules

All rules added to `InsightQuerySpec`. Update any existing rules that assume all non-session types use `events`.

| Rule ID | Intent |
| --- | --- |
| `user_flow_only_for_user_flow` | `user_flow` set → `insight_type == USER_FLOW` |
| `user_flow_required` | `insight_type == USER_FLOW` → `has(user_flow)` |
| `user_flow_no_events` | `USER_FLOW` → `events.size() == 0` |
| `user_flow_no_session` | `USER_FLOW` → `!has(session)` |
| `user_flow_no_breakdowns` | `USER_FLOW` → `breakdowns.size() == 0` |
| `user_flow_no_breakdown_limit` | `USER_FLOW` → `breakdown_limit == 0` |
| `user_flow_property_required` | `node_kind == PROPERTY` → `node_property.size() > 0` and matches `^\$?[a-zA-Z0-9_.-]+$` |
| `user_flow_max_hops_range` | `max_hops != 0` → `max_hops >= 1 && max_hops <= 10` |
| `user_flow_max_nodes_range` | `max_nodes != 0` → `max_nodes >= 2 && max_nodes <= 50` |
| `user_flow_max_links_range` | `max_links != 0` → `max_links >= 1 && max_links <= 500` |

`max_nodes` minimum of 2: `$others` needs room alongside at least one real node. Enforced via CEL only — no redundant Go check.

**Granularity:** `USER_FLOW` ignores `QueryRequest.granularity` at query-build time (same pattern as funnel/segmentation). `QueryRequest` still requires an explicit granularity field — dashboard per-granularity time-range caps apply when `renderInsightTile` re-validates.

### 3.5 Dashboard view mode

**File:** `proto/dashboard/dashboards/v1/dashboards.proto`

```protobuf
DASHBOARD_TILE_VIEW_MODE_SANKEY = 7;
```

Client rendering hint only. Server stores and echoes; no server-side enforcement of insight ↔ view_mode pairing.

**After changes:** `make rpc && make lint-proto`

---

## 4. Builder

**Files:** `internal/core/insights/builder.go`, `internal/core/insights/user_flow.go`

Extend the builder table in [`insights.md`](insights.md):

| Insight type | Builder | Query type |
| --- | --- | --- |
| User flow | `BuildUserFlowQuery` | `UserFlowQuery` |

`UserFlowQuery` exposes `.SQL()`, `.Args()`, `.MaxNodes()`, `.MaxLinks()` (same pattern as `FunnelQuery`).

**Query construction policy:** assemble all SQL via `internal/core/clickhouse` (`chq.NewQuery`, `With`, `Select` / `SelectExpr`, `From`, `Where`, `GroupBy`, `OrderBy`, `WithQueryCache`). Do **not** hand-build full query strings. This matches funnel, retention, and session builders — see [`clickhouse.md`](clickhouse.md).

Allowed exceptions (same as existing insight builders):

- **Complex SELECT expressions** (array functions) composed with `fmt.Sprintf` and passed to `SelectExpr` — e.g. `windowFunnel(...)` in funnel counts, `groupArray(...)` in funnel timing.
- **Trusted literals only** in those expressions: column names (`kind`, `session_id`), `chq.PropertyExpr(...)` output (property key validated by proto), and integers derived from validated request fields (`max_hops + 1`).
- **`project_id`, time bounds, and filter values** always via `chq.Eq` / `chq.Gte` / `chq.Lt` / `chq.RawCond` args — never interpolated into SQL strings.

`BuildUserFlowQuery` returns `(UserFlowQuery, error)` by calling an internal `buildUserFlowQuery(...) (*chq.Query, error)`, then:

```go
sql, args, err := q.WithQueryCache(analyticsCacheTTL).Build()
```

### 4.1 Constants

```go
const (
    defaultUserFlowMaxHops  = 5
    defaultUserFlowMaxNodes = 20
    defaultUserFlowMaxLinks = 100

    // defaultUserFlowNodeKind is the resolved value when NodeKind is UNSPECIFIED.
    // Not a silent fallback — resolved explicitly in resolveUserFlowParams, not via
    // a switch fallthrough.
    defaultUserFlowNodeKind = insightsv1.UserFlowQuery_NODE_KIND_EVENT_KIND
)
```

(Shipped: `group_by` has only `GROUP_BY_SESSION`, which has no effect on output, so the
builder no longer resolves it — there is no `defaultUserFlowGroupBy` constant.)

### 4.2 Query structure (two CTEs + outer SELECT)

Logical shape (what `Build()` emits):

1. **`session_nodes` CTE** — per `group_key`, ordered node array (sort-then-slice, max `max_hops + 1` nodes).
2. **`pairs` CTE** — explode consecutive `(source, target)` via `ARRAY JOIN arrayEnumerate(nodes)`.
3. **Outer SELECT** — filter degenerate pairs, `count(DISTINCT group_key)`, `GROUP BY source, target`.

Implementation uses three `*chq.Query` values composed with `With`:

```go
func buildUserFlowQuery(req *insightsv1.QueryRequest, projectID string) (*chq.Query, error) {
    // Resolve defaults (group_by, node_kind, max_hops, …) at top — see §4.1, §14.3.
    groupKeyCol := userFlowGroupKeyColumn(groupBy)       // always "session_id" in v1
    nodeExpr := userFlowNodeExpr(nodeKind, nodeProperty) // "kind" | chq.PropertyExpr(...)

    topLevelFilterCond, err := buildTopLevelFilterCondition(
        req.GetSpec().GetFilterGroups(),
        req.GetSpec().GetFilterGroupsOperator(),
        projectID, "",
    )
    if err != nil {
        return nil, fmt.Errorf("user flow: %w", err)
    }
    scopeCond, err := userFlowScopeCondition(req.GetSpec().GetUserFlow(), projectID)
    if err != nil {
        return nil, fmt.Errorf("user flow: %w", err)
    }

    from := req.GetTimeRange().GetFrom().AsTime()
    to := req.GetTimeRange().GetTo().AsTime()
    maxHopsP1 := resolvedMaxHops + 1

    // CTE 1: session_nodes
    sessionNodesCTE := chq.NewQuery().
        Select(groupKeyCol + " AS group_key").
        SelectExpr(userFlowNodesArrayExpr(nodeExpr, maxHopsP1) + " AS nodes").
        From("events").
        Where(
            chq.Eq("project_id", projectID),
            chq.Gte("occur_time", from),
            chq.Lt("occur_time", to),
            scopeCond,
            topLevelFilterCond,
            userFlowNonEmptyGroupKeyCond(groupBy),
        ).
        GroupBy(groupKeyCol)

    // CTE 2: pairs (reads session_nodes from outer WITH)
    pairsCTE := chq.NewQuery().
        Select(
            "group_key",
            "nodes[idx] AS source",
            "nodes[idx + 1] AS target",
        ).
        From("session_nodes ARRAY JOIN arrayEnumerate(nodes) AS idx").
        Where(chq.RawCond("idx < length(nodes)"))

    // Outer query
    return chq.NewQuery().
        With("session_nodes", sessionNodesCTE).
        With("pairs", pairsCTE).
        Select("source", "target", "count(DISTINCT group_key) AS value").
        From("pairs").
        Where(
            chq.Neq("source", ""),
            chq.Neq("target", ""),
            chq.RawCond("source != target"),
        ).
        GroupBy("source", "target").
        OrderBy("value DESC"), nil
}
```

**`userFlowNodesArrayExpr`** — pure string helper for the array pipeline (called from `SelectExpr`):

```go
// userFlowNodesArrayExpr returns the arraySlice/arraySort/groupArray expression.
// maxHopsP1 is max_hops+1 (edges → nodes). arraySlice offset is 1-indexed.
// Collect all (occur_time, node) tuples, sort by time, slice to first maxHopsP1.
// Do NOT use groupArray(N)(...) before sorting — size-limited groupArray retains
// arbitrary rows, not earliest by time.
func userFlowNodesArrayExpr(nodeExpr string, maxHopsP1 int) string {
    return fmt.Sprintf(
        "arraySlice(arrayMap(x -> x.2, arraySort(x -> x.1, groupArray((occur_time, %s)))), 1, %d)",
        nodeExpr, maxHopsP1,
    )
}
```

**`userFlowGroupKeyColumn` / `userFlowNodeExpr`:**

| Helper | v1 |
| --- | --- |
| `userFlowGroupKeyColumn` | `"session_id"` |
| `userFlowNodeExpr` (EVENT_KIND) | `"kind"` |
| `userFlowNodeExpr` (PROPERTY) | `chq.PropertyExpr(key)` |

**`userFlowNonEmptyGroupKeyCond`** — exclude empty keys (§14.4):

```go
// session_id is UUID; distinct_id is String (migration 001).
func userFlowNonEmptyGroupKeyCond(groupBy insightsv1.UserFlowQuery_GroupBy) chq.Condition {
    switch groupBy {
    case insightsv1.UserFlowQuery_GROUP_BY_SESSION:
        return chq.Neq("session_id", "00000000-0000-0000-0000-000000000000")
    default:
        return chq.Neq("session_id", "00000000-0000-0000-0000-000000000000")
    }
}
```

`ARRAY JOIN arrayEnumerate` on the sorted array produces consecutive `(idx, idx+1)` pairs without a self-join. Self-loops and empty labels are dropped in the outer `Where`.

**No `FINAL`:** ReplacingMergeTree duplicate delivery may slightly inflate values. Document in [`insights.md`](insights.md) accuracy notes.

### 4.3 Scope and filter wiring

`scope` is an event-level filter on the `session_nodes` CTE `Where` clause. Implementation:

```go
func userFlowScopeCondition(uf *insightsv1.UserFlowQuery, projectID string) (chq.Condition, error) {
    if uf == nil || uf.GetScope() == nil {
        return chq.Condition{}, nil
    }
    return chq.EventConditionAliased([]*commonv1.EventFilter{uf.GetScope()}, projectID, "")
}
```

(Same path as `buildSessionScopeCondition` for `SessionQuery.scope`.)

Top-level `filter_groups` compose via `buildTopLevelFilterCondition` in the same `Where` call — same as funnel/retention.

### 4.4 Helpers and tests

| Helper | Returns | Purpose |
| --- | --- | --- |
| `buildUserFlowQuery` | `*chq.Query` | Full query tree (CTEs + outer SELECT) |
| `userFlowNodesArrayExpr` | `string` | Array pipeline for `SelectExpr` |
| `userFlowGroupKeyColumn` | `string` | Column name from `GroupBy` |
| `userFlowNodeExpr` | `string` | Node label expression |
| `userFlowScopeCondition` | `chq.Condition` | Optional scope filter |
| `userFlowNonEmptyGroupKeyCond` | `chq.Condition` | Empty key guard |

**Builder tests** (`builder_test.go`): call `BuildUserFlowQuery`, assert on `q.SQL()` and `q.Args()` — same table-driven pattern as `TestAnalyticsBuildersUseQueryCache` / funnel structure tests. Check for:

- `WITH session_nodes` and `pairs` CTE names
- `project_id = ?` (bound arg, not literal interpolation)
- `use_query_cache = 1`
- `groupArray((occur_time,` without `groupArray(N)(` before `arraySort`
- Scope / filter SQL fragments present or absent per fixture

---

## 5. Executor

**File:** `internal/core/insights/executor.go`

`count(DISTINCT ...)` returns an integer from ClickHouse. `Value` must be `int64` — not `float64`.

Follow the existing funnel/scalar scan pattern (`e.ch.Query`, `rows.Scan`, `recordQueryError`):

```go
type UserFlowRow struct {
    Source string
    Target string
    Value  int64  // count(DISTINCT group_key) — integer, not float
}

func (e *Executor) QueryUserFlow(
    ctx context.Context,
    projectID string,
    q UserFlowQuery,
) ([]UserFlowRow, error) {
    // e.ch.Query(ctx, q.SQL(), q.Args()...)
    // scan source, target, value per row
    // recordQueryError on failure; return nil, fmt.Errorf("QueryUserFlow: %w", err)
}
```

Nil or empty slice on no transitions is fine; `GroupUserFlowResult` handles it.

---

## 6. Grouping (`GroupUserFlowResult`)

**File:** `internal/core/insights/user_flow.go`

### 6.1 Algorithm

```
1.  Compute per-node total weight:
      node_weight[n] += link.value  for each link where source==n or target==n

2.  Sort nodes by weight desc, then id asc (tie-break for determinism).

3.  top_set = set of node_weight[:max_nodes] node ids

4.  Remap each row:
      remap(n) = n if n in top_set, else "$others"

5.  Drop degenerate links: source_remapped == target_remapped
      *** This step MUST run after step 4 ***
      Any two low-volume nodes both collapsing to "$others" would form
      a "$others→$others" self-loop; only post-remap removal catches it.

6.  Re-aggregate: sum(value) grouped by (source_remapped, target_remapped)

7.  Drop zero-value links (defensive; should not occur after step 6)

8.  Sort links by value desc, then source asc, target asc (determinism)

9.  Truncate to max_links (lowest-value links dropped)

10. Build node list from surviving link endpoints:
      - Real nodes: is_others=false, sorted by id asc
      - Overflow bucket: is_others=true, id "$others" (disambiguated if a real
        surviving node already uses that id), always last
      - Nodes referenced only by truncated links are NOT included
```

Shipped refinement (§4.3): steps 5–10 key nodes by an internal `nodeRef{id, others}`,
so the overflow bucket is a distinct identity from a real node literally named
`"$others"` — their traffic is never merged. The bucket is reported via the
`is_others` flag rather than a `label`.

### 6.2 Signature

```go
// GroupUserFlowResult applies top-N pruning, $others collapse, and link truncation
// to raw ClickHouse rows and returns a ready-to-serialize UserFlowResult.
//
// No error return: this function is pure Go with no I/O. It always returns a
// non-nil result. On empty input it returns an empty but valid UserFlowResult.
func GroupUserFlowResult(
    ctx context.Context,
    rows []UserFlowRow,
    maxNodes, maxLinks int,
) *insightsv1.UserFlowResult
```

Always returns non-nil. Never panics on empty input.

### 6.3 Edge cases

| Case | Behaviour |
| --- | --- |
| Empty rows | `UserFlowResult{nodes:[], links:[]}` |
| Single-event sessions | No transitions emitted from ClickHouse; no rows |
| All sessions single-event | Empty result |
| `max_nodes >= total distinct nodes` | No `$others` node; no remapping |
| All nodes collapse to `$others` | All edges become `$others→$others` self-loops; all dropped; empty result |
| `max_links` truncation | Lowest-value links dropped; their endpoint nodes excluded from node list |

---

## 7. `ExecuteQuery` dispatch

**File:** `internal/core/insights/query_execute.go`

`GroupUserFlowResult` has no error return (§6.2). The dispatch has no grouping error path — no double-logging risk.

```go
case insightsv1.InsightType_INSIGHT_TYPE_USER_FLOW:
    q, err := BuildUserFlowQuery(req, projectID)
    if err != nil {
        slog.WarnContext(ctx, "failed to build user flow query", slogx.Error(err),
            slog.String("project_id", projectID))
        return nil, &InvalidQueryError{Message: err.Error(), err: err}
    }
    rows, err := executor.QueryUserFlow(ctx, projectID, q)
    if err != nil {
        return nil, queryFailed(err)  // already logged + recorded in executor
    }
    result := GroupUserFlowResult(ctx, rows, q.MaxNodes(), q.MaxLinks())
    resp.Result = &insightsv1.QueryResponse_UserFlow{UserFlow: result}
```

**Handler** (`internal/app/server/rpc/shared/insights/handler.go`): passthrough unchanged; comment update only.

**Execution path:** `ExecuteQuery` calls `BuildUserFlowQuery` directly — no `userFlowQueryForExecution` dispatcher and no rollup branch (§15).

---

## 8. Dashboard integration

| Area | Action |
| --- | --- |
| `RenderDashboard` / `renderInsightTile` | None |
| `Upsert` / `payload_hash` | None — `InsightQuerySpec` hashes via existing `Encode` |
| `normalizedViewMode` | Wire `SANKEY` → `DASHBOARD_TILE_VIEW_MODE_SANKEY` in `normalizedTileViewModeProto` and `tileViewModeToRPC`. **Do not** default UNSPECIFIED → SANKEY by insight type — the normalizer only sees tile kind + view mode, not `InsightQuerySpec`; today UNSPECIFIED insight tiles default to `LINE`. FE must send `SANKEY` explicitly for user flow tiles. |
| `tileViewModeToRPC` | Map `SANKEY` |
| `tile_payload_test.go` | Add SANKEY to normalization + hash tests |

View mode is a client rendering hint. Server never rejects a non-SANKEY view mode on a user flow tile — no server-side enforcement needed.

**FE coordination window:** Between PR 2 (user flow query ships) and PR 5 (FE tile editor ships), any user flow tile the FE creates will have `UNSPECIFIED` view mode → normalizes to `LINE` → renders incorrectly. This is acceptable as a temporary state since the FE cannot create user flow tiles before PR 5. If there is any interim FE work (e.g. testing against staging), it must explicitly send `view_mode = SANKEY` from the start. Note this in the PR 5 kickoff.

---

## 9. Tests

| Test | File | Covers |
| --- | --- | --- |
| SQL structure | `builder_test.go` | `BuildUserFlowQuery` → `q.SQL()` / `q.Args()`; CTE names; bound `project_id`; `WithQueryCache`; no `groupArray(N)` before sort; scope/filter fixtures |
| Grouping unit | `user_flow_test.go` | Top-N selection; `$others` collapse; self-loop formation and removal after remap; `max_links` truncation; node list excludes truncated-link endpoints; empty input; all-collapse → empty result; deterministic ordering |
| Integration | `integration_test.go` | Seed sessions with known sequences; assert exact link values; single-event session contributes nothing; scope filter excludes expected events |
| Handler happy path | `handler_test.go` | End-to-end response shape for user flow query |
| Dashboard | dashboard tests | User flow tile in `QueryDashboard` returns `UserFlowResult` |

Run: `make test` (integration needs `make infra`).

### Integration test seed

```
session A:  login → dashboard → settings → logout   (3 edges)
session B:  login → dashboard → logout              (2 edges)
session C:  login → logout                          (1 edge)
session D:  login                                   (0 edges — no transitions)

Expected links and values:
  login → dashboard:    2  (A, B)
  login → logout:       1  (C)
  dashboard → settings: 1  (A)
  dashboard → logout:   1  (B)
  settings → logout:    1  (A)

Session D produces zero rows. Assert no panic, no error, D not counted in any edge.
```

### Grouping unit test: `$others` self-loop invariant

```
Input:  [{source:"a", target:"b", value:10}, {source:"b", target:"c", value:5}]
max_nodes = 1

node weights: a=10, b=15, c=5 → top_set = {b}
after remap:  {$others → b: 10}, {b → $others: 5}
no self-loops; result: 2 links, nodes = [b, $others]

---

max_nodes = 0 (impossible via CEL min=2, but test grouping in isolation):
all nodes → $others; all links become $others→$others self-loops; all dropped
result: empty nodes, empty links
```

---

## 10. Delivery slices (PRs)

| PR | Scope | Depends on |
| --- | --- | --- |
| **1** | Proto + CEL + `make rpc` | — |
| **2** | `BuildUserFlowQuery` + `QueryUserFlow` + `GroupUserFlowResult` + `ExecuteQuery` dispatch + unit tests (builder + grouping) | PR 1 |
| **3** | Property nodes + `scope` filter clause + integration test | PR 2 |
| **4** | `SANKEY` view mode + dashboard normalization + `insights.md` section | PR 1 (independent of PR 2/3) |
| **5** | Frontend (separate repo): tile editor + Sankey chart component | PR 2 (needs response shape); independent of PR 3/4 |

**Note on original PR split:** the original plan split builder/executor (PR 2) from grouping/tests (PR 3). This left `ExecuteQuery` dispatching to a not-yet-merged `GroupUserFlowResult`. Fixed here — PRs 2+3 are merged into one.

**Note on PR 5 coordination:** frontend work can begin after PR 2 merges (response shape is stable at that point). PR 4 (SANKEY view mode) ships independently of frontend readiness. See §8 for the FE coordination window between PR 2 and PR 5.

**Note on PR 2 integration test:** a minimal integration test (event kind, session, no scope) is optional in PR 2. The full four-session seed (§9) lands in PR 3 alongside scope/filter coverage.

---

## 11. Defaults and limits

| Field | Default (builder) | CEL min | CEL max |
| --- | --- | --- | --- |
| `node_kind` | `EVENT_KIND` | — | — |
| `group_by` | `SESSION` | — | — |
| `max_hops` | 5 | 1 | 10 |
| `max_nodes` | 20 | 2 | 50 |
| `max_links` | 100 | 1 | 500 |

---

## 12. Out of scope (v1)

- Breakdown series (`UserFlowResult.series`)
- **Rollup MV / fast path** — see §15; raw `events` only
- Funnel → Sankey view transform
- `VisualizationOptions` Sankey tuning (client-only when needed)
- SDK exposure (insights stays on shared/private key/JWT boundary)

---

## 13. Pre-merge checklist

> Historical (pre-merge planning). The feature is shipped and the items below are
> satisfied by the implementation, subject to the divergences noted at the top of
> this document (e.g. `is_others` replaced `label`; no `defaultUserFlowGroupBy`).

- [ ] `make rpc && make lint-proto && make lint && make test`
- [ ] Query assembled exclusively via `internal/core/clickhouse` query builder (§4) — no hand-built SQL strings
- [ ] `project_id` never interpolated into SQL (tenant isolation)
- [ ] Build errors → `InvalidQueryError`; executor errors → `queryFailed` (no sentinel leak)
- [ ] `GroupUserFlowResult` has no error return — confirmed pure Go with no I/O or external deps
- [ ] `GroupUserFlowResult` returns non-nil `UserFlowResult` on empty rows (no nil pointer in handler)
- [ ] Degenerate link removal confirmed to run **after** `$others` remapping (not before)
- [ ] `max_hops` documented as **edges** in proto field comment
- [ ] `scope` semantics (event-level, not session-level) documented in proto field comment
- [x] `defaultUserFlowNodeKind` resolved explicitly in `resolveUserFlowParams`; no silent switch fallthrough (`group_by` has no `defaultUserFlowGroupBy` constant — only SESSION exists)
- [ ] `UserFlowRow.Value` is `int64`, not `float64`
- [ ] Sort-then-slice SQL confirmed (no `groupArray(N)` before `arraySort`) — §4.2, §14.6
- [ ] `arraySlice` 1-indexed offset confirmed correct with a call-site comment — §4.2
- [x] Session-only grouping in v1; cross-session `GroupBy` deferred (§14.2)
- [ ] Empty group key predicate uses correct type for schema (§14.4) — `String` vs `UUID`
- [ ] No rollup wiring — `ExecuteQuery` has no `canUseEventRollup` / `canUseSessionRollup` branch (§15)
- [ ] Window clipping documented in `insights.md` (§14.1)
- [ ] FE coordination window noted in PR 5 kickoff (§8)
- [ ] Single-event session integration test case passes (session D produces no rows, no error)
- [ ] `$others` self-loop unit test case passes
- [ ] Context cancel/deadline propagates unwrapped from `QueryDashboard` errgroup
- [ ] No redundant Go validation duplicating CEL rules
- [ ] [`insights.md`](insights.md) builder table updated with user flow row
- [ ] [`todo.md`](../../todo.md) "User flow (Sankey)" marked done
- [ ] [`CLAUDE.md`](../../CLAUDE.md) updated with SANKEY view mode (optional; do in PR 4)

---

## 14. Gaps and implementation notes

Items surfaced during plan review. Resolve or document before / during implementation.

### 14.1 Time window semantics

The query filters `occur_time ∈ [from, to)` and builds paths from **only events in that window**. A session spanning the boundary gets a **clipped path** (partial sequence), unlike session insights which attribute the full session to its start time via `HAVING on start_time`.

Document this in the user-flow section of [`insights.md`](insights.md) when implementation lands. Callers expecting "full session if it started in window" will see a different graph.

### 14.2 Cross-session user grouping (deferred)

Grouping by `distinct_id` would merge in-window events across sessions into one ordered list, producing transitions such as last-event-of-session-1 → first-event-of-session-2. Those edges are not within a single visit.

**v1 decision:** `GROUP_BY_USER` is not in the proto. Ship session-only (`GROUP_BY_SESSION` / `UNSPECIFIED`). Add a new `GroupBy` enum value when cross-session semantics and UI copy are defined.

### 14.3 Builder defaults (mirror `group_by`)

Resolve `NODE_KIND_UNSPECIFIED` at the top of `BuildUserFlowQuery` using `defaultUserFlowNodeKind` — not a switch fallthrough. CEL may optionally require `node_kind != UNSPECIFIED`; an empty `user_flow: {}` message is valid and relies on builder defaults for limits and kind.

### 14.4 Empty group keys

Implemented via `userFlowNonEmptyGroupKeyCond` in the `session_nodes` CTE `Where` (§4.2):

- `session_id` is `UUID` → `chq.Neq("session_id", "00000000-0000-0000-0000-000000000000")`
- `distinct_id` is `String` → `chq.Neq("distinct_id", "")`

Pin in builder tests. Do not rely on `HAVING length(nodes) >= 2` alone — empty keys still waste scan work upstream.

### 14.5 Same-`occur_time` tie order

Events within a group sharing identical `occur_time` have undefined relative order in `arraySort`. Accepted v1 limitation; note in accuracy docs. Stable tie-break (e.g. `event_id`) is a v2 improvement if needed.

### 14.6 Per-group scan cost

Sort-then-slice collects **all** eligible events per group in the window before truncating to `max_hops + 1` nodes. Cost scales with events per session/user in range, not just `max_hops`. Accepted for v1 — no rollup (§15). Revisit only with prod metrics.

### 14.7 `GroupUserFlowResult` error return — dropped

`GroupUserFlowResult` is pure Go with no I/O. There is no concrete failure condition. The error return has been dropped (see §2 decisions table). The function signature is:

```go
func GroupUserFlowResult(ctx context.Context, rows []UserFlowRow, maxNodes, maxLinks int) *insightsv1.UserFlowResult
```

If a future change introduces a failure mode (e.g. proto marshalling), restore the error return at that point rather than pre-emptively keeping a dead code path.

### 14.8 Dashboard tile customization

`compare`, `thresholds`, and axis-oriented `VisualizationOptions` do not apply to Sankey user-flow tiles. No server enforcement — FE should hide irrelevant controls for `INSIGHT_TYPE_USER_FLOW`.

### 14.9 Property nodes (PR 3)

Use `chq.PropertyExpr` for `NODE_KIND_PROPERTY`. Promoted auto-properties (`$url`, `$country`, …) must read dedicated columns, not stale map keys — same invariant as rollup migration tests (`PropertyExpr` / migration 006 expr sync).

### 14.10 Accuracy tradeoffs (document in `insights.md`)

In addition to ReplacingMergeTree duplicate-delivery inflation (no `FINAL`):

- Window clipping (§14.1)
- Same-timestamp tie order (§14.5)
- Cross-session user grouping when a new `GroupBy` value ships (§14.2)

### 14.11 PR test scope

| PR | Integration test |
| --- | --- |
| **2** | Optional minimal test (event kind, session, no scope). Full four-session seed waits for **PR 3** if scope/filter coverage is required there. |
| **3** | Full integration seed (§9) including scope filter case |
| **4** | Extend `tile_payload_test.go` and `dashboards_internal_test.go` with `SANKEY` — until wired, unknown SANKEY on Upsert normalizes to `LINE` |

### 14.12 Existing CEL rules — no change needed

`funnel_retention_require_events` already allows `USER_FLOW` with empty `events`: the disjunct `(not funnel && not retention) || events.size() > 0` passes when type is USER_FLOW. Do not add redundant exclusion unless the rule is rewritten.

---

## 15. Rollup strategy

User flow does **not** use a rollup in v1. This is intentional — not an omission to backfill before launch.

### 15.1 v1 decision: raw `events` only

| Aspect | Choice |
| --- | --- |
| Query path | `BuildUserFlowQuery` → scan `events` → `GroupUserFlowResult` |
| Dispatch | Single builder in `ExecuteQuery` — **no** `canUseEventRollup`, **no** `canUseSessionRollup`, **no** `userFlowQueryForExecution` |
| MV work | None — do not extend migration 006 or 007 |

Same posture as funnel and retention today: correctness and filter flexibility first; pre-aggregation deferred until usage proves need.

### 15.2 Why existing rollups do not apply

| Rollup | Grain / contents | Why it cannot serve user flow |
| --- | --- | --- |
| **`dashboard_event_rollup_daily`** (006) | `project_id × kind × dim × day` → event counts, `uniqState` | Aggregates **counts by dimension**, not **ordered sequences** or **source→target transitions** |
| **`dashboard_session_rollup`** (007) | `project_id × kind × session_id` → start/end, bounce, entry/exit dim states | Pre-aggregates **session metrics**, not consecutive event paths within a session |

User flow requires: per `session_id` / `distinct_id`, time-ordered node labels → consecutive pairs → `count(DISTINCT group_key)` per edge. Neither MV preserves that path structure.

Additionally, typical user-flow queries **fail rollup eligibility anyway**:

- `scope` and `filter_groups` (session rollup rejects scoped/filtered queries)
- `NODE_KIND_PROPERTY` / custom property breakdowns (event rollup limited to materialized dims)
- Cross-session user `GroupBy` (session rollup is session-grain)
- `max_hops` truncation (path-dependent — not reconstructible from day-level kind counts)

### 15.3 Cost controls without rollup

v1 relies on bounds already in the plan:

| Control | Limit |
| --- | --- |
| Dashboard time-range caps | Per-granularity max window on `QueryRequest` |
| `max_hops` | Default 5, CEL max 10 — caps nodes per path |
| `max_nodes` / `max_links` | Response size caps in Go after query |
| `WithQueryCache(analyticsCacheTTL)` | 60s ClickHouse query cache on outer SELECT (tenant-isolated via bound `project_id`) |
| Partition pruning | `[from, to)` on `occur_time` in CTE WHERE |

**Accepted cost:** per-group scan collects all eligible events in the window before `arraySlice` (§14.6). Heavy sessions in long windows dominate; monitor in prod before considering v2 rollup.

### 15.4 v2+ optional: dedicated transition rollup

Revisit **only if** profiling shows raw scans are too slow for common dashboard tiles **and** hot queries are narrow.

A future MV would be **new infrastructure** — not wiring into 006/007. Sketch:

| Field | Purpose |
| --- | --- |
| `project_id`, `day` | Tenant + partition |
| `source`, `target` | Node labels (event kind or materialized property value) |
| `group_by_kind` | Session vs user grain (or separate tables) |
| `uniqState(distinct_id)` or `uniqState(session_id)` | Distinct traversals per edge |

**Incremental MV** could emit one row per consecutive pair at ingest time (requires ordered per-session processing in MV — non-trivial). **Refreshable APPEND** over `FROM events FINAL` with a closed-bucket watermark is the safer pattern if duplicate delivery matters — see [`clickhouse.md`](clickhouse.md).

**Eligibility would be narrow** (mirroring event rollup conservatism):

- `NODE_KIND_EVENT_KIND` only (or single materialized property dim)
- No `scope`, no `filter_groups`
- `GROUP_BY_SESSION` only
- Day-aligned window
- `max_hops` may still require raw fallback for path truncation correctness

**Accuracy tradeoff:** same as 006 — key omitting `event_id` cannot dedup redeliveries; document if shipped.

**Implementation gate:** add `canUseUserFlowRollup` + `buildUserFlowFromRollup` in a follow-up migration/PR only after integration tests prove parity with raw on a fixed fixture set.
