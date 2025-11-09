create table users (
  id char(20) primary key,
  customer_id char(20) not null references customers(id),
  email varchar(255) unique not null,
  password_hash varchar(255) not null,
  create_time timestamptz not null default now(),
  update_time timestamptz not null default now(),
  unique (customer_id, email)
);
create trigger update_timestamp before update on users for each row execute procedure moddatetime(update_time);