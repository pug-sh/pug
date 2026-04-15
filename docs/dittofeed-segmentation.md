# Dittofeed Segmentation System — Detailed Explanation

> **Quick Start (5 min):** Read "The Problem They're Solving" → "Architecture Overview" → "Complete Flow" sections only. Stop when you understand the high-level flow.

> **Want to implement?:** Read all sections in order.

> **Going deep?:** Also read the engine explanations and code verification section.

This document explains how Dittofeed implements real-time user segmentation using ClickHouse. It's written for developers who want to understand the architecture and implement similar patterns.

## Overview

Dittofeed is an open-source customer engagement platform (MIT licensed). They solved a common problem: **How do you keep user segments up-to-date without running expensive full-table scans on every change?**

Their solution: **Micro-batching with change tracking via Materialized Views**.

```
Events arrive → Track changes via MV → Worker processes only changed properties → Store results
```

This provides **near real-time** segmentation updates (seconds latency) without crushing ClickHouse.

> **Note:** "Near real-time" in this context means updates happen during the next worker run (typically every 30 seconds to a few minutes, configurable via `computePropertiesInterval`), not instantly on event insert. The workers run via **Temporal** scheduled workflows.
>
> By default, segment recomputation happens every ~30 seconds per workspace (configurable via `computePropertiesInterval` config).

---

## The Problem They're Solving

### Naive Approach (Bad)
```sql
-- Every request scans ALL events
SELECT DISTINCT user_id
FROM user_events
WHERE event = 'button_click'
GROUP BY user_id
HAVING count() >= 2
```

This works initially but gets slower as events grow. Running this every minute = disaster at scale.

### Dittofeed Approach (Good)
1. Track only what changed
2. Recompute only those properties
3. Store results in a separate table

---

## Architecture Overview

```
┌─────────────────────────────────────────────────────────────────────────┐
│                        ClickHouse                                    │
├─────────────────────────────────────────────────────────────────────────┤
│                                                                 │
│  user_events_v2 (MergeTree) ──────┐                               │
│       │                          │                               │
│       │  (raw events)            │                               │
│       ▼                          │                               │
│  updated_computed_property_state (MergeTree + TTL)  ◄── MV     │
│       │                          │                               │
│       │  (tracks what changed)   │                               │
│       │                          │                               │
│       ▼                          │                               │
│  computed_property_state_v3 (AggregatingMergeTree)               │
│       │                          │                               │
│       │  (intermediate state)   │                               │
│       │                          │                               │
│       ▼                          │                               │
│  computed_property_assignments_v2 (ReplacingMergeTree) ◄── Worker   │
│       │                                                        │
│       │  (final results: user → segment assignments)            │
│       ▼                                                        │
│  resolved_segment_state (ReplacingMergeTree)                    │
│                                                                 │
└─────────────────────────────────────────────────────────────────────────┘
                              ▲
                              │
                    Worker polls this table
                    (only changed properties)
```

---

## Database Schema (Tables)

### 1. user_events_v2 (Main Events Table)

```sql
CREATE TABLE IF NOT EXISTS user_events_v2 (
  -- Event type: identify, track, page, screen, group, alias
  event_type Enum(
    'identify' = 1,
    'track' = 2,
    'page' = 3,
    'screen' = 4,
    'group' = 5,
    'alias' = 6
  ) DEFAULT JSONExtract(message_raw, 'type', 'Enum(...)'),

  -- Event name (e.g., "button_click", "page_view")
  event String DEFAULT JSONExtract(message_raw, 'event', 'String'),

  -- When the event actually happened
  event_time DateTime64 DEFAULT parseDateTime64BestEffortOrNull(
    JSONExtractString(message_raw, 'timestamp')
  ),

  -- Unique message ID for idempotency
  message_id String,

  -- User identifiers
  user_id String DEFAULT JSONExtract(message_raw, 'userId', 'String'),
  anonymous_id String DEFAULT JSONExtract(message_raw, 'anonymousId', 'String'),
  user_or_anonymous_id String DEFAULT coalesce(user_id, anonymous_id),

  -- Event properties/traits as JSON string
  properties String DEFAULT JSONExtract(message_raw, 'traits', 'String'),

  -- Hidden from campaigns
  hidden Boolean DEFAULT JSONExtractBool(message_raw, 'context', 'hidden'),

  -- When ClickHouse received the event
  processing_time DateTime64(3) DEFAULT now64(3),

  -- Original raw message
  message_raw String,

  -- Workspace (tenant) ID
  workspace_id String,

  INDEX message_id_idx message_id TYPE bloom_filter(0.01) GRANULARITY 4
)
ENGINE = MergeTree()
ORDER BY (
  workspace_id,
  processing_time,
  user_or_anonymous_id,
  event_time,
  message_id
);
```

**Key points:**
- `message_raw` stores the complete event as JSON (flexible schema)
- `Processing_time` indexed first for time-series queries
- `bloom_filter` index on `message_id` for fast dedup checks
- Column is named `properties` in actual code (not shown in doc snippet above)
- There's also `server_time` column for server-side timestamp

### Why MergeTree?

**MergeTree** is the standard ClickHouse table engine for time-series data:

- **Append-only** — New rows are inserted, never updated in place
- **Background merge** — ClickHouse merges parts in background to optimize storage
- **Primary key index** — Can efficiently filter by primary key columns
- **Suitable for high-volume inserts** — Designed for millions of events/second
- **PARTITION BY** — Can partition by time (e.g., monthly) for efficient queries

Used here because events are **immutable once written** — they represent historical facts.

---

### 2. computed_property_state_v3 (State Table)

This is the **intermediate state** — stores partial aggregations so workers can compute incrementally.

