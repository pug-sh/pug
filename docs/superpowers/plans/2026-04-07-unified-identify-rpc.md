# Unified Identify RPC Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the two-RPC `Register` + `Identify` profiles SDK surface with a single `Identify(external_id, traits?, anonymous_id?)` RPC.

**Architecture:** The handler publishes a single NATS message. The identify worker upserts by `(project_id, external_id)` for the common path, and optionally merges an anonymous profile when `anonymous_id` is provided. Server generates internal `profile_id` via xid.

**Tech Stack:** Go, Connect RPC, protobuf (buf), sqlc, NATS JetStream, PostgreSQL.

---

### Task 1: Proto — Replace Register + Identify with unified Identify

**Files:**
- Modify: `proto/sdk/profiles/v1/profiles.proto`

- [ ] **Step 1: Rewrite the proto file**

Replace the entire contents of `proto/sdk/profiles/v1/profiles.proto` with:

```protobuf
edition = "2023";
package sdk.profiles.v1;

import "buf/validate/validate.proto";
import "google/protobuf/struct.proto";

option features.field_presence = IMPLICIT;
option go_package = "github.com/fivebitsio/cotton/internal/gen/proto/sdk/profiles/v1;sdkprofilesv1";

service ProfilesSDKService {
  rpc Identify(IdentifyRequest) returns (IdentifyResponse);
}

message IdentifyRequest {
  string external_id = 1 [(buf.validate.field).string = {min_len: 1}];
  google.protobuf.Struct traits = 2;
  string anonymous_id = 3;
}

message IdentifyResponse {}

message ProfileIdentifyMessage {
  string external_id = 1;
  google.protobuf.Struct traits = 2;
  string anonymous_id = 3;
  string project_id = 4;
}
```

- [ ] **Step 2: Lint and regenerate**

Run: `make lint-proto && make rpc`
Expected: Clean lint, generated files updated in `internal/gen/proto/sdk/profiles/v1/`.

- [ ] **Step 3: Commit**

```bash
git add proto/sdk/profiles/v1/profiles.proto internal/gen/proto/sdk/profiles/v1/
git commit -m "proto: replace Register+Identify with unified Identify RPC"
```

---

### Task 2: SQL — Add UpsertProfileByExternalID query

**Files:**
- Modify: `schema/postgres/queries/write/profiles.sql`

- [ ] **Step 1: Add UpsertProfileByExternalID and remove SetProfileExternalID**

In `schema/postgres/queries/write/profiles.sql`, add the new query and remove `SetProfileExternalID` (no longer used — the identify worker now upserts by external_id directly). Keep `RegisterProfile` (still used by seed data for anonymous profiles). The file should become:

```sql
-- name: DeleteProfileByIDAndProjectID :execrows
delete from profiles
where id = @id and project_id = @project_id;

-- name: MergeProfileProperties :one
update profiles
set properties = jsonb_shallow_merge(s.properties, profiles.properties)
from profiles s
where s.id = @source_id
  and s.project_id = @project_id
  and profiles.id = @target_id
  and profiles.project_id = @project_id
returning profiles.*;

-- name: ReassignProfileDevices :exec
update profile_devices
set profile_id = @target_id
where profile_id = @source_id and project_id = @project_id;

-- name: RegisterProfile :one
insert into profiles (properties, id, project_id)
values (coalesce(@properties::jsonb, '{}'), @id, @project_id)
on conflict (id, project_id) do update set
  properties = jsonb_shallow_merge(profiles.properties, excluded.properties)
returning *;

-- name: UpsertProfileByExternalID :one
insert into profiles (id, project_id, external_id, properties)
values (@id, @project_id, @external_id, coalesce(@properties::jsonb, '{}'))
on conflict (project_id, external_id) do update set
  properties = jsonb_shallow_merge(profiles.properties, excluded.properties)
returning *;
```

- [ ] **Step 2: Regenerate sqlc**

