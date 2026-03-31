-- +goose Up
create index idx_profiles_project_create_time on profiles (project_id, create_time desc, id desc);

-- +goose Down
drop index if exists idx_profiles_project_create_time;
