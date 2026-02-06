-- +goose Up

-- Rename users table to profiles
alter table users rename to profiles;

-- Rename users indexes
alter index idx_users_properties rename to idx_profiles_properties;
alter index idx_users_custom_properties rename to idx_profiles_custom_properties;

-- Rename trigger
alter trigger update_timestamp on profiles rename to update_timestamp;

-- Rename user_id to profile_id in subscriptions
alter table subscriptions rename column user_id to profile_id;

-- Rename subscriptions indexes that reference user_id
alter index idx_subscriptions_user_id rename to idx_subscriptions_profile_id;
alter index idx_subscriptions_project_user rename to idx_subscriptions_project_profile;
alter index idx_subscriptions_project_user_status rename to idx_subscriptions_project_profile_status;

-- Rename FK constraint on subscriptions.profile_id
alter table subscriptions rename constraint subscriptions_user_id_fkey to subscriptions_profile_id_fkey;

-- +goose Down

-- Reverse FK constraint rename
alter table subscriptions rename constraint subscriptions_profile_id_fkey to subscriptions_user_id_fkey;

-- Reverse subscriptions index renames
alter index idx_subscriptions_profile_id rename to idx_subscriptions_user_id;
alter index idx_subscriptions_project_profile rename to idx_subscriptions_project_user;
alter index idx_subscriptions_project_profile_status rename to idx_subscriptions_project_user_status;

-- Reverse column rename
alter table subscriptions rename column profile_id to user_id;

-- Reverse index renames
alter index idx_profiles_properties rename to idx_users_properties;
alter index idx_profiles_custom_properties rename to idx_users_custom_properties;

-- Reverse table rename
alter table profiles rename to users;