Run: `make sqlc`
Expected: Generated files updated in `internal/gen/repo/dbwrite/`.

- [ ] **Step 3: Verify compilation**

Run: `go build ./...`
Expected: Build errors in handler, worker, and seed code (they still reference old generated proto types). This is expected — we fix them in subsequent tasks.

- [ ] **Step 4: Commit**

```bash
git add schema/postgres/queries/write/profiles.sql internal/gen/repo/
git commit -m "sql: add UpsertProfileByExternalID, remove SetProfileExternalID"
```

---

### Task 3: NATS — Remove register consumer and subjects

**Files:**
- Modify: `schema/nats/consumers.yaml`
- Modify: `internal/deps/nats/subjects.go`

- [ ] **Step 1: Remove register consumer from consumers.yaml**

Remove lines 17-25 from `schema/nats/consumers.yaml` (the entire `profile-register-processor` block):

```yaml
  # Consumer for profile register events
  - name: "profile-register-processor"
    stream_name: "profiles"
    durable_name: "profile-register-processor-durable"
    filter_subject: "profiles.register"
    deliver_policy: "all"
    ack_explicit: true
    max_deliver: 5
    replay_policy: "instant"
```

- [ ] **Step 2: Remove register subjects from subjects.go**

In `internal/deps/nats/subjects.go`, remove:
- `ProfileRegisterSubject = "profiles.register"` (line 9)
- `DLQProfilesRegisterSubject = "dlq.profiles.register"` (line 29)

The file should look like:

```go
package nats

// Subject constants for NATS publishing
const (
	// Device subjects
	DeviceOpsSubject = "devices.ops"

	// Profile subjects
	ProfileIdentifySubject = "profiles.identify"
	ProfileAliasSubject    = "profiles.alias"
	ProfileUpsertSubject   = "profiles.upsert"

	// Campaign subjects
	CampaignScheduledSubject = "campaigns.scheduled"

	// Delivery subjects
	DeliveryEventsSubject = "deliveries.events"

	// Events subjects
	EventsIngestSubject = "events.ingest"

	// Dead letter queue subjects — mirror the ingest subject hierarchy.
	// Subscribe to "dlq.>" for all DLQ messages, or "dlq.profiles.>" for a domain.
	DLQDevicesSubject          = "dlq.devices.ops"
	DLQCampaignsSubject        = "dlq.campaigns.scheduled"
	DLQDeliveriesSubject       = "dlq.deliveries.events"
	DLQEventsSubject           = "dlq.events.ingest"
	DLQProfilesIdentifySubject = "dlq.profiles.identify"
	DLQProfilesAliasSubject    = "dlq.profiles.alias"
	DLQProfilesUpsertSubject   = "dlq.profiles.upsert"
)
```

- [ ] **Step 3: Commit**

```bash
git add schema/nats/consumers.yaml internal/deps/nats/subjects.go
git commit -m "nats: remove register consumer and subject constants"
```

---

### Task 4: Handler — Rewrite to unified Identify

**Files:**
- Modify: `internal/app/server/rpc/sdk/profiles/handler.go`

- [ ] **Step 1: Rewrite the handler**

Replace the entire contents of `internal/app/server/rpc/sdk/profiles/handler.go`:

