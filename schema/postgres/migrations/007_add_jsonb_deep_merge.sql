-- +goose Up
create or replace function jsonb_deep_merge(a jsonb, b jsonb) returns jsonb as $$
select case
  when a is null then b
  when b is null then a
  when jsonb_typeof(a) = 'object' and jsonb_typeof(b) = 'object' then
    (select jsonb_object_agg(
      coalesce(ka, kb),
      case
        when va is null then vb
        when vb is null then va
        else jsonb_deep_merge(va, vb)
      end
    )
    from jsonb_each(a) as ta(ka, va)
    full outer join jsonb_each(b) as tb(kb, vb) on ka = kb)
  else b
end
$$ language sql immutable;

-- +goose Down
drop function if exists jsonb_deep_merge;
