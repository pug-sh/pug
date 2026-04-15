# Dittofeed Segmentation — Quick Start

This doc gives you the **essential understanding** in ~15 minutes. 

**Goal:** Understand the flow, not the implementation details.

> **No ClickHouse experience needed** — This doc explains what you need as you go.

---

## What is ClickHouse? (Just Enough)

ClickHouse is a **database optimized for analytics**. Think of it as:

| Your SQL DB | ClickHouse |
|------------|-----------| | Rows → millions/billions |
| Queries → simple | Queries → complex aggregations |
| Updates → common | Updates → rare (append-mostly) |
| Real-time ← | Near real-time → |

**The only thing you need to know:**

> ClickHouse is incredibly fast at counting/aggregating billions of rows, but it works best when you **append data** rather than constantly **update** it.

---

## The Problem

### ❌ Naive Approach (Bad)

```sql
-- Scans ALL events every request
SELECT user_id
FROM user_events
WHERE event = 'button_click'
GROUP BY user_id
HAVING count() >= 3;
```

This works at first but becomes slow as events grow.

### ✅ Dittofeed Approach

1. **Track changes** — Not events, but what changed
2. **Process incrementally** — Only changed segments
3. **Store results** — Separate table for fast reads

---

## The Flow (Memorize This)

```
┌────────────┐     ┌─────────────┐     ┌──────────────┐
│   User     │────▶│  Events    │────▶│ ClickHouse  │
│  does      │     │  API       │     │  inserts   │
│  action    │     │            │     │            │
└────────────┘     └─────────────┘     └──────┬───────┘
                                         │
                                         ▼
                              ┌────────────────────┐
                              │ user_events_v2     │
                              │ (raw events)       │
                              └─────────┬──────────┘
                                        │
                                        ▼
                              ┌────────────────────┐
                              │ Worker runs every  │
                              │ 30 seconds         │
                              │ - What changed?    │
                              │ - Recompute those  │
                              └─────────┬──────────┘
                                        │
                                        ▼
                              ┌────────────────────┐
                              │ segment_assignments │
                              │ (user → segment)    │
                              └────────────────────┘
                                        │
                                        ▼
                              ┌────────────────────┐
                              │ Journey triggers   │
                              └────────────────────┘
```

**Key insight:** Worker processes **only changed properties**, not all events.

---

## Three Tables to Know

```
user_events_v2              → Raw events
       │
       ▼ Worker computes
computed_property_state_v3   → Intermediate (aggregated counts)
       │
       ▼ Worker writes result  
computed_property_assignments → Final results
```

| Table | What | Why | Engine (ignore for now) |
|-------|------|-----|--------------------------|
| `user_events_v2` | Raw events | Append-only, never updated | MergeTree |
| `computed_property_state_v3` | Counts/latest | Auto-merges when data combines | AggregatingMergeTree |
| `computed_property_assignments` | Current status | Keep newest, discard old | ReplacingMergeTree |

> **Engine types:** Think of them like different storage strategies. Don't worry about the details yet — just know each table has a purpose.

### When to Use Each Engine (Simple)

**MergeTree** — For events (append & forget)
- You write, never touch again

**AggregatingMergeTree** — For counts that grow
- First insert: count=1
- Second insert: auto-merges → count=2 ✨

**ReplacingMergeTree** — For current state
- New insert overwrites old
- Always keep latest

---

## Entry Example: User Clicks 3 Times

```javascript
// 1. User clicks button 3 times
Analytics.track('button_click'); // x3
```

```
Events stored → Worker runs → User meets condition (3>=3) → segment_value=true
```

**What worker does:**

```sql
-- 1. Find changed since last run
SELECT user_id, count() as clicks
FROM user_events_v2
WHERE event = 'button_click'
  AND event_time >= now() - INTERVAL 30 SECOND  -- changed in last run
GROUP BY user_id
HAVING count() >= 3;

-- 2. Write true/false to assignments
INSERT INTO computed_property_assignments_v2
(user_id, segment_value) VALUES ('user_A', true);
```

---

## Exit Example: User Stops Clicking

**Key insight:** Exit is "who was TRUE but no longer qualifies"

```sql
-- Find users TRUE but NOT in qualifying set anymore
INSERT INTO computed_property_assignments_v2
SELECT user_id, false
FROM computed_property_assignments_v2   -- currently TRUE
WHERE segment = 'power_users' = true
  AND user_id NOT IN (
    SELECT user_id FROM user_events_v2     -- no longer qualifies
    WHERE event = 'button_click'
    GROUP BY user_id HAVING count() >= 3
  );
```

**Why efficient:** Only checks TRUE users, not all users.

---

## Journey Triggers

Any segment type can trigger journeys:

| Segment Type | Entry | Exit |
|-------------|-------|------|
| Event count | ✅ | ✅ |
| User property | ✅ | ✅ |
| Manual | ✅ | ✅ |
| Everyone | ✅ | — |

```javascript
// Journey definition
{
  name: "Welcome Pro",
  entryNode: { type: "SegmentEntryNode", segment: "seg_pro_users" }
}
```

When user enters segment → journey starts automatically.

---

## Key Terms (Simple)

| Term | Simple Meaning | When You'll Use It |
|------|----------------|-------------------|
| **Micro-batching** | Worker runs every 30 sec, not real-time | When you need near-live updates |
| **AggregatingMergeTree** | Auto-adds counts together | When tracking "how many times" |
| **ReplacingMergeTree** | New data replaces old | When you only want current value |
| **TTL** | Auto-delete old data | When old data = garbage |
| **Materialized View** | Auto-runs SQL on insert | When you want automatic updates |

### What's TTL?

"Time To Live" — like automatically throwing away old food:

```sql
TTL created_at + INTERVAL 24 HOUR  -- delete after 24 hours
```

That's it. ClickHouse handles the cleanup.

---

## Next Steps

**Want more detail?** Read:
- `dittofeed-segmentation.md` — Full documentation with SQL, code references

**Want to implement?** Start with:
- Understand these 3 tables first
- Then learn the engine types in full doc