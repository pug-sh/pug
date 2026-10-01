-- name: WriteAheadBillingMeterPeriod :exec
-- Before the ingest: the row claims the statement, un-acked, so it can never
-- understate what the provider holds. window_start is kept from the first write:
-- a period's window starts once. The counts are overwritten, not maxed: the pass's
-- advisory lock is what makes the max it took against this row still hold.
insert into billing_meter_periods (
  acked, allowance, carry_events, org_id, own_events, period_start, plan_slug,
  provider_customer_id, provider_sub_id, stated_at, summed_through, window_end, window_start
) values (
  false, @allowance, @carry_events::bigint[], @org_id, @own_events::bigint[], @period_start,
  @plan_slug, @provider_customer_id, @provider_sub_id, @stated_at, @summed_through,
  @window_end, @window_start
)
on conflict (org_id, period_start) do update
set acked = false,
    allowance = excluded.allowance,
    carry_events = excluded.carry_events,
    own_events = excluded.own_events,
    plan_slug = excluded.plan_slug,
    provider_customer_id = excluded.provider_customer_id,
    provider_sub_id = excluded.provider_sub_id,
    stated_at = excluded.stated_at,
    summed_through = least(greatest(billing_meter_periods.summed_through, excluded.summed_through),
      excluded.window_end),
    window_end = excluded.window_end;

-- name: AdvanceBillingMeterPeriodSummedThrough :exec
-- A tick that stated nothing still saw the subscription live. Moves forward only,
-- and writes at most once a UTC day, since that is how often the value changes.
-- Capped at the row's own window_end, which only a statement moves: a period whose
-- end the provider pushed out since then is summed past it, and the row's CHECK
-- would fail the org on every quiet tick.
update billing_meter_periods set summed_through = least(@summed_through::date, window_end)
where org_id = @org_id and period_start = @period_start
  and summed_through < least(@summed_through::date, window_end);

-- name: AckBillingMeterPeriod :execrows
-- After the ingest. Matched on stated_at, so an ack confirms only the statement it
-- followed.
update billing_meter_periods set acked = true
where org_id = @org_id and period_start = @period_start and stated_at = @stated_at;
