-- name: CreateCustomerIdentity :one
insert into customer_identities (id, customer_id, provider, provider_subject)
values (@id, @customer_id, @provider, @provider_subject)
returning *;