```sql
CREATE TABLE IF NOT EXISTS computed_property_state_v3 (
  workspace_id LowCardinality(String),
  
  -- Type: user_property or segment
  type Enum('user_property' = 1, 'segment' = 2),
  
  -- ID of the computed property/segment definition
  computed_property_id LowCardinality(String),
  
  -- State ID (multiple states per property for complex definitions)
  state_id LowCardinality(String),
  
  -- User ID
  user_id String,
  
  -- For string properties: argMax(state, timestamp)
  last_value AggregateFunction(argMax, String, DateTime64(3)),
  
  -- For count-based: uniq count of message IDs
  unique_count AggregateFunction(uniq, String),
  
  -- When the event happened
  event_time DateTime64(3),
  
  -- Array of message IDs for deduplication
  grouped_message_ids AggregateFunction(groupArray, String),
  
  -- When state was computed
  computed_at DateTime64(3)
)
ENGINE = AggregatingMergeTree()
PARTITION BY (workspace_id, toYear(event_time))
ORDER BY (
  workspace_id,
  type,
  computed_property_id,
  state_id,
  user_id,
  event_time
);
```

**Key concepts:**
- **AggregateFunction** columns store intermediate state, not final values
- `uniq` = count distinct (e.g., "how many button clicks")
- `argMax` = latest value by timestamp
- `groupArray` = collect all message IDs for idempotency
- State is keyed by `(workspace_id, computed_property_id, state_id, user_id)` — multiple states per segment (for complex definitions with multiple conditions)

### Why AggregatingMergeTree?

**AggregatingMergeTree** automatically **merges and aggregates** rows with the same key:

- **AggregateFunction columns** — Store intermediate aggregation state (not final values)
- **Automatic merging** — When ClickHouse merges parts, it aggregates matching keys
- **Incredibly efficient** — Can process billions of rows with minimal computation
- **Incremental updates** — Just insert new state; merging auto-aggregates

**How it works:**
```sql
-- First insert
user_A, uniqState(msg_1) -- count = 1

-- Second insert  
user_A, uniqState(msg_2) -- count = 1

-- After merge (automatic)
user_A, uniqMerge(state) -- count = 2 ✨
```

**Used here** because we need to incrementally accumulate state (count, latest value) across worker runs without reading all historical data.

---

### 3. computed_property_assignments_v2 (Final Assignments)

Stores the final segment/property assignments for each user.

```sql
CREATE TABLE IF NOT EXISTS computed_property_assignments_v2 (
  workspace_id LowCardinality(String),
  
  -- user_property or segment
  type Enum('user_property' = 1, 'segment' = 2),
  
  computed_property_id LowCardinality(String),
  
  user_id String,
  
  -- For segments: true/false
  -- For user properties: the actual value (JSON string)
  segment_value Boolean,
  user_property_value String,
  
  -- Latest event time for this assignment
  max_event_time DateTime64(3),
  
  -- When assigned
  assigned_at DateTime64(3) DEFAULT now64(3)
)
ENGINE = ReplacingMergeTree()
ORDER BY (
  workspace_id,
  type,
  computed_property_id,
  user_id
);
```

**ReplacingMergeTree** keeps the latest value per `(workspace_id, type, computed_property_id, user_id)` tuple.

### Why ReplacingMergeTree?

**ReplacingMergeTree** keeps only the **latest row** for each primary key:

- **automatic deduplication** — On merge, keeps newest row by `sort_by` column
- **simplifies logic** — Don't need to re-aggregate; latest insert wins
- **good for mutable data** — User segments change over time
- **lightweight** — No AggregateFunction complexity

**Used here** because assignments change — we want the latest value, not all historical values.

> **Note for resolved_segment_state:** Also uses ReplacingMergeTree (same explanation applies) — stores boolean evaluation results for segment nodes (AND/OR tree evaluation).

---

### 4. updated_computed_property_state (Change Tracker)

Tracks which properties have changed — worker queries this to know what to recompute.

```sql
CREATE TABLE IF NOT EXISTS updated_computed_property_state (
  workspace_id LowCardinality(String),
  type Enum('user_property' = 1, 'segment' = 2),
  computed_property_id LowCardinality(String),
  state_id LowCardinality(String),
  user_id String,
  computed_at DateTime64(3)
)
ENGINE = MergeTree()
PARTITION BY toYYYYMMDD(computed_at)
ORDER BY computed_at
TTL toStartOfDay(computed_at) + interval 24 hour;
```

**Auto-cleanup:** TTL deletes old rows after 24 hours.

### Why MergeTree with TTL?

**MergeTree** with **TTL** (Time To Live) provides automatic cleanup:

- **PARTITION BY** — Daily partitions for time-based data
- **TTL** — Automatic row deletion after interval (24 hours here)
- **No maintenance needed** — ClickHouse auto-deletes old rows
- **Memory efficient** — Old change records auto-removed

**Used here** because the change tracker only needs **recent** changes — old entries are irrelevant after processing.

---

### 5. resolved_segment_state (Boolean Evaluation)

For complex segments with AND/OR logic, stores intermediate boolean evaluations.

```sql
CREATE TABLE IF NOT EXISTS resolved_segment_state (
  workspace_id LowCardinality(String),
  segment_id LowCardinality(String),
  state_id LowCardinality(String),
  user_id String,
  
  -- Result of evaluating this segment node
  segment_state_value Boolean,
  
  max_event_time DateTime64(3),
  
  computed_at DateTime64(3),
  
  -- Indexes for fast queries
  INDEX segment_state_value_idx segment_state_value TYPE minmax GRANULARITY 4,
  INDEX computed_at_idx computed_at TYPE minmax GRANULARITY 4
)
ENGINE = ReplacingMergeTree()
ORDER BY (
  workspace_id,
  segment_id,
  state_id,
  user_id
);
```

---

### 6. Internal Events Table (Campaign Tracking)

Stores pre-parsed campaign events for journey triggers.

