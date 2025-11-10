-- Enable moddatetime extension if not already enabled
create extension if not exists moddatetime;

-- Create journeys table for journey templates/workflows
create table journeys (
  id char(20) primary key,
  project_id char(20) not null references projects(id) on delete cascade,
  name varchar(150) not null,
  description text,
  state text not null default 'draft' check (state in ('active', 'draft', 'paused', 'archived')),
  entry_type text not null check (entry_type in ('segment', 'event')),
  config jsonb not null,
  start_time timestamptz,
  end_time timestamptz,
  create_time timestamptz not null default now(),
  update_time timestamptz not null default now()
);

create trigger update_timestamp before
update on journeys for each row execute procedure moddatetime(update_time);

create index idx_journeys_project_id on journeys (project_id);
create index idx_journeys_state on journeys (state);

-- Create journey_executions table for active journey instances for users
create table journey_executions (
  id char(20) primary key,
  journey_id char(20) not null references journeys(id) on delete cascade,
  user_id char(20) not null references users(id) on delete cascade,
  state text not null default 'active' check (state in ('active', 'completed', 'exited', 'failed')),
  entry_time timestamptz not null default now(),
  exit_time timestamptz,
  entry_trigger text not null,
  create_time timestamptz not null default now(),
  update_time timestamptz not null default now()
);

create trigger update_timestamp before
update on journey_executions for each row execute procedure moddatetime(update_time);

create index idx_journey_executions_journey_id on journey_executions (journey_id);
create index idx_journey_executions_user_id on journey_executions (user_id);
create index idx_journey_executions_state on journey_executions (state);
create index idx_journey_executions_user_journey on journey_executions (user_id, journey_id);

-- Create journey_steps table for template steps within a journey
create table journey_steps (
  id char(20) primary key,
  journey_id char(20) not null references journeys(id) on delete cascade,
  step_id varchar(50) not null,
  step_type text not null check (step_type in ('message', 'wait', 'decision', 'action', 'pause')),
  config jsonb not null,
  next_step_id varchar(50),
  create_time timestamptz not null default now(),
  update_time timestamptz not null default now(),
  unique (journey_id, step_id)
);

create trigger update_timestamp before
update on journey_steps for each row execute procedure moddatetime(update_time);

create index idx_journey_steps_journey_id on journey_steps (journey_id);

-- Create journey_user_steps table for current step execution for users
create table journey_user_steps (
  id char(20) primary key,
  journey_execution_id char(20) not null references journey_executions(id) on delete cascade,
  step_id varchar(50) not null,
  state text not null default 'pending' check (state in ('pending', 'executing', 'completed', 'failed')),
  executed_time timestamptz,
  attempt_count int not null default 0,
  create_time timestamptz not null default now(),
  update_time timestamptz not null default now()
);

create trigger update_timestamp before
update on journey_user_steps for each row execute procedure moddatetime(update_time);

create index idx_journey_user_steps_execution_id on journey_user_steps (journey_execution_id);
create index idx_journey_user_steps_state on journey_user_steps (state);

-- Create journey_operations table for idempotency tracking
create table journey_operations (
  id char(20) primary key,
  journey_execution_id char(20) not null references journey_executions(id) on delete cascade,
  step_id varchar(50) not null,
  operation_id varchar(100) not null,
  result jsonb,
  create_time timestamptz not null default now(),
  unique (journey_execution_id, step_id, operation_id)
);

create index idx_journey_operations_execution_id on journey_operations (journey_execution_id);
create index idx_journey_operations_operation_id on journey_operations (operation_id);