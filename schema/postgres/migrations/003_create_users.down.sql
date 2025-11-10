drop index if exists idx_users_project_id;
drop index if exists idx_users_external_id;
drop index if exists idx_users_project_external;
drop trigger if exists update_timestamp on users;
drop table users;