```sql
CREATE TABLE IF NOT EXISTS internal_events (
  workspace_id String,
  user_or_anonymous_id String,
  user_id String,
  anonymous_id String,
  message_id String,
  event String,
  
  -- Event timestamps
  event_time DateTime64(3),
  processing_time DateTime64(3),
  
  -- Parsed properties
  properties String,
  
  -- Pre-extracted campaign fields (from JSON)
  template_id String,
  broadcast_id String,
  journey_id String,
  triggering_message_id String,
  channel_type String,
  delivery_to String,
  delivery_from String,
  origin_message_id String,
  
  -- Hidden flag
  hidden Boolean,
  
  -- Bloom filters for fast lookups
  INDEX idx_template_id template_id TYPE bloom_filter(0.01) GRANULARITY 4,
  INDEX idx_broadcast_id broadcast_id TYPE bloom_filter(0.01) GRANULARITY 4,
  INDEX idx_journey_id journey_id TYPE bloom_filter(0.01) GRANULARITY 4
)
ENGINE = MergeTree()
ORDER BY (
  workspace_id,
  processing_time,
  event,
  user_or_anonymous_id,
  message_id
);
```

---

## Materialized Views

### Purpose

Materialized Views (MVs) in ClickHouse are **not views** — they're **insert triggers**. When data is inserted into the source table, the MV automatically inserts transformed data into the target table.

### Change Tracking MV

```sql
CREATE MATERIALIZED VIEW IF NOT EXISTS updated_computed_property_state_v3_mv
TO updated_computed_property_state
AS SELECT
  workspace_id,
  type,
  computed_property_id,
  state_id,
  user_id,
  computed_at
FROM computed_property_state_v3
GROUP BY
  workspace_id,
  type,
  computed_property_id,
  state_id,
  user_id,
  computed_at;
```

This MV automatically tracks when any user's state changes in `computed_property_state_v3`.

---

## The Compute Worker

The worker is the critical piece. It doesn't recompute everything — it only processes properties that have changes.

### Simplified Flow

```typescript
// Pseudocode of the worker

async function computePropertiesIncremental(workspaceId: string) {
  // 1. Find which properties have changed since last run
  const changedProperties = await query(`
    SELECT DISTINCT computed_property_id, type
    FROM updated_computed_property_state
    WHERE workspace_id = {workspaceId}
  `);

  // 2. For each changed property, recompute incrementally
  for (const prop of changedProperties) {
    // Read intermediate state from AggregatingMergeTree
    const state = await query(`
      SELECT user_id, uniqMerge(unique_count) as count
      FROM computed_property_state_v3
      WHERE workspace_id = {workspaceId}
        AND computed_property_id = {prop.id}
        AND type = {prop.type}
      GROUP BY user_id
      HAVING count >= {prop.threshold}
    `);

    // 3. Write final assignments
    await insert(`
      INSERT INTO computed_property_assignments_v2
      (workspace_id, type, computed_property_id, user_id, segment_value, assigned_at)
    `, state.map(user => ({
      workspace_id: workspaceId,
      type: prop.type,
      computed_property_id: prop.id,
      user_id: user.id,
      segment_value: user.count >= prop.threshold,
      assigned_at: now()
    })));
  }
}
```

### Why This Is Fast

- Worker only processes rows from `updated_computed_property_state` (small)
- Not scanning all events (millions/second vs thousands changed)
- AggregatingMergeTree stores pre-aggregated state

---

## Understanding AggregateFunction

ClickHouse AggregateFunctions are special — they store **intermediate state** that can be merged.

### Example: Count Distinct

```sql
-- Store: uniqState collects unique values
INSERT INTO computed_property_state_v3
SELECT
  workspace_id,
  'segment',
  'button_click_2plus',
  'button_click',  -- state_id for a single condition
  user_id,
  argMaxState('', event_time),          -- not used for count
  uniqState(message_id),              -- collect unique message IDs
  event_time,
  groupArrayState(message_id),        -- for idempotency
  now()
FROM user_events_v2
WHERE event = 'button_click'
GROUP BY workspace_id, user_id;
```

```sql
-- Read: uniqMerge combines states
SELECT
  user_id,
  uniqMerge(unique_count) as button_click_count
FROM computed_property_state_v3
WHERE computed_property_id = 'button_click_2plus'
GROUP BY user_id
HAVING uniqMerge(unique_count) >= 2;
```

### Available AggregateFunctions

| Function | Purpose | Use Case |
|----------|---------|---------|
| `uniqState`/`uniqMerge` | Count distinct | "how many unique events" |
| `argMaxState`/`argMaxMerge` | Latest value | "latest property value" |
| `sumState`/`sumMerge` | Sum | "total revenue" |
| `avgState`/`avgMerge` | Average | "average order value" |
| `groupArrayState`/`groupArrayMerge` | Collect array | Store IDs for dedup |

---

## Segment Types Supported

### 1. Performed (Event Count)

"User who clicked button 3+ times in last 7 days"

```typescript
{
  type: 'segment',
  name: 'Power Users',
  definition: {
    nodes: [{
      type: 'Performed',
      property: 'button_click',
      relation: 'and',  // AND/OR with other conditions
      operator: 'performed',
      count: 3,
      timeWindow: 7 * 24 * 60 * 60 * 1000  // 7 days in ms
    }]
  }
}
```

### 2. User Property

"User with plan = 'pro'"

```typescript
{
  type: 'segment',
  name: 'Pro Users',
  definition: {
    nodes: [{
      type: 'UserProperty',
      property: 'plan',
      operator: 'equals',
      value: 'pro'
    }]
  }
}
### 3. Manual

Manually assigned segment.

### 4. Has Started Journey

User who started a specific journey.

---

## Behavioral Segmentation

Yes! Dittofeed supports the two cases you mentioned:

### 1. Event Frequency — "did X more than N times"

```typescript
// Segment: "User who clicked button 3+ times in last 7 days"
{
  type: 'segment',
  name: 'Power Users',
  definition: {
    entryNode: {
      type: 'Performed',
      id: 'node_1',
      event: 'button_click',
      times: 3,                    // N times
      timesOperator: 'greater_than',  // >=
      withinSeconds: 7 * 24 * 60 * 60  // 7 days in seconds
    }
  }
}
```

The `Performed` node type checks event frequency:

| Field | Type | Description |
|-------|------|-------------|
| `event` | string | Event name to match |
| `times` | number | Required count |
| `timesOperator` | enum | `greater_than`, `equals`, `less_than`, etc. |
| `withinSeconds` | number | Time window (optional) |

SQL equivalent:

```sql
-- Count events per user, filter by count
SELECT user_id
FROM user_events_v2
WHERE workspace_id = 'ws_123'
  AND event = 'button_click'
  AND event_time >= now() - INTERVAL 7 DAY
