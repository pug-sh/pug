-- +goose Up
-- Billing entitlement (docs/architecture/billing.md): what an org is allowed to
-- send, and nothing else. No provider, no payments, no enforcement -- ingestion
-- never reads this, so a wrong row costs a wrong banner and never an event.
--
-- Signup does NOT write here. An org with no row is the normal state: its
-- entitlement derives from orgs.create_time (trial for 14 days, then free), so
-- org creation cannot fail on a billing table and an empty table behaves exactly
-- like a fresh one.

create table billing_entitlements (
  -- Day of month the org's quota window starts on. NULL means orgs.create_time's
  -- day, which is every org today. A day number rather than a date or an instant:
  -- the meter's period sum is exact only for midnight-aligned windows, so a
  -- period must not be able to start at 14:32.
  anchor_day smallint
    constraint billing_entitlements_anchor_day_check
      check (anchor_day is null or anchor_day between 1 and 31),
  -- When a granted plan lapses back to the floor. NULL is open-ended. This is the
  -- end of the DEAL, not of a quota window -- an annual contract ending in March
  -- does not make March's window a year long.
  contract_ends_at timestamptz,
  create_time timestamptz not null default now(),
  -- NO price or currency column, deliberately (docs/architecture/payments.md
  -- section 4): the amount a deal is charged lives in the payments provider,
  -- which is the only thing that can charge it. A copy here would be a second
  -- authority that goes stale the first time a deal is repriced. What the deal
  -- was agreed at goes in note, which is honest about being a record.
  display_name_override varchar(150),
  -- The negotiated quota. NULL means "use the plan's". Checked > 0 because 0
  -- would read as a quota of zero rather than as "no override"; unlimited is
  -- deliberately not expressible.
  included_events_override bigint
    constraint billing_entitlements_override_check
      check (included_events_override is null or included_events_override > 0),
  -- The operator's record of why. Never returned by any RPC.
  note text not null default '',
  -- The org is the primary key: one entitlement per org is then structural
  -- rather than a unique constraint somebody has to remember.
  org_id char(20) primary key references orgs(id) on delete cascade,
  -- Deliberately NOT constrained to a list of slugs. The catalog is Go (see
  -- plans.go) and SetPlan already rejects a slug it does not know, so a list here
  -- would only be a second copy that has to be migrated in lockstep -- and the
  -- documented reprice workflow, which mints a new slug, would fail against the
  -- stale copy with a raw SQLSTATE.
  plan_slug varchar(50) not null,
  -- Set only by extend-trial. NULL means the trial window derives from
  -- orgs.create_time, which is the ordinary case.
  trial_ends_at timestamptz,
  update_time timestamptz not null default now(),
  -- A custom plan has no catalog quota to fall back on, so the deal is
  -- unrepresentable without this. Enforced here rather than in the CLI: the row
  -- is what every read trusts.
  constraint billing_entitlements_custom_needs_quota
    check (plan_slug <> 'custom' or included_events_override is not null)
);

create trigger update_timestamp before
update on billing_entitlements for each row execute procedure moddatetime(update_time);

-- Append-only history. The entitlement row is mutated in place, so on its own it
-- can only answer "what is true now"; this answers "what was true then, and who
-- did it" -- the question actually asked about a commercial agreement, months
-- later and usually in a dispute. Snapshots rather than diffs: one row is the
-- complete answer, with no replay and no dependence on earlier rows surviving.
create table billing_entitlement_history (
  -- Who made the change. Required: an unattributed change to a commercial
  -- agreement is barely better than no record.
  actor varchar(150) not null,
  anchor_day smallint,
  changed_at timestamptz not null default now(),
  contract_ends_at timestamptz,
  display_name_override varchar(150),
  id char(20) primary key,
  included_events_override bigint,
  note text not null default '',
  -- Deliberately NO foreign key to orgs: billing_entitlements cascades away with
  -- the org, and "what were they on when they left" is precisely the question
  -- asked after a deletion, usually in a refund dispute.
  org_id char(20) not null,
  -- NULL across the value columns is a deletion: the org returned to the derived
  -- floors.
  plan_slug varchar(50),
  trial_ends_at timestamptz
);

create index billing_entitlement_history_org_idx
  on billing_entitlement_history (org_id, changed_at desc);

-- +goose Down
drop table if exists billing_entitlement_history;
drop table if exists billing_entitlements;
