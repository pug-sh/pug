-- +goose Up
alter table customers add column disabled_at timestamptz;
alter table customers add column session_version bigint not null default 0;

-- +goose Down
alter table customers drop column session_version;
alter table customers drop column disabled_at;