GROUP BY user_id
HAVING count() >= 3;
```

### 2. NOT Condition — "did X but NOT Y"

NOT conditions are handled via **negative segment filters** when querying users:

```typescript
// Query: Get users in "US" but NOT in "Enterprise" segment
const users = await findUsers({
  workspaceId: 'ws_123',
  segmentFilter: ['seg_us'],      // MUST be in this segment
  negativeSegmentFilter: ['seg_enterprise']  // MUST NOT be in this segment
});
```

Query logic (simplified):

```sql
SELECT user_id
FROM computed_property_assignments_v2
WHERE workspace_id = 'ws_123'
  AND segment_value = true
  AND computed_property_id = 'seg_us'
  AND user_id NOT IN (
    SELECT user_id
    FROM computed_property_assignments_v2
    WHERE workspace_id = 'ws_123'
      AND segment_value = true
      AND computed_property_id = 'seg_enterprise'
  );
```

### Complete Segment Node Types

| Node Type | Description | Supports Frequency | Supports NOT |
|-----------|-------------|-------------------|--------------|
| `Performed` | Did event N times | ✅ | ❌ |
| `KeyedPerformed` | Did event with property | ✅ | ❌ |
| `LastPerformed` | Last event matches | ❌ | via operator |
| `Trait` | User property = value | ❌ | ✅ (NotEquals) |
| `And` | All children must match | N/A | N/A |
| `Or` | Any child matches | N/A | N/A |
| `Manual` | Manually assigned | N/A | ✅ via query |

### Negation at Query Time

For "did action X but NOT action Y", create two segments:

1. `seg_clicked_button` — users who clicked
2. `seg_purchased` — users who purchased

Query for "clicked but didn't purchase":

```typescript
const users = await findUsers({
  workspaceId: 'ws_123',
  segmentFilter: ['seg_clicked_button'],
  negativeSegmentFilter: ['seg_purchased']
});
```

---

## Query Examples

### Get All Users in a Segment

```sql
SELECT user_id
FROM computed_property_assignments_v2
WHERE workspace_id = 'ws_123'
  AND type = 'segment'
  AND computed_property_id = 'seg_power_users'
  AND segment_value = true;
```

### Get User Property Value

```sql
SELECT 
  user_id,
  user_property_value
FROM computed_property_assignments_v2
WHERE workspace_id = 'ws_123'
  AND type = 'user_property'
  AND computed_property_id = 'up_email';
```

### Check Segment Entry (for Journey Triggers)

```sql
SELECT user_id
FROM computed_property_assignments_v2
WHERE workspace_id = 'ws_123'
  AND type = 'segment'
  AND computed_property_id = 'seg_new_signup'
  AND segment_value = true
  AND max_event_time > now64(3) - INTERVAL 1 MINUTE;
```

---

## For Cotton (Our Implementation)

We can adapt this pattern:

### Tables We Need

1. `user_events` — existing (our events table)
2. `segment_assignments` — user → segment mappings (ReplacingMergeTree)
3. `updated_segments` — change tracker (MergeTree + TTL)
4. `segment_state` — intermediate state (AggregatingMergeTree)

### Worker Pattern

```go
func (w *segmentWorker) Run(ctx context.Context) error {
    // 1. Get changed segments
    changed, err := w.getChangedSegments(ctx)
    if err != nil {
        return err
    }

    // 2. For each changed, recompute
    for _, seg := range changed {
        if err := w.recomputeSegment(ctx, seg); err != nil {
            return err
        }
    }

    // 3. Mark as processed
    return w.markProcessed(ctx, changed)
}
```

### Trigger Options

1. **Temporal workflow** (Dittofeed uses Temporal for orchestration): Scheduled runs
2. **Kafka consumer**: On new events, INSERT to change tracker (optional mode)
3. **Timer-based cron**: Every 1-5 minutes

> **Note:** Dittofeed uses **Temporal** (workflow orchestration platform) to schedule segment recomputation runs. The worker is triggered through a Temporal workflow, not a simple cron job. Each run processes all segments for a workspace.

---

## Corrections from Code Review

### Correction 1: Kafka Mode is Optional

The doc mentions "auto via MV" but Dittofeed actually supports two modes:

1. **Direct insert** (default): Events go directly to `user_events_v2`
2. **Kafka mode** (optional): Events go to Kafka → queue table → MV → main table

```sql
-- Kafka queue table (optional)
CREATE TABLE IF NOT EXISTS user_events_queue_v2
(message_raw String, workspace_id String, message_id String)
ENGINE = Kafka('brokers', 'topic', 'group', 'JSONEachRow');

