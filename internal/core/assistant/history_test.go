package assistant

import (
	"context"
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
	if err := saveHistory(ctx, rd.Client, "conv_1", messages); err != nil {
		t.Fatalf("saveHistory: %v", err)
	}

	loaded, err := loadHistory(ctx, rd.Client, "conv_1")
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

// The stored value must stay byte-compatible with the TS service —
// [{"role":1,"content":"hi"}] — so in-flight conversations survive cutover.
func TestHistory_StoredShapeIsTSCompatible(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rd := testutil.SetupRedis(t)
	ctx := context.Background()

	messages := []*aidashboardsv1.Message{
		{Role: aidashboardsv1.Message_ROLE_USER.Enum(), Content: proto.String("hi")},
	}
	if err := saveHistory(ctx, rd.Client, "conv_shape", messages); err != nil {
		t.Fatalf("saveHistory: %v", err)
	}
	raw, err := rd.Client.Get(ctx, "conversation:conv_shape:messages").Result()
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

	loaded, err := loadHistory(context.Background(), rd.Client, "conv_unseen")
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

	if err := rd.Client.Set(ctx, "conversation:conv_bad:messages", "{not json", 0).Err(); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := loadHistory(ctx, rd.Client, "conv_bad"); err == nil {
		t.Fatal("expected an error for malformed history")
	}
}

func TestHistory_SaveSetsAndRefreshesTTL(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rd := testutil.SetupRedis(t)
	ctx := context.Background()

	if err := saveHistory(ctx, rd.Client, "conv_ttl", nil); err != nil {
		t.Fatalf("saveHistory: %v", err)
	}
	ttl, err := rd.Client.TTL(ctx, "conversation:conv_ttl:messages").Result()
	if err != nil {
		t.Fatalf("ttl: %v", err)
	}
	if ttl <= 6*24*time.Hour || ttl > 7*24*time.Hour {
		t.Fatalf("ttl = %v, want ~7d", ttl)
	}

	// Age the key, then save again: the TTL must reset to the full window so an
	// in-use conversation never expires mid-session.
	if err := rd.Client.Expire(ctx, "conversation:conv_ttl:messages", time.Hour).Err(); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if err := saveHistory(ctx, rd.Client, "conv_ttl", nil); err != nil {
		t.Fatalf("saveHistory: %v", err)
	}
	ttl, err = rd.Client.TTL(ctx, "conversation:conv_ttl:messages").Result()
	if err != nil {
		t.Fatalf("ttl: %v", err)
	}
	if ttl <= 6*24*time.Hour {
		t.Fatalf("ttl = %v, want refreshed to ~7d", ttl)
	}
}
