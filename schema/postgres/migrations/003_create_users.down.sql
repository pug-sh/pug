drop index if exists idx_users_project_id;
drop index if exists idx_users_external_id;
drop index if exists idx_users_project_external;
drop index if exists idx_users_segments;
drop index if exists idx_users_metadata;
drop trigger if exists update_timestamp on users;
drop table users;