-- MV moves data from queue to main table
CREATE MATERIALIZED VIEW user_events_mv_v2 TO user_events_v2 AS
SELECT * FROM user_events_queue_v2;
```

This is configured via `config().writeMode === "kafka"`.

### Why Kafka Engine?

**Kafka** table engine is a **consumer** — it reads from a Kafka topic:

- **Push to ClickHouse** — Kafka pushes messages to ClickHouse (not ClickHouse pulling)
- **Exactly-once semantics** — Can configure for exactly-once delivery
- **Decoupling** — API writes to Kafka; CH processes async
- **Backpressure handling** — Kafka buffers when CH is slow
- **Materialized View bridge** — MV reads from Kafka table to populate main table

```
Kafka topic → Kafka table (ENGINE=Kafka) → Materialized View → user_events_v2
```

**Used here** for high-throughput deployments where the API shouldn't wait for CH inserts.

### Correction 2: MV Tracks State Changes, Not Events

The doc says MV tracks "what changed" but the actual MV tracks changes to `computed_property_state_v3` (the intermediate state), not raw events:

```sql
-- Actual MV (from code line 159-176)
CREATE MATERIALIZED VIEW updated_computed_property_state_v3_mv
TO updated_computed_property_state
AS SELECT ... FROM computed_property_state_v3  -- NOT user_events_v2
GROUP BY workspace_id, type, computed_property_id, state_id, user_id;
```

The flow is: Events → worker updates state → state changes trigger MV → worker knows to recompute.

### Correction 3: Worker Uses "Pruned" Properties

The actual code uses "pruning" to skip properties that haven't changed:

```typescript
// From computeProperties.ts line 126
const prunedComputedProperties = await pruneComputedProperties(args);
// This filters to only properties with actual changes
```

The `pruneComputedProperties` function determines which properties need recomputation.

### Correction 4: Three-Step Worker Flow (Not Two)

The actual worker has three phases:

1. **Prune** — Filter to only changed properties
2. **Compute State** — Update intermediate state in AggregatingMergeTree
3. **Compute Assignments** — Write final assignments to ReplacingMergeTree

```typescript
await computeState(prunedArgs);        // Step 2: Update state
await computeAssignments(prunedArgs);  // Step 3: Write assignments
await processAssignments(args);       // Step 4 (optional): Trigger journeys
```

The third phase (`processAssignments`) handles journey triggers and external integrations.

---

## Complete Flow: How Users Enter and Exit Segments

This section explains the complete journey from event to segment membership.

### Overview Diagram

```
┌──────────────────────────────────────────────────────────────────────────────────────┐
│                         COMPLETE FLOW                                    │
│                                                                      │
│  ┌─────────┐     ┌──────────────┐     ┌─────────────┐                 │
│  │  User   │────▶│  Events API  │────▶│ ClickHouse │                 │
│  │  does   │     │  receives   │     │  inserts   │                 │
│  │ action  │     │  event      │     │  to table  │                 │
│  └─────────┘     └──────────────┘     └──────┬──────┘                 │
│                                              │                        │
│                                              ▼                        │
│                                    ┌─────────────────┐               │
│                                    │ user_events_v2 │               │
│                                    └────────┬────────┘               │
│                                             │                        │
│                                             │ (auto via MV?)         │
│                                             ▼                        │
│                                    ┌─────────────────────┐          │
│                                    │ updated_computed_  │          │
│                                    │ property_state      │          │
│                                    └──────────┬────────┘          │
│                                               │                    │
│                                               ▼                    │
│  ┌──────────────────────────────────────────────────────┐            │
│  │              WORKER (runs every N seconds)            │            │
│  │  1. Poll updated_computed_property_state           │            │
│  │  2. For each changed property:                       │            │
│  │     - Read AggregatingMergeTree state              │            │
│  │     - Compute: uniqMerge(count) >= N?               │            │
│  │     - Write to ReplacingMergeTree                  │            │
│  └───────────────────────────────────────┬────────────┘            │
│                                          │                          │
│                                          ▼                          │
│  ┌──────────────────────────────────────────────────────┐            │
│  │           computed_property_assignments_v2            │            │
│  │    (user_id, segment_id, segment_value=true/false)   │            │
│  └──────────────────────────────────────────────────────┘            │
│                                          │                          │
│                                          ▼                          │
│  ┌──────────────────────────────────────────────────────┐            │
│  │                   JOURNEY TRIGGERS                    │            │
│  │   - Segment entered → Start journey                   │            │
│  │   - Segment exited  → Exit journey               │            │
│  └──────────────────────────────────────────────────────┘            │
└──────────────────────────────────────────────────────────────────────────────────────┘
```

---

### Step-by-Step Flow (Enter)

Let's walk through: **User clicks button 3 times → enters "Power Users" segment**

#### Step 1: User Performs Action

```javascript
// Frontend: User clicks "Buy Now" button
Analytics.track('button_click', {
  productId: 'prod_123',
  price: 99
});
```

**What's sent to API:**

```json
{
  "type": "track",
  "event": "button_click",
  "userId": "user_456",
  "timestamp": "2024-01-15T10:30:00Z",
  "properties": {
    "productId": "prod_123",
    "price": 99
  }
}
```

#### Step 2: API Receives Event

```typescript
// API endpoint receives the event
app.post('/collect', async (req, res) => {
  const event = req.body;
  
  // Insert into ClickHouse
  await clickhouse.insert({
    table: 'user_events_v2',
    values: [{
      event_type: 'track',
      event: event.event,
      event_time: parseDateTime64(event.timestamp),
      message_id: uuid(),
      user_id: event.userId,
      user_or_anonymous_id: event.userId,
      properties: JSON.stringify(event.properties),
      workspace_id: 'ws_123',
      processing_time: now64(3)
    }],
    format: 'JSONEachRow'
  });
  
  res.json({ success: true });
});
```

#### Step 3: Event Stored in ClickHouse

```sql
-- user_events_v2 table now has:
┌─────────────┬──────────┬────────────┬───────────┬─────────────┬──────────────┐
│ message_id │ user_id  │  event    │event_time │workspace_id│process_time│
├─────────────┼──────────┼────────────┼───────────┼─────────────┼──────────────┤
│ msg_001    │ user_456 │button_click│10:30:00  │ ws_123     │ 10:30:01   │
│ msg_002    │ user_456 │button_click│10:30:05  │ ws_123     │ 10:30:06   │
│ msg_003    │ user_456 │button_click│10:30:10  │ ws_123     │ 10:30:11   │
└─────────────┴──────────┴────────────┴───────────┴─────────────┴──────────────┘
```

#### Step 4: Change Tracking (Automatic or Triggered)

**Option A: Materialized View (automatic)**

```sql
-- This MV fires on every insert to user_events_v2
-- Tracks which properties need recomputation
CREATE MATERIALIZED VIEW user_events_changed_mv
TO updated_computed_property_state
AS SELECT DISTINCT
  'ws_123' as workspace_id,
  'segment' as type,
  'seg_power_users' as computed_property_id,
  'button_click' as state_id,
  'user_456' as user_id,
  now64(3) as computed_at
