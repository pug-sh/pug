# TODO

## Subsume delivery events into events domain

The `delivery` domain currently handles both sending notifications and recording delivery events. Once the `events` domain is mature, delivery event recording should move entirely into the events system:

- Remove `DeliveryService.RecordEvent` RPC and related proto definitions
- Remove the `deliveries` NATS stream (delivery events flow through `events` stream instead)
- Delivery service only publishes `pug.*` events via the events `Publisher` interface
- Delivery domain becomes purely about routing and sending notifications (FCM, APN, email)
- Delete `delivery/v1/delivery.proto` event-related messages (`DeliveryEvent`, `DeliveryEventMessage`, `BatchDeliveryEvents`, `RecordEventRequest/Response`)
- Keep only delivery-specific messages (`BatchMulticastMessage`, `SubscriptionToken`, etc.)

## Push notifications: revive or remove (dormant since PR #4)

Push-notification **entry points** were removed in PR #4 ("Disable push notification worker and API entry points") while the implementation was intentionally kept until push delivery is production-ready. The implementation packages are now orphaned (unreferenced by any binary), so `go build` still passes and the compiler will **not** flag them as dead. When push delivery is picked back up, **either**:

**Revive** (re-wire the kept implementation):

- Re-add the cobra commands + `dev` errgroup launches in `cmd/pug/main.go` (`device`, `campaign`, `scheduler`)
- Re-add the standalone worker binaries `cmd/workers/{campaign,device,scheduler}` and their `Makefile` build lines
- Re-register the `campaigns`/`delivery`/`devices` RPC handlers + reflection names in `internal/app/server/server.go`
- Restore the `devices`/`campaigns`/`deliveries` streams (+ their `dlq-*`) and the `device-processor`/`campaign-processor` consumers in `schema/nats/{streams,consumers}.yaml` (removed alongside PR #4)

**Remove** (if push delivery is abandoned):

- Delete `internal/app/workers/{campaigns,devices,scheduler}`, `internal/core/{campaigns,delivery,devices}`, and `internal/app/server/rpc/{shared/campaigns,shared/delivery,sdk/devices}`
- Delete the `campaigns`/`delivery`/`devices` `.proto` definitions and regenerate (`make rpc`)

## Dead letter queue for events pipeline

Poison messages (e.g. corrupt protobuf) are currently terminated via `msg.Term()` and logged, but the data is lost. Add a dead letter queue so failed messages can be inspected and replayed:

- Create a `dlq.events` subject and separate NATS stream for terminated messages (must not fall under `events.>` or the events-writer will consume them)
- On `PermanentError`, publish the raw message bytes to the DLQ before calling `msg.Term()`
- Add a CLI command (`pug events dlq inspect` / `pug events dlq replay`) to view and replay DLQ messages
- Add metrics/alerting on DLQ depth

## Client-side SDKs

Create official mobile SDKs in separate repositories that wrap the SDK RPCs:

### Android SDK (Kotlin)
- HTTP client with OkHttp
- Event batching with local persistence (Room/SQLite)
- Device token registration (FCM)
- Retry logic with exponential backoff

### iOS SDK (Swift)
- HTTP client with URLSession
- Event batching with UserDefaults/SQLite
- Device token registration (APN)
- Retry logic with exponential backoff

## Data Governance

- Data retention policies (auto-delete events older than N days)
- GDPR export (export all user data as JSON/CSV)
- GDPR delete (right to be forgotten - hard-delete profile + events)

## Collaboration

- Teams/workspaces (group projects under a team)
- Role-based permissions (admin, editor, viewer)
- Audit logs (track who did what)

## Product Analytics Features

- Computed properties (derived properties from raw events)
- Saved views (save filter configurations)

## Integrations

- Slack notifications (alert on anomaly thresholds)
- Export to data warehouses (BigQuery, Snowflake, Redshift)

## Enterprise

- SSO/SAML authentication
- API rate limiting per project/org
