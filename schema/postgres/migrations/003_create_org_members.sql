-- +goose Up
create table org_members (
  create_time timestamptz not null default now(),
  customer_id char(20) not null references customers(id) on delete cascade,
  org_id char(20) not null references orgs(id) on delete cascade,
  role varchar(30) not null default 'ORG_ROLE_MEMBER',
  primary key (org_id, customer_id)
);


-- +goose Down
drop table org_members;
