package assistant

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	aidashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/ai/dashboards/v1"
	"github.com/pug-sh/pug/internal/testutil"
)

func TestHistory_RoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rd := testutil.SetupRedis(t)
	ctx := context.Background()

	messages := []*aidashboardsv1.Message{
		{Role: aidashboardsv1.Message_ROLE_USER.Enum(), Content: proto.String("hi")},
		{Role: aidashboardsv1.Message_ROLE_ASSISTANT.Enum(), Content: proto.String("hello")},
	}
	if err := saveHistory(ctx, rd.Client, testCreds, "conv_1", messages); err != nil {
		t.Fatalf("saveHistory: %v", err)
	}

	loaded, err := loadHistory(ctx, rd.Client, testCreds, "conv_1")
	if err != nil {
		t.Fatalf("loadHistory: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("len = %d", len(loaded))
	}
	if loaded[0].GetRole() != aidashboardsv1.Message_ROLE_USER || loaded[0].GetContent() != "hi" {
		t.Fatalf("first = %v %q", loaded[0].GetRole(), loaded[0].GetContent())
	}
	if loaded[1].GetRole() != aidashboardsv1.Message_ROLE_ASSISTANT || loaded[1].GetContent() != "hello" {
		t.Fatalf("second = %v %q", loaded[1].GetRole(), loaded[1].GetContent())
	}
}

// The stored value is shared across a rolling deploy: old and new pods read the
// same Redis, so the shape cannot change without a migration.
func TestHistory_StoredShapeIsStable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rd := testutil.SetupRedis(t)
	ctx := context.Background()

	messages := []*aidashboardsv1.Message{
		{Role: aidashboardsv1.Message_ROLE_USER.Enum(), Content: proto.String("hi")},
	}
	if err := saveHistory(ctx, rd.Client, testCreds, "conv_shape", messages); err != nil {
		t.Fatalf("saveHistory: %v", err)
	}
	raw, err := rd.Client.Get(ctx, historyKey(testCreds, "conv_shape")).Result()
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if raw != `[{"role":1,"content":"hi"}]` {
		t.Fatalf("stored = %s", raw)
	}
}

func TestHistory_LoadAbsentKeyReturnsEmpty(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rd := testutil.SetupRedis(t)

	loaded, err := loadHistory(context.Background(), rd.Client, testCreds, "conv_unseen")
	if err != nil {
		t.Fatalf("loadHistory: %v", err)
	}
	if len(loaded) != 0 {
		t.Fatalf("len = %d, want 0", len(loaded))
	}
}

// Fail closed: malformed stored JSON must surface as an error, not as an empty
// history that would read to the user like the model forgot everything.
func TestHistory_MalformedStoredJSONIsAnError(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rd := testutil.SetupRedis(t)
	ctx := context.Background()

	if err := rd.Client.Set(ctx, historyKey(testCreds, "conv_bad"), "{not json", 0).Err(); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := loadHistory(ctx, rd.Client, testCreds, "conv_bad"); err == nil {
		t.Fatal("expected an error for malformed history")
	}
}

func TestHistory_SaveSetsAndRefreshesTTL(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rd := testutil.SetupRedis(t)
	ctx := context.Background()

	if err := saveHistory(ctx, rd.Client, testCreds, "conv_ttl", nil); err != nil {
		t.Fatalf("saveHistory: %v", err)
	}
	ttl, err := rd.Client.TTL(ctx, historyKey(testCreds, "conv_ttl")).Result()
	if err != nil {
		t.Fatalf("ttl: %v", err)
	}
	if ttl <= 6*24*time.Hour || ttl > 7*24*time.Hour {
		t.Fatalf("ttl = %v, want ~7d", ttl)
	}

	// Age the key, then save again: the TTL must reset to the full window so an
	// in-use conversation never expires mid-session.
	if err := rd.Client.Expire(ctx, historyKey(testCreds, "conv_ttl"), time.Hour).Err(); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if err := saveHistory(ctx, rd.Client, testCreds, "conv_ttl", nil); err != nil {
		t.Fatalf("saveHistory: %v", err)
	}
	ttl, err = rd.Client.TTL(ctx, historyKey(testCreds, "conv_ttl")).Result()
	if err != nil {
		t.Fatalf("ttl: %v", err)
	}
	if ttl <= 6*24*time.Hour {
		t.Fatalf("ttl = %v, want refreshed to ~7d", ttl)
	}
}

