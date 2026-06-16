-- name: GetComplianceRequestByID :one
select * from compliance_requests
where id = @id and project_id = @project_id;
