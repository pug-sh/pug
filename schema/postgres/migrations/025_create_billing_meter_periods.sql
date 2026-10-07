-- +goose Up
-- What pug has stated to the provider's usage meters, per org and provider
-- period. The meter pass writes a row BEFORE it ingests and acknowledges it after,
-- so a row never understates what the provider holds: the next period's carry is
-- computed against it, and the dashboard shows it. One writer:
-- internal/core/billing/meter.
create table billing_meter_periods (
  acked boolean not null default false,
  allowance bigint not null
    constraint billing_meter_periods_allowance_check check (allowance >= 0),
  -- One count per tier, never empty, NULL or negative. `0 <= all(...)` alone would
  -- pass a NULL element, since the comparison is then NULL rather than false.
  carry_events bigint[] not null
    constraint billing_meter_periods_carry_check
      check (cardinality(carry_events) >= 1 and array_position(carry_events, null) is null
        and 0 <= all(carry_events)),
  create_time timestamptz not null default now(),
  org_id char(20) not null references orgs(id) on delete cascade,
  own_events bigint[] not null
    constraint billing_meter_periods_own_check
      check (cardinality(own_events) >= 1 and array_position(own_events, null) is null
        and 0 <= all(own_events)),
  period_start timestamptz not null,
  plan_slug varchar(50) not null
    constraint billing_meter_periods_plan_slug_check check (plan_slug <> ''),
  provider_customer_id text not null
    constraint billing_meter_periods_customer_check check (provider_customer_id <> ''),
  -- The subscription the period belongs to. A successor of the same subscription is
  -- a renewal, which follows the whole window; any other follows summed_through.
  provider_sub_id text not null
    constraint billing_meter_periods_sub_check check (provider_sub_id <> ''),
  stated_at timestamptz not null,
  -- Exclusive, like window_end: the window's days [window_start, summed_through)
  -- were summed while the subscription was live, as of the last tick that saw it.
  summed_through date not null,
  update_time timestamptz not null default now(),
  -- Exclusive: the window is the whole UTC days [window_start, window_end).
  window_end date not null,
  window_start date not null,

  primary key (org_id, period_start),
  constraint billing_meter_periods_window_check
    check (window_start <= summed_through and summed_through <= window_end),
  constraint billing_meter_periods_tiers_check
    check (cardinality(own_events) = cardinality(carry_events))
);

create trigger update_timestamp before
update on billing_meter_periods for each row execute procedure moddatetime(update_time);

-- +goose Down
drop table if exists billing_meter_periods;