```go
package profiles

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"
	"github.com/fivebitsio/cotton/internal/app/server/rpc"
	"github.com/fivebitsio/cotton/internal/deps/nats"
	sdkprofilesv1 "github.com/fivebitsio/cotton/internal/gen/proto/sdk/profiles/v1"
	"github.com/fivebitsio/cotton/internal/gen/proto/sdk/profiles/v1/sdkprofilesv1connect"
	"github.com/fivebitsio/cotton/internal/slogx"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

type Server struct {
	sdkprofilesv1connect.UnimplementedProfilesSDKServiceHandler
	producer jetstream.JetStream
}

func NewServer(js jetstream.JetStream) *Server {
	return &Server{
		producer: js,
	}
}

func (s *Server) Identify(
	ctx context.Context,
	req *connect.Request[sdkprofilesv1.IdentifyRequest],
) (*connect.Response[sdkprofilesv1.IdentifyResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	msg := &sdkprofilesv1.ProfileIdentifyMessage{
		ExternalId:  req.Msg.GetExternalId(),
		Traits:      req.Msg.GetTraits(),
		AnonymousId: req.Msg.GetAnonymousId(),
		ProjectId:   principal.Project.ID,
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		slog.ErrorContext(ctx, "failed to marshal identify message", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to process request"))
	}

	if _, err = s.producer.Publish(ctx, nats.ProfileIdentifySubject, data); err != nil {
		slog.ErrorContext(ctx, "failed to publish identify message", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to process request"))
	}

	return connect.NewResponse(&sdkprofilesv1.IdentifyResponse{}), nil
}
```

- [ ] **Step 2: Verify handler compiles**

Run: `go build ./internal/app/server/...`
Expected: PASS (handler only depends on generated proto, not sqlc).

- [ ] **Step 3: Commit**

```bash
git add internal/app/server/rpc/sdk/profiles/handler.go
git commit -m "handler: rewrite SDK profiles to single Identify RPC"
```

---

### Task 5: Worker — Rewrite identify worker for unified flow

**Files:**
- Modify: `internal/app/workers/profiles/identify/identify.go`

- [ ] **Step 1: Rewrite the identify worker**

Replace the entire contents of `internal/app/workers/profiles/identify/identify.go`:

