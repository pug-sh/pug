-- name: GetOrgEmailProvider :one
select org_id, kind, from_address, reply_to, secret_ciphertext, create_time, update_time
from org_email_providers
where org_id = @org_id;
