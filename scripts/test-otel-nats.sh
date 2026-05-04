#!/usr/bin/env bash
# Fires a single SDK event through the RPC server to verify end-to-end NATS OTel
# tracing. After running this, open HyperDX at http://localhost:3001 and search
# for "send events.ingest" — you should see a three-span trace:
#
#   sdk.events.v1.EventsService/BatchCreate  [pug-server]
#     └─ send events.ingest                  [pug-server]
#          └─ process events.ingest          [pug-worker-events]
#
# Usage:
#   SDK_KEY=<your-public-sdk-key> ./scripts/test-otel-nats.sh
#   SDK_KEY=<key> SERVER=http://localhost:9090 ./scripts/test-otel-nats.sh

set -euo pipefail

SERVER="${SERVER:-http://localhost:3000}"
SDK_KEY="${SDK_KEY:-pub_221a6fbf4f3d426be3cb}"  # default project (d7c531ch9uepjbmemuqg)

curl -sf -X POST "${SERVER}/sdk.events.v1.EventsService/BatchCreate" \
  -H "Content-Type: application/json" \
  -H "x-api-key: ${SDK_KEY}" \
  -d '{
    "events": [{
      "event_id":   "550e8400-e29b-41d4-a716-446655440000",
      "distinct_id": "test-user-otel",
      "kind":        "otel_nats_test",
      "session_id":  "550e8400-e29b-41d4-a716-446655440001",
      "occur_time":  "'"$(date -u +%Y-%m-%dT%H:%M:%SZ)"'",
      "custom_properties": {
        "test": "otel-nats",
        "span_check": "send events.ingest → process events.ingest"
      }
    }]
  }'

echo  # newline after curl output
echo "Event fired. Check HyperDX at http://localhost:3001"
echo "OTel collector span count: curl -s http://localhost:8888/metrics | grep otelcol_exporter_sent_spans"
