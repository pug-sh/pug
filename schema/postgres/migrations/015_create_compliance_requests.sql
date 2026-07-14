-- +goose Up
-- compliance_requests is the unified DSAR ledger for GDPR/DPDP data-subject
-- rights (roadmap §4). Each row records one request — erasure (§4.1) or export
-- (§4.2) — discriminated by `kind`. A row is created synchronously when the
-- request is made; for erasure the worker then freezes the resolved identifiers,
-- performs the hard deletes, and marks it completed. Rows are retained after the
-- subject is gone as proof of what was done and when.
create table compliance_requests (
  id              char(20)    primary key,
  project_id      char(20)    not null references projects(id) on delete cascade,
  -- 'erase' (§4.1) or 'export' (§4.2).
  kind            text        not null check (kind in ('erase', 'export')),
  -- Resolved subject. Deliberately NOT a foreign key: an erasure hard-deletes the
  -- profile, and the audit row must outlive it. NULL when the request targets an
  -- external_id with no matching profile row.
  profile_id      char(20),
  external_id     text,
  status          text        not null default 'pending'
                    check (status in ('pending', 'processing', 'completed', 'failed')),
  -- Erase-only: frozen on the first worker pass so retries stay correct after
  -- events are deleted (session_ids are otherwise unrecoverable). Also the audit
  -- record of "what". NULL for export.
  distinct_ids    text[],
  session_ids     text[],
  -- Events erased (erase) or exported (export), disambiguated by `kind`. For erase
  -- this is a PRE-delete snapshot counted at freeze time (no FINAL), so it can exceed
  -- the rows physically removed by the un-merged ReplacingMergeTree duplicate rate —
  -- an "identified" count, not a reconciled "deleted" count.
  events_affected bigint      not null default 0,
  -- Accountability: customer id for JWT callers, NULL for private-API-key callers.
  requested_by    char(20),
  requested_at    timestamptz not null default now(),
  completed_at    timestamptz,
  update_time     timestamptz not null default now(),
  error           text,
  -- A completed request must record when it completed: the row is the proof of
  -- fulfilment, so "completed" without a timestamp is not a valid state.
  constraint compliance_requests_completed_at_check
    check (status <> 'completed' or completed_at is not null),
  -- A request must identify a subject: at least one of profile_id / external_id.
  -- The worker also detects an empty identifier set (ErrNoErasableIdentifiers), but
  -- only on the async pass, *after* the prelude has committed the row and soft-deleted
  -- the profile. This makes the unidentifiable request unrepresentable at the storage
  -- layer, demoting the runtime check to a defensive backstop.
  constraint compliance_requests_identifier_present
    check (profile_id is not null or external_id is not null)
);

create index compliance_requests_project_kind_status on compliance_requests (project_id, kind, status);

-- First-time-request dedup: at most one open (pending/processing) request per subject
-- per kind, so two concurrent first-time requests for the same subject cannot both
-- insert a ledger row (the sequential case is already caught by
-- GetReopenableComplianceRequest; this closes the concurrent race). Two indexes because
-- a subject is identified by either column and a request usually carries both.
-- 'completed'/'failed' rows are excluded so a genuinely new erasure (data re-arrived
-- under the same id) or a revived failed row can re-enter the open set. The prelude
-- catches the unique violation and re-drives the existing row via reopenErasure instead
-- of surfacing an error.
create unique index compliance_requests_open_profile_uniq
  on compliance_requests (project_id, kind, profile_id)
  where status in ('pending', 'processing') and profile_id is not null;
create unique index compliance_requests_open_external_uniq
  on compliance_requests (project_id, kind, external_id)
  where status in ('pending', 'processing') and external_id is not null;

create trigger update_timestamp before
update on compliance_requests for each row execute procedure moddatetime(update_time);

-- +goose Down
drop table if exists compliance_requests;
