-- name: CreateCustomerIdentity :one
insert into customer_identities (id, customer_id, provider, provider_subject)
values (@id, @customer_id, @provider, @provider_subject)
returning *;

-- name: DeleteSSOConnectionIdentities :exec
-- Only a connection's (conn:<id>) rows, so a config provider id can't unlink everyone.
delete from customer_identities where provider = @provider and provider like 'conn:%';