FROM user_events_v2
WHERE event = 'button_click';
```

**Option B: Worker Polls (simpler)**

Instead of MVs, worker polls for new events:

```sql
-- Worker runs every 30 seconds
SELECT user_id, count() as click_count
FROM user_events_v2
WHERE workspace_id = 'ws_123'
  AND event = 'button_click'
  AND event_time >= now() - INTERVAL 30 SECOND  -- Only last 30 sec
GROUP BY user_id
HAVING count() >= 3;
```

#### Step 5: Worker Recomputes Segment

```typescript
// Worker pseudocode
async function computePropertiesIncremental() {
  // Get all events in last 30 seconds (simplified)
  const newEvents = await query(`
    SELECT user_id, event, count() as cnt
    FROM user_events_v2
    WHERE workspace_id = 'ws_123'
      AND event_time >= now() - INTERVAL 30 SECOND
    GROUP BY user_id, event
  `);
  
  for (const row of newEvents) {
    // Check each segment definition
    const segments = await getSegmentsForEvent(row.event);
    
    for (const seg of segments) {
      const meetsCondition = row.cnt >= seg.threshold;
      const previousValue = await getPreviousValue(seg.id, row.user_id);
      
      if (meetsCondition !== previousValue) {
        // Value changed → write new assignment
        await insert(`
          INSERT INTO computed_property_assignments_v2
          (workspace_id, type, computed_property_id, user_id, 
           segment_value, max_event_time, assigned_at)
        `, [{
          workspace_id: 'ws_123',
          type: 'segment',
          computed_property_id: seg.id,
          user_id: row.user_id,
          segment_value: meetsCondition,
          max_event_time: row.max_event_time,
          assigned_at: now64(3)
        }]);
        
        // Trigger journey if entered
        if (meetsCondition && !previousValue) {
          await triggerJourneyEntry(seg.id, row.user_id);
        }
      }
    }
  }
}
```

#### Step 6: Assignment Stored

```sql
-- computed_property_assignments_v2:
┌─────────────┬─────────┬──────────────┬──────────┬────────────┐
│ workspace_id│  type   │computed_prop │ user_id  │seg_value  │
├─────────────┼─────────┼──────────────┼──────────┼───────────┤
│ ws_123     │ segment │seg_power_user │ user_456 │ true      │
└─────────────┴─────────┴──────────────┴──────────┴───────────┘
```

User is now IN the segment!

#### Step 7: Journey Triggered (Optional)

If there's a journey with segment entry trigger:

```typescript
// Journey definition
{
  name: 'Onboarding Flow',
  entryNode: {
    type: 'SegmentEntryNode',
    segmentId: 'seg_power_users'
  }
}
```

When user enters segment:

```sql
-- Detect recent segment entries
SELECT user_id, computed_property_id
FROM computed_property_assignments_v2
WHERE workspace_id = 'ws_123'
  AND computed_property_id = 'seg_power_users'
  AND segment_value = true
  AND assigned_at > now() - INTERVAL 1 MINUTE;
```

Then trigger journey:

```typescript
await temporalClient.signalStart(
  'journey-workflow',
  { userId: 'user_456' }
);
```

---

### Step-by-Step Flow (Exit)

User exits when they no longer meet segment conditions. Example: **User hasn't clicked in 7 days → exits "Power Users"**

#### Exit Detection

The worker checks on every run:

```sql
-- User exits when their count falls below threshold
SELECT user_id, uniqMerge(unique_count) as click_count
FROM computed_property_state_v3
WHERE workspace_id = 'ws_123'
  AND computed_property_id = 'seg_power_users'
GROUP BY user_id
HAVING uniqMerge(unique_count) < 3;
```

For time-windowed segments (7 days):

```sql
-- Re-compute with time window
SELECT user_id, count() as recent_clicks
FROM user_events_v2
WHERE workspace_id = 'ws_123'
  AND event = 'button_click'
  AND event_time >= now() - INTERVAL 7 DAY  -- Only last 7 days
GROUP BY user_id
HAVING count() < 3;
```

#### Write Exit Assignment

```sql
-- Update to false
INSERT INTO computed_property_assignments_v2
(workspace_id, type, computed_property_id, user_id, segment_value, assigned_at)
VALUES
('ws_123', 'segment', 'seg_power_users', 'user_456', false, now64(3));
```

#### Journey Exit (Optional)

If journey has segment exit trigger:

```typescript
// Journey definition
{
  name: 'Re-engagement',
  exitNode: {
    type: 'SegmentExitNode',
    segmentId: 'seg_power_users'
  }
}
```

When user exits, journey can send re-engagement email.

---

### Exit Flow is Also Scalable — The "Expired Query" Pattern

The naive exit approach (checking every user every run) doesn't scale. Dittofeed uses a clever pattern:

**Key insight:** Instead of checking if user should exit, flip the logic — mark users as false who no longer match.

#### Step 1: User Has Existing Assignment (value = true)

```sql
-- Current state: user is in "Power Users" segment
SELECT * FROM computed_property_assignments_v2
WHERE user_id = 'user_456' AND computed_property_id = 'seg_power_users';

-- Result:
┌─────────────┬──────────┬─────────────┬──────────┬────────────┐
│ workspace_id│  type   │ computed_id │ user_id  │seg_value │
├─────────────┼──────────┼─────────────┼──────────┼──────────┤
│ ws_123     │ segment │seg_power_usr │ user_456 │ true    │
└─────────────┴──────────┴─────────────┴──────────┴──────────┘
```

#### Step 2: User Clicks Button (3+ times in last 7 days)

Worker runs → computes that user meets condition → writes `segment_value = true`

#### Step 3: Time Passes — No More Clicks

After 7 days pass, user hasn't clicked. Workers runs:

```sql
-- The "expired query" pattern
INSERT INTO resolved_segment_state
SELECT
  workspace_id,
  segment_id,
  state_id,
  user_id,
  False,  -- MARK AS FALSE (exited)
  max_event_time,
  now()