```go
package identify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/fivebitsio/cotton/internal/app/workers/profiles"
	natsworker "github.com/fivebitsio/cotton/internal/deps/nats"
	"github.com/fivebitsio/cotton/internal/deps/postgres"
	sdkprofilesv1 "github.com/fivebitsio/cotton/internal/gen/proto/sdk/profiles/v1"
	workerprofilesv1 "github.com/fivebitsio/cotton/internal/gen/proto/workers/profiles/v1"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/fivebitsio/cotton/internal/slogx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/rs/xid"
	"github.com/sethvargo/go-envconfig"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func Run(ctx context.Context) error {
	var pgCfg postgres.Config
	if err := envconfig.Process(ctx, &pgCfg); err != nil {
		return err
	}

	pgRO, err := postgres.NewReaderPool(ctx, &pgCfg)
	if err != nil {
		return err
	}
	defer pgRO.Close()

	pgW, err := postgres.NewWriterPool(ctx, &pgCfg)
	if err != nil {
		return err
	}
	defer pgW.Close()

	natsClient, err := natsworker.New(ctx)
	if err != nil {
		return err
	}
	defer natsClient.Close()

	slog.InfoContext(ctx, "Starting profile identify worker...")
	return StartWorker(ctx, pgRO, pgW, natsClient)
}

func StartWorker(ctx context.Context, pgRO, pgW *pgxpool.Pool, natsClient *natsworker.NATSClient) error {
	consumerConfig, err := natsClient.GetConsumerConfigByName("profile-identify-processor-durable")
	if err != nil {
		return fmt.Errorf("failed to get profile identify consumer config: %w", err)
	}

	profileWorker := profiles.NewWorker(pgRO, pgW)

	messageProcessor := func(ctx context.Context, msg jetstream.Msg) error {
		return handleIdentify(ctx, profileWorker, natsClient, msg.Data())
	}

	config := natsworker.WorkerConfig{
		StreamName:        consumerConfig.StreamName,
		ConsumerName:      consumerConfig.DurableName,
		DurableName:       consumerConfig.DurableName,
		FilterSubject:     consumerConfig.FilterSubject,
		Concurrency:       100,
		ProcessingTimeout: 25 * time.Second,
		MaxDeliver:        consumerConfig.MaxDeliver,
		AckWait:           30 * time.Second,
		DLQSubject:        natsworker.DLQProfilesIdentifySubject,
	}

	worker, err := natsworker.NewWorker(config, messageProcessor, natsClient)
	if err != nil {
		return err
	}

	return worker.Start(ctx)
}

func handleIdentify(ctx context.Context, w *profiles.Worker, natsClient *natsworker.NATSClient, data []byte) error {
	msg := &sdkprofilesv1.ProfileIdentifyMessage{}
	if err := proto.Unmarshal(data, msg); err != nil {
		slog.ErrorContext(ctx, "failed to unmarshal identify message", slogx.Error(err))
		return natsworker.NewPermanentError(err).
			With("worker", "profile-identify")
	}

	projectID := msg.GetProjectId()
	externalID := msg.GetExternalId()
	anonymousID := msg.GetAnonymousId()

	traits := msg.GetTraits().AsMap()
	if traits == nil {
		traits = map[string]any{}
	}

	// Upsert the identified profile — creates if new, merges traits if exists.
	profile, err := w.Write.UpsertProfileByExternalID(ctx, dbwrite.UpsertProfileByExternalIDParams{
		ID:         xid.New().String(),
		ProjectID:  projectID,
		ExternalID: externalID,
		Properties: traits,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to upsert profile", slogx.Error(err),
			slog.String("externalId", externalID))
		return err
	}

	// No anonymous profile to merge — publish upsert and we're done.
	if anonymousID == "" {
		return publishUpsert(ctx, natsClient, profile.ID, projectID, profile.ExternalID.String, profile.Properties, false, profile.CreateTime.Time, profile.UpdateTime.Time)
	}

	// Anonymous merge path: merge anonymous profile into the identified one.
	return mergeAnonymous(ctx, w, natsClient, projectID, externalID, anonymousID, profile)
}

func mergeAnonymous(ctx context.Context, w *profiles.Worker, natsClient *natsworker.NATSClient, projectID, externalID, anonymousID string, target dbwrite.Profile) error {
	// If the anonymous ID is the same as the target, nothing to merge.
	if anonymousID == target.ID {
		return publishUpsert(ctx, natsClient, target.ID, projectID, target.ExternalID.String, target.Properties, false, target.CreateTime.Time, target.UpdateTime.Time)
	}

	tx, err := w.PgW.Begin(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed starting merge transaction", slogx.Error(err))
		return err
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			slog.ErrorContext(ctx, "failed rolling back merge transaction", slogx.Error(err))
		}
	}()

	qtx := w.Write.WithTx(tx)

	// Merge properties from anonymous into target.
	merged, err := qtx.MergeProfileProperties(ctx, dbwrite.MergeProfilePropertiesParams{
		SourceID:  anonymousID,
		TargetID:  target.ID,
		ProjectID: projectID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Anonymous profile already deleted (retry). Use pre-merge target snapshot.
			slog.WarnContext(ctx, "anonymous profile missing during merge, using pre-merge snapshot",
				slog.String("anonymousId", anonymousID), slog.String("targetId", target.ID))
			merged = target
		} else {
			slog.ErrorContext(ctx, "failed merging profile properties", slogx.Error(err),
				slog.String("anonymousId", anonymousID), slog.String("targetId", target.ID))
			return err
		}
	}

	if err := qtx.ReassignProfileDevices(ctx, dbwrite.ReassignProfileDevicesParams{
		TargetID:  target.ID,
		SourceID:  anonymousID,
		ProjectID: projectID,
	}); err != nil {
		slog.ErrorContext(ctx, "failed reassigning devices", slogx.Error(err),
			slog.String("anonymousId", anonymousID), slog.String("targetId", target.ID))
		return err
	}

	if _, err := qtx.DeleteProfileByIDAndProjectID(ctx, dbwrite.DeleteProfileByIDAndProjectIDParams{
		ID:        anonymousID,
		ProjectID: projectID,
	}); err != nil {
		slog.ErrorContext(ctx, "failed deleting anonymous profile", slogx.Error(err),
			slog.String("anonymousId", anonymousID))
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "failed committing merge transaction", slogx.Error(err))
		return err
	}

	slog.InfoContext(ctx, "identify merge committed",
		slog.String("targetId", target.ID),
		slog.String("anonymousId", anonymousID))

	// Publish upsert for target.
	if err := publishUpsert(ctx, natsClient, merged.ID, projectID, merged.ExternalID.String, merged.Properties, false, merged.CreateTime.Time, merged.UpdateTime.Time); err != nil {
		return err
	}

	// Soft-delete the anonymous profile in ClickHouse.
	if err := publishUpsert(ctx, natsClient, anonymousID, projectID, "", nil, true, time.Now(), time.Now()); err != nil {
		return err
	}

	// Publish alias for traceability.
	aliasMsg := &workerprofilesv1.ProfileAliasMessage{
		AliasId:    anonymousID,
		ProfileId:  target.ID,
		ExternalId: externalID,
		ProjectId:  projectID,
	}

	aliasData, err := proto.Marshal(aliasMsg)
	if err != nil {
		slog.ErrorContext(ctx, "failed marshalling alias message", slogx.Error(err),
			slog.String("anonymousId", anonymousID), slog.String("targetId", target.ID))
		return natsworker.NewPermanentError(err).
			With("worker", "profile-identify").
			With("anonymous_id", anonymousID)
	}
	if err = natsClient.Publish(ctx, natsworker.ProfileAliasSubject, aliasData); err != nil {
		slog.ErrorContext(ctx, "failed publishing alias message", slogx.Error(err),
			slog.String("anonymousId", anonymousID), slog.String("targetId", target.ID))
		return fmt.Errorf("publish alias after committed merge: %w", err)
	}

	return nil
}

func publishUpsert(ctx context.Context, natsClient *natsworker.NATSClient, profileID, projectID, externalID string, properties map[string]any, isDeleted bool, createTime, updateTime time.Time) error {
	if properties == nil {
		properties = map[string]any{}
	}

	propsStruct, err := structpb.NewStruct(properties)
	if err != nil {
		slog.ErrorContext(ctx, "failed converting profile properties to struct", slogx.Error(err),
			slog.String("profileId", profileID))
		return natsworker.NewPermanentError(err).
			With("worker", "profile-identify").
			With("profile_id", profileID)
	}

	upsertMsg := &workerprofilesv1.ProfileUpsertMessage{
		ProfileId:  profileID,
		ProjectId:  projectID,
		ExternalId: externalID,
		Properties: propsStruct,
		IsDeleted:  isDeleted,
		CreateTime: timestamppb.New(createTime),
		UpdateTime: timestamppb.New(updateTime),
	}

	upsertData, err := proto.Marshal(upsertMsg)
	if err != nil {
		slog.ErrorContext(ctx, "failed marshalling profile upsert message", slogx.Error(err),
			slog.String("profileId", profileID))
		return natsworker.NewPermanentError(err).
			With("worker", "profile-identify").
			With("profile_id", profileID)
	}

	if err := natsClient.Publish(ctx, natsworker.ProfileUpsertSubject, upsertData); err != nil {
		slog.ErrorContext(ctx, "failed publishing profile upsert", slogx.Error(err),
			slog.String("profileId", profileID))
		return fmt.Errorf("publish profile upsert: %w", err)
	}

	return nil
}
```

