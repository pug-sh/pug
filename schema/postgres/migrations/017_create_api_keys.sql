-- +goose Up
-- API keys move off the projects row into a first-class table: many keys per
-- project, explicitly created and deletable, so a leaked key can be rotated.
--
-- A private key is stored only as a sha256 hash (never plaintext), exactly like
-- email_action_tokens and refresh_tokens — a leak of this table must not hand an
-- attacker working credentials. A public key is embedded in client apps and is
-- extractable by design, so hashing it would buy nothing and would cost the
-- ability to show it back on the project settings page: it stays plaintext.
--
-- projects.private_api_key / public_api_key are dropped here, in this same
-- migration, rather than kept alive for a release. That buys the absence of a
-- transitional dual-read — no *Legacy fallback lookups, no re-runnable backfill,
-- no marker value standing in for a column that must hold something, no phase 2 —
-- at the cost of a deploy window: a pod still running the previous image selects
-- these columns and fails until it rolls. Deliberate at current traffic (one
-- project, no signups mid-deploy). If a rollout ever has to be seamless, the
-- shape to reach for is dual-write + fallback-read + a second migration to drop,
-- and it is a lot more code than this.
create table api_keys (
  create_time timestamptz not null default now(),
  display_name varchar(150) not null default '',
  id char(20) primary key,
  kind varchar(10) not null constraint api_keys_kind_check check (kind in ('public', 'private')),
  -- Display hint ("prv_...3f9c") for the settings page: a private key is
  -- unrecoverable once created, so this is all we can ever show of it again.
  masked varchar(24) not null,
  project_id char(20) not null references projects(id) on delete cascade,
  -- What auth looks a presented key up by: the key itself for a public key, its
  -- sha256 hex digest for a private key. Unique across projects, so a lookup
  -- needs no project scope.
  token varchar(64) not null unique,
  update_time timestamptz not null default now()
);

-- Supports listing a project's keys and the on-delete cascade.
create index api_keys_project_id_idx on api_keys (project_id);

create trigger update_timestamp before
update on api_keys for each row execute procedure moddatetime(update_time);

-- Carry every existing key pair over, hashing the private key so its plaintext
-- never lands in this table. sha256() is built in (PG11+); there is no cast from
-- char/text to bytea, hence convert_to(). This runs exactly once — the columns are
-- gone by the end of this migration, so there is nothing to catch up later and no
-- re-run to stay correct against.
--
-- The prefix guards are input validation, not transitional machinery: every value
-- in these columns was written by the previous image as a real key, but a row that
-- somehow holds something else must be skipped rather than minted into a working
-- credential nobody created.
--
-- Ids derive from the project id (there is no xid generator in SQL, and this is
-- unique per project per kind). create_time is inherited from the project — that
-- is when these keys really came into existence, and the settings page orders by
-- it.
insert into api_keys (create_time, id, kind, masked, project_id, token)
select
  p.create_time,
  left(encode(sha256(convert_to(p.id || ':public', 'UTF8')), 'hex'), 20),
  'public',
  left(p.public_api_key, 4) || '...' || right(p.public_api_key, 4),
  p.id,
  p.public_api_key
from projects p
where left(p.public_api_key, 4) = 'pub_'
union all
select
  p.create_time,
  left(encode(sha256(convert_to(p.id || ':private', 'UTF8')), 'hex'), 20),
  'private',
  left(p.private_api_key, 4) || '...' || right(p.private_api_key, 4),
  p.id,
  encode(sha256(convert_to(p.private_api_key, 'UTF8')), 'hex')
from projects p
where left(p.private_api_key, 4) = 'prv_';

alter table projects
  drop column private_api_key,
  drop column public_api_key;

-- +goose Down
-- Lossy, and only meaningful paired with an image rollback. The public key is
-- restored from api_keys. The private key cannot be — only its digest was ever
-- stored — so the column comes back holding the project id, which carries no
-- "prv_" prefix and therefore authenticates nothing: a project that had a private
-- key before this migration needs a new one issued after a rollback. A project
-- with no public key in api_keys gets its id there too, for the same reason — the
-- columns are not null unique and must hold something unique.
alter table projects
  add column private_api_key char(24),
  add column public_api_key char(24);

update projects p
set private_api_key = p.id,
    public_api_key = coalesce(
      (select k.token from api_keys k
       where k.project_id = p.id and k.kind = 'public'
       order by k.create_time asc
       limit 1),
      p.id);

alter table projects
  alter column private_api_key set not null,
  alter column public_api_key set not null;

alter table projects
  add constraint projects_private_api_key_key unique (private_api_key),
  add constraint projects_public_api_key_key unique (public_api_key);

drop table api_keys;