FROM resolved_segment_state
WHERE workspace_id = 'ws_123'
  AND segment_id = 'seg_power_users'
  AND state_id = 'button_click'
  AND segment_state_value = True  -- Currently in segment
  AND (workspace_id, segment_id, state_id, user_id, True) NOT IN (
    -- User no longer meets criteria (no events in window)
    SELECT workspace_id, computed_property_id, state_id, user_id, True
    FROM computed_property_state_v3
    WHERE event_time >= now() - INTERVAL 7 DAY
    GROUP BY workspace_id, computed_property_id, state_id, user_id
    HAVING uniqMerge(unique_count) >= 3
  );
```

**Logic:** "Find users currently true, who are NOT in the qualifying set anymore → mark as false"

This is **scalable** because:

1. Only processes users currently TRUE (small set, not all users)
2. The subquery is the same as enter query (reused)
3. Uses NOT IN rather than full scan

#### Step 4: User Exits Segment

```sql
-- Now shows false
SELECT * FROM computed_property_assignments_v2
WHERE user_id = 'user_456' AND computed_property_id = 'seg_power_users';

-- Result:
┌───��─────────┬──────────┬─────────────┬──────────┬────────────┐
│ workspace_id│  type   │ computed_id │ user_id  │seg_value │
├─────────────┼──────────┼─────────────┼──────────┼──────────┤
│ ws_123     │ segment │seg_power_usr │ user_456 │ false   │  ← EXITED!
└─────────────┴──────────┴─────────────┴──────────┴──────────┘
```

---

### Correction 5: Segment Node Types are Different

The doc uses generic names (`Performed`, `UserProperty`). The actual Dittofeed types:

```typescript
// From isomorphic-lib/src/types.ts
export enum SegmentNodeType {
  Trait = "Trait",           // User property check
  And = "And",             // AND logic
  Or = "Or",              // OR logic
  Performed = "Performed",    // Did event N times
  LastPerformed = "LastPerformed", // Last event matches
  Broadcast = "Broadcast",    // Received broadcast/message
  SubscriptionGroup = "SubscriptionGroup",     // Subscription group member
  SubscriptionGroupUnsubscribed = "SubscriptionGroupUnsubscribed",
  Email = "Email",           // Email action (sent, opened, clicked)
  Manual = "Manual",        // Manually assigned
  RandomBucket = "RandomBucket", // Random % rollout
  KeyedPerformed = "KeyedPerformed", // Event with property filter
  Everyone = "Everyone",   // Everyone (no filter)
  Includes = "Includes", // User ID list
}
```

**All segment types can trigger journey entry/exit** — The journey system doesn't care HOW the segment gets its value, only that the value changes. This includes:
- **Trait segments** — trigger when user's property changes to matching value
- **Performed segments** — trigger when event count meets threshold  
- **Manual segments** — trigger when admin manually adds/removes user
- **Any other type** — same mechanism

---

## Why This Is Scalableh | Enter | Exit | Scales? |
|----------|-------|-----|--------|
| Check every user | ✅ | ❌ | No — O(n) every run |
| Only check TRUE users | ✅ | ✅ | Yes — O(active users) |

**The expired query only processes users currently in segments**, not all users.

---

### Summary: Enter vs Exit

| Event | Enter (value changes false→true) | Exit (value changes true→false) |
|-------|------------------------------|------------------------------|
| **How** | User meets condition (subquery matches) | User doesn't meet anymore (NOT IN subquery) |
| **Query** | `HAVING count >= N` | `current=True AND NOT IN qualifying` |
| **Set size** | All users | Only TRUE users |
| **Scalable** | ✅ | ✅ |
| **Journey** | SegmentEntered → Start | SegmentExited → Exit |

### Key Points

1. **Exit via expired query** — Only check users currently in segment (small set)
2. **NOT IN pattern** — "In segment but no longer qualifies = exit"
3. **Time windows work** — Rolling window naturally expires users
4. **Journey triggers** — Can trigger on entry or exit based on journey config
5. **ALL segment types trigger journeys** — Trait, Performed, Manual, Everyone — any type can subscribe a journey

---

## Journey Triggers Work for Property-Based Segments Too

This is important: **journey entry/exit triggers work for ALL segment types**, not just event-based ones:

| Segment Type | Entry Trigger | Exit Trigger |
|-------------|--------------|-------------|
| **Trait** (property = value) | ✅ when property changes to match | ✅ when changes away |
| **Performed** (event N times) | ✅ when count >= N | ✅ when count < N |
| **Manual** (manual assignment) | ✅ when admin adds user | ✅ when admin removes |
| **Everyone** | ✅ all new users | N/A |
| **And/Or** | ✅ combined logic | ✅ combined logic |

---

## Complete Flow: Property-Based Segment Entry (Trait)

This explains how a user's property update flows into a segment.

### Step 1: User Sends Identify Event

```javascript
// User updates their profile
Analytics.identify("user_456", {
  plan: "pro",
  company: "Acme Corp"
});
```

**API payload:**

```json
{
  "type": "identify",
  "userId": "user_456",
  "traits": {
    "plan": "pro",
    "company": "Acme Corp"
  },
  "timestamp": "2024-01-15T10:30:00Z"
}
```

### Step 2: Event Stored

```sql
-- user_events_v2 receives identify event
INSERT INTO user_events_v2 (event_type, user_id, properties, event_time, workspace_id)
VALUES ('identify', 'user_456', '{"plan":"pro","company":"Acme Corp"}', '2024-01-15T10:30:00Z', 'ws_123');
```

> **Key:** Trait segments filter on `event_type == 'identify'` events!

### Step 3: Worker Recomputes Trait Segment

The segment definition:

```typescript
{
  id: "seg_pro_users",
  definition: {
    entryNode: {
      type: "Trait",
      path: "plan",
      operator: "equals",
      value: "pro"
    }
  }
}
```

SQL generated for Trait segment (from `computePropertiesIncremental.ts:1793`):

```sql
-- Filter identify events for the property path
INSERT INTO computed_property_state_v3
SELECT
  workspace_id,
  'segment',
  'seg_pro_users',
  'plan_equals_pro',  -- state_id
  user_id,
  argMaxState(properties['plan'], event_time),  -- latest value
  uniqState(message_id),                    -- count
  event_time,
  groupArrayState(message_id),
  now()