// conversation_id is client-minted, so the key scope is the only thing keeping
// one caller's conversation out of another's reach.
func TestHistory_IsolatedByProjectAndCustomer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rd := testutil.SetupRedis(t)
	ctx := context.Background()

	mine := CallerCredentials{JWT: "j", ProjectID: "prj_1", CustomerID: "cus_1"}
	messages := []*aidashboardsv1.Message{
		{Role: aidashboardsv1.Message_ROLE_USER.Enum(), Content: proto.String("secret")},
	}
	if err := saveHistory(ctx, rd.Client, mine, "shared_id", messages); err != nil {
		t.Fatalf("saveHistory: %v", err)
	}

	for name, theirs := range map[string]CallerCredentials{
		"other customer": {JWT: "j", ProjectID: "prj_1", CustomerID: "cus_2"},
		"other project":  {JWT: "j", ProjectID: "prj_2", CustomerID: "cus_1"},
	} {
		loaded, err := loadHistory(ctx, rd.Client, theirs, "shared_id")
		if err != nil {
			t.Fatalf("%s: loadHistory: %v", name, err)
		}
		if len(loaded) != 0 {
			t.Fatalf("%s: read %d messages from another caller's conversation", name, len(loaded))
		}
	}

	// Pinned as a literal so the scope cannot be reverted without a failure.
	if got, want := historyKey(mine, "shared_id"), "conversation:prj_1:cus_1:shared_id:messages"; got != want {
		t.Fatalf("historyKey = %q, want %q", got, want)
	}
}

func TestTrimHistory_KeepsNewestWithinBudget(t *testing.T) {
	big := strings.Repeat("x", historyByteBudget/2+1)
	msgs := []*aidashboardsv1.Message{
		{Role: aidashboardsv1.Message_ROLE_USER.Enum(), Content: proto.String(big)},
		{Role: aidashboardsv1.Message_ROLE_ASSISTANT.Enum(), Content: proto.String("old")},
		{Role: aidashboardsv1.Message_ROLE_USER.Enum(), Content: proto.String(big)},
		{Role: aidashboardsv1.Message_ROLE_ASSISTANT.Enum(), Content: proto.String("new")},
	}
	got := trimHistory(msgs)
	if len(got) != 3 || got[0].GetContent() != "old" {
		t.Fatalf("trimHistory kept %d messages", len(got))
	}
	if small := trimHistory(msgs[1:2]); len(small) != 1 {
		t.Fatalf("under-budget history was trimmed")
	}
}

func TestAcquireTurn_ReleaseOnlyDeletesItsOwnLock(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rd := testutil.SetupRedis(t)
	ctx := context.Background()
	key := turnLockKey(testCreds, "conv_lock")

	release, err := acquireTurn(ctx, rd.Client, testCreds, "conv_lock")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireTurn(ctx, rd.Client, testCreds, "conv_lock"); !errors.Is(err, ErrTurnInProgress) {
		t.Fatalf("second acquire err = %v", err)
	}
	release()
	if n, _ := rd.Client.Exists(ctx, key).Result(); n != 0 {
		t.Fatal("lock not released")
	}

	// The lock expired and a newer turn took it: the stale release is a no-op.
	release, err = acquireTurn(ctx, rd.Client, testCreds, "conv_lock")
	if err != nil {
		t.Fatal(err)
	}
	if err := rd.Client.Set(ctx, key, "newer-turn", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	release()
	if got, _ := rd.Client.Get(ctx, key).Result(); got != "newer-turn" {
		t.Fatalf("stale release deleted the newer lock (value=%q)", got)
	}
}
