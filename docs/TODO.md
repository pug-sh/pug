# TODO

## Subsume delivery events into events domain

The `delivery` domain currently handles both sending notifications and recording delivery events. Once the `events` domain is mature, delivery event recording should move entirely into the events system:

- Remove `DeliveryService.RecordEvent` RPC and related proto definitions
- Remove the `deliveries` NATS stream (delivery events flow through `events` stream instead)
- Delivery service only publishes `cotton.*` events via the events `Publisher` interface
- Delivery domain becomes purely about routing and sending notifications (FCM, APN, email)
- Delete `delivery/v1/delivery.proto` event-related messages (`DeliveryEvent`, `DeliveryEventMessage`, `BatchDeliveryEvents`, `RecordEventRequest/Response`)
- Keep only delivery-specific messages (`BatchMulticastMessage`, `SubscriptionToken`, etc.)

## Dead letter queue for events pipeline

Poison messages (e.g. corrupt protobuf) are currently terminated via `msg.Term()` and logged, but the data is lost. Add a dead letter queue so failed messages can be inspected and replayed:

- Create a `dlq.events` subject and separate NATS stream for terminated messages (must not fall under `events.>` or the events-writer will consume them)
- On `PermanentError`, publish the raw message bytes to the DLQ before calling `msg.Term()`
- Add a CLI command (`cotton events dlq inspect` / `cotton events dlq replay`) to view and replay DLQ messages
- Add metrics/alerting on DLQ depth
