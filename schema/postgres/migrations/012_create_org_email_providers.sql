-- +goose Up
create table org_email_providers (
  org_id            char(20)     primary key references orgs(id) on delete cascade,
  kind              varchar(64)  not null check (kind in ('ORG_EMAIL_PROVIDER_KIND_SMTP', 'ORG_EMAIL_PROVIDER_KIND_RESEND')),
  from_address      varchar(255) not null,
  reply_to          varchar(255) null,
  secret_ciphertext bytea        not null,
  create_time       timestamptz  not null default now(),
  update_time       timestamptz  not null default now()
);

create trigger update_timestamp before
update on org_email_providers for each row execute procedure moddatetime(update_time);

-- +goose Down
drop table org_email_providers;
