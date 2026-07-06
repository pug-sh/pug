-- +goose Up
-- compliance_requests.profile_id was char(20), sized for xid-generated profile
-- ids. Erasure-by-id now also accepts derived anonymous person ids, which ARE
-- events distinct_ids (e.g. a 41-char anon-<uuid>) with no length bound. bpchar
-- additionally space-pads shorter values, which corrupts the frozen identifier
-- fan-out (the padded id matches no events row in ClickHouse). text removes
-- both failure modes; the bpchar->text cast strips padding from existing rows.
alter table compliance_requests alter column profile_id type text;

-- +goose Down
alter table compliance_requests alter column profile_id type char(20);
