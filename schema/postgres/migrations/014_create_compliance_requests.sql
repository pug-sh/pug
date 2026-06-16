-- +goose Up
-- compliance_requests is the unified DSAR ledger for GDPR/DPDP data-subject
-- rights (roadmap §4). Each row records one request — erasure (§4.1) or export
-- (§4.2) — discriminated by `kind`. A row is created synchronously when the
-- request is made; for erasure the worker then freezes the resolved identifiers,
-- performs the hard deletes, and marks it completed. Rows are retained after the
-- subject is gone as proof of what was done and when. See
-- docs/compliance/4.1-erasure-scope.md and docs/compliance/4.2-export-scope.md.
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
  -- Events erased (erase) or exported (export), disambiguated by `kind`.
  events_affected bigint      not null default 0,
  -- Accountability: customer id for JWT callers, NULL for private-API-key callers.
  requested_by    char(20),
  requested_at    timestamptz not null default now(),
  completed_at    timestamptz,
  update_time     timestamptz not null default now(),
  error           text
);

create index compliance_requests_project_kind_status on compliance_requests (project_id, kind, status);

create trigger update_timestamp before
update on compliance_requests for each row execute procedure moddatetime(update_time);

-- +goose Down
drop table if exists compliance_requests;
