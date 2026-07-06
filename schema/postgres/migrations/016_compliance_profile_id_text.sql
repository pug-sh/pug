-- +goose Up
-- compliance_requests.profile_id was char(20), sized for xid-generated profile
-- ids. Erasure-by-id now also accepts derived anonymous person ids, which ARE
-- events distinct_ids (e.g. a 41-char anon-<uuid>) with no length bound. bpchar
-- additionally space-pads shorter values, which corrupts the frozen identifier
-- fan-out (the padded id matches no events row in ClickHouse). text removes
-- both failure modes; the bpchar->text cast strips padding from existing rows.
alter table compliance_requests alter column profile_id type text;

-- +goose Down
-- Intentionally a no-op. text is a superset of char(20), so the widening loses
-- no data and needs no structural revert. The literal reverse (type char(20))
-- is unsafe: it ERRORs ("value too long for type character(20)") on any
-- profile_id a post-widening erasure recorded that exceeds 20 chars — e.g. a
-- 41-char anon-<uuid> — which would make `goose down` past this migration
-- un-runnable on real data. Leaving the column as text keeps rollback safe;
-- re-narrowing is deliberately not offered.
select 1;
