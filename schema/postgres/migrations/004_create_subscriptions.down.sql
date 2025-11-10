drop index if exists idx_subscriptions_project_status_platform;
drop index if exists idx_subscriptions_user_id;
drop index if exists idx_subscriptions_project_user;
drop index if exists idx_subscriptions_project_user_status;
drop trigger if exists update_timestamp on subscriptions;
drop table subscriptions;