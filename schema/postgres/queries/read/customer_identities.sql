-- name: GetCustomerIdentityByProviderSubject :one
select *
from customer_identities
where provider = @provider
  and provider_subject = @provider_subject;
