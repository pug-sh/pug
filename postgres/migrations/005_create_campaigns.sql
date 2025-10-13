-- Write your migrate up statements here
create table campaigns (
    create_time timestamptz not null default now(),
    end_time timestamptz,
    id char(20) primary key,
    notification_data jsonb not null,
    project_id char(20) not null references projects(id) on delete cascade,
    scheduled_time timestamptz not null default now(),
    start_time timestamptz,
    status text not null default 'scheduled' check (
        status in ('complete', 'fail', 'in-progress', 'scheduled')
    ),
    update_time timestamptz not null default now()
);
create trigger update_timestamp before
update on campaigns for each row execute procedure moddatetime(update_time);
create index idx_campaigns_project_id on campaigns (project_id);
create index idx_campaigns_scheduled_time on campaigns (scheduled_time);
create index idx_campaigns_status on campaigns (status);
---- create above / drop below ----
-- Write your migrate down statements here. If this migration is irreversible
-- Then delete the separator line above.
drop table campaigns;
