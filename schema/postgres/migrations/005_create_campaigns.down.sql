drop index if exists idx_campaigns_project_id;
drop index if exists idx_campaigns_scheduled_time;
drop index if exists idx_campaigns_status;
drop trigger if exists update_timestamp on campaigns;
drop table campaigns;