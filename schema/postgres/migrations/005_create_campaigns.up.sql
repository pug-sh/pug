create table campaigns (
  id char(20) primary key,
  customer_id char(20) not null references customers(id) on delete cascade,
  project_id char(20) not null references projects(id) on delete cascade,
  title varchar(255) not null,
  create_time timestamptz not null default now(),
  update_time timestamptz not null default now()
);
create trigger update_timestamp before update on campaigns for each row execute procedure moddatetime(update_time);