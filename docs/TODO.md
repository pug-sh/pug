# TODO

## Subsume delivery events into events domain

The `delivery` domain currently handles both sending notifications and recording delivery events. Once the `events` domain is mature, delivery event recording should move entirely into the events system:

- Remove `DeliveryService.RecordEvent` RPC and related proto definitions
- Remove the `deliveries` NATS stream (delivery events flow through `events` stream instead)
- Delivery service only publishes `cotton.*` events via the events `Publisher` interface
- Delivery domain becomes purely about routing and sending notifications (FCM, APN, email)
- Delete `delivery/v1/delivery.proto` event-related messages (`DeliveryEvent`, `DeliveryEventMessage`, `BatchDeliveryEvents`, `RecordEventRequest/Response`)
- Keep only delivery-specific messages (`BatchMulticastMessage`, `SubscriptionToken`, etc.)