FROM user_events_v2
WHERE event_type = 'identify'
  AND workspace_id = 'ws_123'
  AND JSONExtractString(properties, 'plan') = 'pro'
GROUP BY workspace_id, user_id;
```

### Step 4: Check Segment Value

```sql
-- Query current assignments
SELECT user_id, segment_value
FROM computed_property_assignments_v2
WHERE computed_property_id = 'seg_pro_users'
  AND user_id = 'user_456';

-- Result: segment_value = true (user now in segment)
```

### Step 5: Journey Triggered (If Subscribed)

If a journey subscribes to this segment:

```typescript
{
  name: "Welcome Pro Flow",
  entryNode: {
    type: "SegmentEntryNode",
    segment: "seg_pro_users"
  }
}
```

When user enters segment:

```typescript
// From computePropertiesIncremental.ts:3675
await triggerSegmentEntryJourney({
  workspaceId: 'ws_123',
  segmentId: 'seg_pro_users',
  segmentDefinition: segment,
  segmentAssignment: assignment,
  journey: journey
});
```

---

### Complete Flow: Property-Based Segment Exit

When user sends new identify with different property value:

```javascript
// User downgrades
Analytics.identify("user_456", {
  plan: "free"
});
```

Worker runs again:

1. New `identify` event with `plan: "free"` 
2. Query shows `plan` no longer equals "pro"
3. Updates `segment_value = false` (exit)
4. Journey exit trigger fires (if subscribed)

---

### How It Works (Summary)

```
Identify Event → user_events_v2 → Worker checks Trait condition → 
segment_value = true/false → processAssignments → 
Journey trigger (if value changed)
```

The key SQL filter (from code line 1793):
```sql
event_type == 'identify'
```

---

## Code Verification Notes

This section confirms specific claims against actual Dittofeed source code:

### ✅ Verified Claims

| Claim | Code Location | Verified |
|-------|-------------|----------|
| MV tracks `computed_property_state_v3` changes | clickhouse.ts:159-176 | ✅ |
| Expire query uses `NOT IN` pattern | computePropertiesIncremental.ts:687-727 | ✅ |
| `AggregatingMergeTree` stores intermediate state | clickhouse.ts:131-157 | ✅ |
| ReplacingMergeTree for final assignments | clickhouse.ts:380-396 | ✅ |
| TTL 24h on change tracker table | clickhouse.ts:432-443 | ✅ |
| Kafka mode is optional | clickhouse.ts:607-657 | ✅ |
| Three-phase compute (state → assignments → process) | computeProperties.ts:131-133 | ✅ |
| Segment entry/exit triggers | various `*Journey*` files | ✅ |

### Key Files Referenced

- `packages/backend-lib/src/userEvents/clickhouse.ts` — Table definitions
- `packages/backend-lib/src/computedProperties/computePropertiesIncremental.ts` — Core compute logic
- `packages/backend-lib/src/computedProperties/computePropertiesWorkflow/activities/computeProperties.ts` — Worker entry point
- `packages/isomorphic-lib/src/types.ts` — Segment node type definitions

---

## References

- Main repo: https://github.com/dittofeed/dittofeed
- Tutorial: https://github.com/dittofeed/clickhouse-segments-tutorial
- Blog: https://altinity.com/blog/how-we-stopped-our-clickhouse-db-from-getting-crushed

---

## Appendix: Full Table List

| Table | Engine | Key Columns |
|-------|--------|-------------|
| `user_events_v2` | MergeTree | workspace_id, user_id, event, event_time |
| `computed_property_state_v3` | AggregatingMergeTree | workspace_id, type, computed_property_id, user_id |
| `computed_property_assignments_v2` | ReplacingMergeTree | workspace_id, type, computed_property_id, user_id |
| `updated_computed_property_state` | MergeTree | workspace_id, computed_property_id (TTL 24h) |
| `updated_property_assignments_v2` | MergeTree | workspace_id, computed_property_id (TTL 24h) |
| `resolved_segment_state` | ReplacingMergeTree | workspace_id, segment_id, user_id |
| `internal_events` | MergeTree | workspace_id, template_id, broadcast_id |
| `group_user_assignments` | ReplacingMergeTree | workspace_id, group_id, user_id |
| `user_group_assignments` | ReplacingMergeTree | workspace_id, user_id, group_id |
| `user_property_idx_num` | ReplacingMergeTree | workspace_id, computed_property_id, user_id, value_num |
| `user_property_idx_str` | ReplacingMergeTree | workspace_id, computed_property_id, user_id, value_str |
| `user_property_idx_date` | ReplacingMergeTree | workspace_id, computed_property_id, user_id, value_date |

---

## Engine Type Summary

| Engine | Purpose | Key Feature | Use Case |
|--------|---------|-------------|----------|
| **MergeTree** | Standard table | High-speed inserts, background merging | Raw events, change tracking |
| **AggregatingMergeTree** | Aggregated table | Auto-merges AggregateFunction state | Intermediate segment state |
| **ReplacingMergeTree** | Deduplicated table | Keeps latest row per key | Final assignments, resolved state |
| **Kafka** | Consumer | Reads from Kafka topic | High-throughput event ingestion |

### When to Use Each

| Data Type | Write Pattern | Recommended Engine |
|-----------|--------------|-------------------|
| **Immutable events** | Append once, never update | MergeTree |
| **Accumulative state** | Incrementally update counts/latest | AggregatingMergeTree |
| **Current state** | Latest value per entity | ReplacingMergeTree |
| **Async ingestion** | Buffer via Kafka | Kafka + MV to MergeTree |