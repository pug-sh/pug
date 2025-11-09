create table subscriptions (
  id char(20) primary key,
  customer_id char(20) not null references customers(id) on delete cascade,
  stripe_customer_id varchar(255) unique,
  stripe_subscription_id varchar(255) unique,
  stripe_price_id varchar(255),
  create_time timestamptz not null default now(),
  update_time timestamptz not null default now(),
  valid_from timestamptz not null,
  valid_to timestamptz
);
create trigger update_timestamp before update on subscriptions for each row execute procedure moddatetime(update_time);