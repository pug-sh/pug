-- name: UpsertOrgEmailProvider :one
insert into org_email_providers (org_id, kind, from_address, reply_to, secret_ciphertext)
values (@org_id, @kind, @from_address, @reply_to, @secret_ciphertext)
on conflict (org_id) do update
set kind = excluded.kind,
    from_address = excluded.from_address,
    reply_to = excluded.reply_to,
    secret_ciphertext = excluded.secret_ciphertext
returning org_id, kind, from_address, reply_to, secret_ciphertext, create_time, update_time;

-- name: DeleteOrgEmailProvider :execrows
delete from org_email_providers where org_id = @org_id;
