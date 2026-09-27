-- name: WriteAheadBillingMeterPeriod :exec
-- Before the ingest: the row claims the statement, un-acked, so it can never
-- understate what the provider holds. window_start is kept from the first write:
-- a period's window starts once.
insert into billing_meter_periods (
  acked, allowance, carry_events, org_id, own_events, period_start, plan_slug,
  provider_customer_id, stated_at, window_end, window_start
) values (
  false, @allowance, @carry_events::bigint[], @org_id, @own_events::bigint[], @period_start,
  @plan_slug, @provider_customer_id, @stated_at, @window_end, @window_start
)
on conflict (org_id, period_start) do update
set acked = false,
    allowance = excluded.allowance,
    carry_events = excluded.carry_events,
    own_events = excluded.own_events,
    plan_slug = excluded.plan_slug,
    provider_customer_id = excluded.provider_customer_id,
    stated_at = excluded.stated_at,
    window_end = excluded.window_end;

-- name: AckBillingMeterPeriod :execrows
-- After the ingest. Matched on stated_at, so an ack confirms only the statement it
-- followed.
update billing_meter_periods set acked = true
where org_id = @org_id and period_start = @period_start and stated_at = @stated_at;
