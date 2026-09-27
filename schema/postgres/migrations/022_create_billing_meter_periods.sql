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
  carry_events bigint[] not null,
  create_time timestamptz not null default now(),
  org_id char(20) not null references orgs(id) on delete cascade,
  own_events bigint[] not null,
  period_start timestamptz not null,
  plan_slug varchar(50) not null
    constraint billing_meter_periods_plan_slug_check check (plan_slug <> ''),
  provider_customer_id text not null
    constraint billing_meter_periods_customer_check check (provider_customer_id <> ''),
  stated_at timestamptz not null,
  update_time timestamptz not null default now(),
  window_end date not null,
  window_start date not null,

  primary key (org_id, period_start),
  constraint billing_meter_periods_window_check check (window_start <= window_end),
  constraint billing_meter_periods_tiers_check
    check (cardinality(own_events) = cardinality(carry_events))
);

create trigger update_timestamp before
update on billing_meter_periods for each row execute procedure moddatetime(update_time);

-- +goose Down
drop table if exists billing_meter_periods;
