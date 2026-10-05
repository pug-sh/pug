-- name: GetSSOConnectionByIDForUpdate :one
select * from org_sso_connections where id = @id and org_id = @org_id for update;

-- name: GetSSOConnectionIssuerForShare :one
select issuer_url from org_sso_connections where id = @id for share;

-- name: CreateSSOConnection :one
insert into org_sso_connections (id, org_id, label, issuer_url, client_id, client_secret_ciphertext)
values (@id, @org_id, @label, @issuer_url, @client_id, @client_secret_ciphertext)
returning *;

-- name: UpdateSSOConnection :one
update org_sso_connections
set label = @label,
    issuer_url = @issuer_url,
    client_id = @client_id,
    client_secret_ciphertext = @client_secret_ciphertext
where id = @id and org_id = @org_id
returning *;

-- name: DeleteSSOConnection :execrows
delete from org_sso_connections where id = @id and org_id = @org_id;
