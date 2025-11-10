-- Drop journey_operations table
drop table if exists journey_operations;

-- Drop journey_user_steps table
drop table if exists journey_user_steps;

-- Drop journey_steps table
drop table if exists journey_steps;

-- Drop journey_executions table
drop table if exists journey_executions;

-- Drop journeys table
drop table if exists journeys;

-- Drop moddatetime extension if we want to clean it up (though it may be used by other tables)
-- For safety we'll leave it as it may be used by other tables