- [ ] **Step 2: Verify worker compiles**

Run: `go build ./internal/app/workers/profiles/...`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/app/workers/profiles/identify/identify.go
git commit -m "worker: rewrite identify worker for unified Identify flow"
```

---

### Task 6: Delete register worker and update CLI wiring

**Files:**
- Delete: `internal/app/workers/profiles/register/register.go`
- Delete: `cmd/workers/profile/register/main.go`
- Modify: `cmd/cotton/main.go`
- Modify: `Makefile`

- [ ] **Step 1: Delete register worker files**

```bash
rm internal/app/workers/profiles/register/register.go
rm cmd/workers/profile/register/main.go
rmdir internal/app/workers/profiles/register
rmdir cmd/workers/profile/register
```

- [ ] **Step 2: Update cmd/cotton/main.go**

Remove the `register` import (line 21):

```go
"github.com/fivebitsio/cotton/internal/app/workers/profiles/register"
```

Remove the `profileRegisterCmd` variable (lines 99-103):

```go
var profileRegisterCmd = &cobra.Command{
	Use:   "register",
	Short: "Start the profile register worker",
	Run:   run(register.Run),
}
```

Remove `register.Run(ctx)` from the `devCmd` errgroup (line 162):

```go
g.Go(func() error { return register.Run(ctx) })
```

Remove `profileRegisterCmd` from `init()` (line 266):

```go
profileCmd.AddCommand(profileRegisterCmd)
```

- [ ] **Step 3: Update Makefile**

Remove line 74 from the `build` target:

```makefile
go build -o bin/cotton-worker-profile-register ./cmd/workers/profile/register
```

- [ ] **Step 4: Verify full build**

Run: `go build ./...`
Expected: PASS. All references to the register worker are removed.

- [ ] **Step 5: Commit**

```bash
git add -A
git commit -m "remove register worker, CLI command, and build target"
```

---

### Task 7: Seed data — Use UpsertProfileByExternalID

**Files:**
- Modify: `internal/app/seed/postgres/seed.go`

- [ ] **Step 1: Update seedProfiles**

In `internal/app/seed/postgres/seed.go`, replace the `seedProfiles` method (lines 232-273). The change: identified profiles now use `UpsertProfileByExternalID` (single call instead of `RegisterProfile` + `SetProfileExternalID`). Anonymous profiles still use `RegisterProfile` (kept in the SQL queries for this purpose).

```go
func (s *Seeder) seedProfiles(ctx context.Context, projectID string) ([]string, error) {
	slog.InfoContext(ctx, "seeding profiles",
		slog.String("project_id", projectID),
		slog.Int("count", profileCount),
	)

	w := dbwrite.New(s.deps.pg)
	var identifiedIDs []string
	for i := range profileCount {
		id := fmt.Sprintf("user-%05d", i)
		props := randomProperties(i)

		if rand.Float32() < 0.60 {
			externalID := externalIDForProfile(props, i)
			if _, err := w.UpsertProfileByExternalID(ctx, dbwrite.UpsertProfileByExternalIDParams{
				ID:         id,
				ProjectID:  projectID,
				ExternalID: externalID,
				Properties: props,
			}); err != nil {
				return nil, fmt.Errorf("upsert profile %s: %w", id, err)
			}
			identifiedIDs = append(identifiedIDs, id)
		} else {
			if _, err := w.RegisterProfile(ctx, dbwrite.RegisterProfileParams{
				ID:         id,
				ProjectID:  projectID,
				Properties: props,
			}); err != nil {
				return nil, fmt.Errorf("insert anonymous profile %s: %w", id, err)
			}
		}
	}

	slog.InfoContext(ctx, "profiles seeded",
		slog.Int("count", profileCount),
		slog.Int("identified", len(identifiedIDs)),
		slog.Int("anonymous", profileCount-len(identifiedIDs)),
	)
	return identifiedIDs, nil
}
```

- [ ] **Step 2: Verify seed compiles**

Run: `go build ./internal/app/seed/...`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/app/seed/postgres/seed.go
git commit -m "seed: use UpsertProfileByExternalID for identified profiles"
```

---

### Task 8: Full build and lint

- [ ] **Step 1: Build everything**

Run: `go build ./...`
Expected: PASS — no compilation errors.

- [ ] **Step 2: Run linter**

Run: `make lint`
Expected: PASS — no lint errors.

- [ ] **Step 3: Run tests**

Run: `make test`
Expected: PASS — all tests pass.

- [ ] **Step 4: Commit any formatting fixes**

```bash
git add -A
git commit -m "chore: formatting fixes"
```

(Skip this commit if there are no changes.)
