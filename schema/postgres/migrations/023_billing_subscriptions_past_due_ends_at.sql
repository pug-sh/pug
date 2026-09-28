-- +goose Up
-- When the provider's grace period for a failed card ends; past it the subscription
-- is held or cancelled. Null outside one. Only a delivery carries it -- a direct
-- read cannot see it -- so the apply keeps a stored one across reads while the card
-- is still failing.
alter table billing_subscriptions add column past_due_ends_at timestamptz;

-- +goose Down
alter table billing_subscriptions drop column past_due_ends_at;
