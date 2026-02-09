-- +goose Up
create or replace function jsonb_shallow_merge(a jsonb, b jsonb) returns jsonb as $$
select case
  when a is null then b
  when b is null then a
  when jsonb_typeof(a) = 'object' and jsonb_typeof(b) = 'object' then
    (select jsonb_object_agg(
      coalesce(ka, kb),
      coalesce(vb, va)
    )
    from jsonb_each(a) as ta(ka, va)
    full outer join jsonb_each(b) as tb(kb, vb) on ka = kb)
  else b
end
$$ language sql immutable;

-- +goose Down
drop function if exists jsonb_shallow_merge;
