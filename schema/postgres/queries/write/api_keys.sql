-- name: CreateApiKey :one
insert into api_keys (display_name, id, kind, masked, project_id, token)
values (@display_name, @id, @kind, @masked, @project_id, @token)
returning *;

-- name: DeleteApiKey :one
-- Scoped by project_id, so a key id belonging to another project can never be
-- deleted by guessing it. No row deleted (pgx.ErrNoRows) means "not found in
-- this project" — the caller does not distinguish the two.
--
-- This row is the only place the key exists, so deleting it is the whole
-- revocation. The caller drops the cached project row keyed by its token.
delete from api_keys k
where k.id = @id and k.project_id = @project_id
returning k.*;
