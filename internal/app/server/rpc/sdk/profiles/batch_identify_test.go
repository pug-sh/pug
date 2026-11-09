package profiles

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/pug-sh/pug/internal/apperr"
	natsdeps "github.com/pug-sh/pug/internal/deps/nats"
	sdkprofilesv1 "github.com/pug-sh/pug/internal/gen/proto/sdk/profiles/v1"
	"github.com/pug-sh/pug/internal/geo"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// publishedIdentifies decodes what the spy captured, checking each message went
// to the identify subject.
func publishedIdentifies(t *testing.T, spy *spyJetStream) []*sdkprofilesv1.ProfileIdentifyMessage {
	t.Helper()
	spy.mu.Lock()
	defer spy.mu.Unlock()
	out := make([]*sdkprofilesv1.ProfileIdentifyMessage, len(spy.published))
	for i, m := range spy.published {
		if m.Subject != natsdeps.ProfileIdentifySubject {
			t.Errorf("message %d subject = %q, want %q", i, m.Subject, natsdeps.ProfileIdentifySubject)
		}
		out[i] = &sdkprofilesv1.ProfileIdentifyMessage{}
		if err := proto.Unmarshal(m.Data, out[i]); err != nil {
			t.Fatalf("unmarshal message %d: %v", i, err)
		}
	}
	return out
}

func batch(items ...*sdkprofilesv1.IdentifyRequest) *connect.Request[sdkprofilesv1.BatchIdentifyRequest] {
	return connect.NewRequest(&sdkprofilesv1.BatchIdentifyRequest{Identifies: items})
}

// acceptBatch sends items through a fresh spy and checks every one was accepted.
func acceptBatch(t *testing.T, items ...*sdkprofilesv1.IdentifyRequest) *spyJetStream {
	t.Helper()
	spy := &spyJetStream{}
	res, err := NewServer(spy).BatchIdentify(sdkContext("proj-test"), batch(items...))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := res.Msg.GetAccepted(), uint32(len(items)); got != want {
		t.Errorf("accepted = %d, want %d", got, want)
	}
	return spy
}

// identify builds one batch item. An empty anonymous or device id and nil traits
// stay unset.
func identify(t *testing.T, externalID, anonymousID, deviceID string, traits map[string]any) *sdkprofilesv1.IdentifyRequest {
	t.Helper()
	item := &sdkprofilesv1.IdentifyRequest{ExternalId: proto.String(externalID)}
	if anonymousID != "" {
		item.AnonymousId = proto.String(anonymousID)
	}
	if deviceID != "" {
		item.DeviceId = proto.String(deviceID)
	}
	if traits != nil {
		s, err := structpb.NewStruct(traits)
		if err != nil {
			t.Fatalf("traits: %v", err)
		}
		item.Traits = s
	}
	return item
}

func TestBatchIdentify_PublishesEachItem(t *testing.T) {
	spy := &spyJetStream{}
	srv := NewServer(spy)
	traits, _ := structpb.NewStruct(map[string]any{"plan": "pro"})

	res, err := srv.BatchIdentify(sdkContext("proj-test"), batch(
		&sdkprofilesv1.IdentifyRequest{
			ExternalId:  proto.String("user-1"),
			Traits:      traits,
			AnonymousId: proto.String("anon-1"),
			DeviceId:    proto.String("device-1"),
		},
		&sdkprofilesv1.IdentifyRequest{ExternalId: proto.String("user-2")}, // no traits
	))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := res.Msg.GetAccepted(); got != 2 {
		t.Errorf("accepted = %d, want 2", got)
	}

	msgs := publishedIdentifies(t, spy)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 published messages, got %d", len(msgs))
	}
	first, second := msgs[0], msgs[1]
	if first.GetExternalId() != "user-1" || first.GetProjectId() != "proj-test" ||
		first.GetAnonymousId() != "anon-1" || first.GetDeviceId() != "device-1" ||
		first.GetTraits().GetFields()["plan"].GetStringValue() != "pro" {
		t.Errorf("first message = %v", first)
	}
	if second.GetExternalId() != "user-2" || second.GetProjectId() != "proj-test" || len(second.GetTraits().GetFields()) != 0 {
		t.Errorf("second message = %v", second)
	}
}

func TestBatchIdentify_StripsClientSuppliedIP(t *testing.T) {
	spy := &spyJetStream{}
	srv := NewServer(spy)
	traits, _ := structpb.NewStruct(map[string]any{geo.PropIP: "203.0.113.7", "plan": "pro"})

	if _, err := srv.BatchIdentify(sdkContext("proj-test"), batch(
		&sdkprofilesv1.IdentifyRequest{ExternalId: proto.String("user-1"), Traits: traits},
	)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	msgs := publishedIdentifies(t, spy)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 published message, got %d", len(msgs))
	}
	fields := msgs[0].GetTraits().GetFields()
	if _, ok := fields[geo.PropIP]; ok {
		t.Errorf("%s must be stripped from traits, but it was published", geo.PropIP)
	}
	if fields["plan"].GetStringValue() != "pro" {
		t.Errorf("legitimate trait dropped: plan = %v, want %q", fields["plan"], "pro")
	}
}

func TestBatchIdentify_EmptyBatch(t *testing.T) {
	spy := &spyJetStream{}
	srv := NewServer(spy)

	res, err := srv.BatchIdentify(sdkContext("proj-test"), batch())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := res.Msg.GetAccepted(); got != 0 {
		t.Errorf("accepted = %d, want 0", got)
	}
	if msgs := publishedIdentifies(t, spy); len(msgs) != 0 {
		t.Errorf("published %d messages for an empty batch", len(msgs))
	}
}

func TestBatchIdentify_Unauthenticated(t *testing.T) {
	spy := &spyJetStream{}
	srv := NewServer(spy)

	// No principal in context.
	_, err := srv.BatchIdentify(context.Background(), batch(
		&sdkprofilesv1.IdentifyRequest{ExternalId: proto.String("user-1")},
	))
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code() != connect.CodeUnauthenticated {
		t.Fatalf("want unauthenticated apperr, got %v (%T)", err, err)
	}
	if msgs := publishedIdentifies(t, spy); len(msgs) != 0 {
		t.Errorf("published %d messages without a principal", len(msgs))
	}
}

func TestBatchIdentify_PublishErrorStopsTheBatch(t *testing.T) {
	spy := &spyJetStream{failOn: 2}
	srv := NewServer(spy)

	_, err := srv.BatchIdentify(sdkContext("proj-test"), batch(
		&sdkprofilesv1.IdentifyRequest{ExternalId: proto.String("user-1")},
		&sdkprofilesv1.IdentifyRequest{ExternalId: proto.String("user-2")},
		&sdkprofilesv1.IdentifyRequest{ExternalId: proto.String("user-3")},
	))
	if code := connect.CodeOf(err); code != connect.CodeInternal {
		t.Fatalf("code = %v (err %v), want %v", code, err, connect.CodeInternal)
	}
	if strings.Contains(err.Error(), errPublishFailed.Error()) {
		t.Errorf("driver error reached the client: %v", err)
	}
	msgs := publishedIdentifies(t, spy)
	if len(msgs) != 1 || msgs[0].GetExternalId() != "user-1" {
		t.Errorf("published %v, want only user-1", msgs)
	}
}

// The caller only learns the batch failed, so the log has to say how far it got.
func TestBatchIdentify_LogsProgressWhenPublishingStops(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	spy := &spyJetStream{failOn: 3}
	if _, err := NewServer(spy).BatchIdentify(sdkContext("proj-test"), batch(
		identify(t, "user-1", "", "", map[string]any{"a": "1"}),
		identify(t, "user-1", "", "", map[string]any{"b": "2"}), // merges into the first
		identify(t, "user-2", "", "", nil),
		identify(t, "user-3", "", "", nil),
		identify(t, "user-4", "", "", nil),
	)); err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"published=2", "messages=4", "items=5"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs missing %q:\n%s", want, logs.String())
		}
	}
}

// A client that gives up must not leave the handler publishing the rest of its
// batch: it will retry the whole thing.
func TestBatchIdentify_CanceledRequestStopsPublishing(t *testing.T) {
	ctx, cancel := context.WithCancel(sdkContext("proj-test"))
	defer cancel()
	spy := &spyJetStream{onPublish: cancel}

	if _, err := NewServer(spy).BatchIdentify(ctx, batch(
		identify(t, "user-1", "", "", nil),
		identify(t, "user-2", "", "", nil),
		identify(t, "user-3", "", "", nil),
	)); err == nil {
		t.Fatal("expected an error")
	}
	if msgs := publishedIdentifies(t, spy); len(msgs) != 1 {
		t.Errorf("published %d messages after the request was canceled, want 1", len(msgs))
	}
}

// Distinct users, so nothing merges and each item must reach its own message.
func TestBatchIdentify_HandlesEveryItem(t *testing.T) {
	items := make([]*sdkprofilesv1.IdentifyRequest, 1000)
	for i := range items {
		items[i] = identify(t, fmt.Sprintf("user-%d", i), "", "", map[string]any{geo.PropIP: "203.0.113.7", "plan": "pro"})
	}
	items[0].AnonymousId = proto.String("anon-1")
	items[0].DeviceId = proto.String("device-1")

	msgs := publishedIdentifies(t, acceptBatch(t, items...))
	if len(msgs) != 1000 {
		t.Fatalf("expected 1000 published messages, got %d", len(msgs))
	}
	for i, m := range msgs {
		if want := fmt.Sprintf("user-%d", i); m.GetExternalId() != want {
			t.Fatalf("message %d external id = %q, want %q", i, m.GetExternalId(), want)
		}
		if _, ok := m.GetTraits().GetFields()[geo.PropIP]; ok {
			t.Fatalf("message %d kept %s", i, geo.PropIP)
		}
		if m.GetTraits().GetFields()["plan"].GetStringValue() != "pro" {
			t.Fatalf("message %d lost its plan: %v", i, m.GetTraits())
		}
		if i > 0 && (m.GetAnonymousId() != "" || m.GetDeviceId() != "") {
			t.Fatalf("message %d inherited ids: anonymous %q, device %q", i, m.GetAnonymousId(), m.GetDeviceId())
		}
	}
	if msgs[0].GetAnonymousId() != "anon-1" || msgs[0].GetDeviceId() != "device-1" {
		t.Errorf("first message lost its ids: %v", msgs[0])
	}
}

// The identify worker runs concurrently, so two messages for one user can apply in
// either order. Merging them first gives the result of applying them in order, which
// jsonb_shallow_merge defines: per top-level key, the later value wins, null included.
func TestBatchIdentify_MergesItemsThatOnlySetTraits(t *testing.T) {
	type message struct {
		externalID, anonymousID, deviceID string
		traits                            map[string]any
	}
	for _, tc := range []struct {
		name  string
		items []*sdkprofilesv1.IdentifyRequest
		want  []message
	}{
		{
			name: "later values win and earlier keys stay",
			items: []*sdkprofilesv1.IdentifyRequest{
				identify(t, "user-1", "", "", map[string]any{"plan": "free", "a": "1"}),
				identify(t, "user-1", "", "", map[string]any{"plan": "pro", "b": "2"}),
			},
			want: []message{{externalID: "user-1", traits: map[string]any{"plan": "pro", "a": "1", "b": "2"}}},
		},
		{
			name: "other users in between",
			items: []*sdkprofilesv1.IdentifyRequest{
				identify(t, "user-1", "", "", map[string]any{"plan": "free"}),
				identify(t, "user-2", "", "", map[string]any{"plan": "team"}),
				identify(t, "user-1", "", "", map[string]any{"plan": "pro"}),
			},
			want: []message{
				{externalID: "user-1", traits: map[string]any{"plan": "pro"}},
				{externalID: "user-2", traits: map[string]any{"plan": "team"}},
			},
		},
		{
			name: "an explicit null overrides",
			items: []*sdkprofilesv1.IdentifyRequest{
				identify(t, "user-1", "", "", map[string]any{"plan": "pro"}),
				identify(t, "user-1", "", "", map[string]any{"plan": nil}),
			},
			want: []message{{externalID: "user-1", traits: map[string]any{"plan": nil}}},
		},
		{
			name: "earlier item has no traits",
			items: []*sdkprofilesv1.IdentifyRequest{
				identify(t, "user-1", "", "", nil),
				identify(t, "user-1", "", "", map[string]any{"plan": "pro"}),
			},
			want: []message{{externalID: "user-1", traits: map[string]any{"plan": "pro"}}},
		},
		{
			name: "earlier item has an empty traits struct",
			items: []*sdkprofilesv1.IdentifyRequest{
				{ExternalId: proto.String("user-1"), Traits: &structpb.Struct{}},
				identify(t, "user-1", "", "", map[string]any{"plan": "pro"}),
			},
			want: []message{{externalID: "user-1", traits: map[string]any{"plan": "pro"}}},
		},
		{
			name: "later item has no traits",
			items: []*sdkprofilesv1.IdentifyRequest{
				identify(t, "user-1", "", "", map[string]any{"plan": "pro"}),
				identify(t, "user-1", "", "", nil),
			},
			want: []message{{externalID: "user-1", traits: map[string]any{"plan": "pro"}}},
		},
		{
			name: "keeps the ids of the item it joins",
			items: []*sdkprofilesv1.IdentifyRequest{
				identify(t, "user-1", "anon-1", "device-1", map[string]any{"plan": "free"}),
				identify(t, "user-1", "", "", map[string]any{"plan": "pro"}),
			},
			want: []message{{externalID: "user-1", anonymousID: "anon-1", deviceID: "device-1", traits: map[string]any{"plan": "pro"}}},
		},
		{
			name: "joins the user's latest message",
			items: []*sdkprofilesv1.IdentifyRequest{
				identify(t, "user-1", "anon-1", "", map[string]any{"plan": "free"}),
				identify(t, "user-1", "anon-2", "", map[string]any{"plan": "team"}),
				identify(t, "user-1", "", "", map[string]any{"plan": "pro"}),
			},
			want: []message{
				{externalID: "user-1", anonymousID: "anon-1", traits: map[string]any{"plan": "free"}},
				{externalID: "user-1", anonymousID: "anon-2", traits: map[string]any{"plan": "pro"}},
			},
		},
		{
			name: "an item with an anonymous id stays separate",
			items: []*sdkprofilesv1.IdentifyRequest{
				identify(t, "user-1", "", "", map[string]any{"plan": "free"}),
				identify(t, "user-1", "anon-1", "", map[string]any{"plan": "pro"}),
			},
			want: []message{
				{externalID: "user-1", traits: map[string]any{"plan": "free"}},
				{externalID: "user-1", anonymousID: "anon-1", traits: map[string]any{"plan": "pro"}},
			},
		},
		{
			// Device links act across users: merging the third item into the first
			// would leave device-1 on user-2 instead of user-1.
			name: "a device moving between users keeps its order",
			items: []*sdkprofilesv1.IdentifyRequest{
				identify(t, "user-1", "", "device-1", nil),
				identify(t, "user-2", "", "device-1", nil),
				identify(t, "user-1", "", "device-1", nil),
			},
			want: []message{
				{externalID: "user-1", deviceID: "device-1", traits: map[string]any{}},
				{externalID: "user-2", deviceID: "device-1", traits: map[string]any{}},
				{externalID: "user-1", deviceID: "device-1", traits: map[string]any{}},
			},
		},
		{
			name: "$ip in a merged item is stripped",
			items: []*sdkprofilesv1.IdentifyRequest{
				identify(t, "user-1", "", "", map[string]any{"plan": "pro"}),
				identify(t, "user-1", "", "", map[string]any{geo.PropIP: "203.0.113.7"}),
			},
			want: []message{{externalID: "user-1", traits: map[string]any{"plan": "pro"}}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msgs := publishedIdentifies(t, acceptBatch(t, tc.items...))
			if len(msgs) != len(tc.want) {
				t.Fatalf("published %d messages, want %d: %v", len(msgs), len(tc.want), msgs)
			}
			for i, want := range tc.want {
				got := msgs[i]
				if got.GetExternalId() != want.externalID || got.GetAnonymousId() != want.anonymousID || got.GetDeviceId() != want.deviceID {
					t.Errorf("message %d = %v, want %+v", i, got, want)
				}
				if traits := got.GetTraits().AsMap(); !reflect.DeepEqual(traits, want.traits) {
					t.Errorf("message %d traits = %v, want %v", i, traits, want.traits)
				}
			}
		})
	}
}

// An item breaking an Identify rule costs only itself: it is listed, and the rest
// still publish.
func TestBatchIdentify_SkipsInvalidItems(t *testing.T) {
	spy := &spyJetStream{}
	res, err := NewServer(spy).BatchIdentify(sdkContext("proj-test"), batch(
		identify(t, "user-1", "", "", map[string]any{"plan": "pro"}),
		identify(t, "cookieless-x", "", "", nil),
		identify(t, "user-2", "", "", nil),
		identify(t, "user\x003", "", "", nil),
	))
	if err != nil {
		t.Fatalf("BatchIdentify: %v", err)
	}
	invalid := res.Msg.GetInvalidIdentifies()
	if res.Msg.GetAccepted() != 2 || len(invalid) != 2 || invalid[0].GetIndex() != 1 || invalid[1].GetIndex() != 3 {
		t.Fatalf("accepted=%d invalid_identifies=%v, want 2 accepted and indexes 1 and 3", res.Msg.GetAccepted(), invalid)
	}
	for _, inv := range invalid {
		if v := strings.Join(inv.GetViolations(), "\n"); !strings.HasPrefix(v, "external_id: ") {
			t.Errorf("violations = %q, want one naming external_id", v)
		}
	}
	msgs := publishedIdentifies(t, spy)
	if len(msgs) != 2 || msgs[0].GetExternalId() != "user-1" || msgs[1].GetExternalId() != "user-2" {
		t.Errorf("published %v, want user-1 and user-2", msgs)
	}
}

// Items are checked before merging: {c} would otherwise merge into the bad item's
// message and be skipped with it.
func TestBatchIdentify_SkipsInvalidItemBeforeMerging(t *testing.T) {
	spy := &spyJetStream{}
	res, err := NewServer(spy).BatchIdentify(sdkContext("proj-test"), batch(
		identify(t, "user-1", "", "", map[string]any{"a": "1"}),
		identify(t, "user-1", "", "", map[string]any{"b": "2"}),
		identify(t, "user-1", "nope", "", map[string]any{"x": "9"}),
		identify(t, "user-1", "", "", map[string]any{"c": "3"}),
	))
	if err != nil {
		t.Fatalf("BatchIdentify: %v", err)
	}
	invalid := res.Msg.GetInvalidIdentifies()
	if res.Msg.GetAccepted() != 3 || len(invalid) != 1 || invalid[0].GetIndex() != 2 {
		t.Fatalf("accepted=%d invalid_identifies=%v, want 3 accepted and index 2", res.Msg.GetAccepted(), invalid)
	}
	want := map[string]any{"a": "1", "b": "2", "c": "3"}
	if msgs := publishedIdentifies(t, spy); len(msgs) != 1 || !reflect.DeepEqual(msgs[0].GetTraits().AsMap(), want) {
		t.Errorf("published %v, want one message with traits %v", msgs, want)
	}
}

func TestBatchIdentify_AllInvalidPublishesNothing(t *testing.T) {
	spy := &spyJetStream{}
	res, err := NewServer(spy).BatchIdentify(sdkContext("proj-test"), batch(identify(t, "", "", "", nil)))
	if err != nil {
		t.Fatalf("BatchIdentify: %v", err)
	}
	if res.Msg.GetAccepted() != 0 || len(res.Msg.GetInvalidIdentifies()) != 1 {
		t.Errorf("accepted=%d invalid_identifies=%v, want 0 and one entry", res.Msg.GetAccepted(), res.Msg.GetInvalidIdentifies())
	}
	if msgs := publishedIdentifies(t, spy); len(msgs) != 0 {
		t.Errorf("published %v, want nothing", msgs)
	}
}

// jsonb refuses a NUL and the worker would fail the whole message carrying one, so the
// trait holding it is dropped and the rest of its item merges as usual.
func TestBatchIdentify_DropsTraitsPostgresCannotStore(t *testing.T) {
	for _, tc := range []struct {
		name   string
		traits map[string]any
	}{
		{"in a string", map[string]any{"note": "a\x00b"}},
		{"in a key", map[string]any{"a\x00b": "note"}},
		{"in a nested object", map[string]any{"meta": map[string]any{"note": "a\x00b"}}},
		{"in a list", map[string]any{"tags": []any{"fine", "a\x00b"}}},
		{"in an object in a list", map[string]any{"tags": []any{map[string]any{"note": "a\x00b"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			traits := maps.Clone(tc.traits)
			traits["seen"] = "yes"
			msgs := publishedIdentifies(t, acceptBatch(t,
				identify(t, "user-1", "anon-1", "device-1", map[string]any{"plan": "free"}),
				identify(t, "user-1", "", "", traits),
				identify(t, "user-1", "", "", map[string]any{"plan": "pro"}),
			))
			want := map[string]any{"plan": "pro", "seen": "yes"}
			if len(msgs) != 1 || !reflect.DeepEqual(msgs[0].GetTraits().AsMap(), want) {
				t.Errorf("published %v, want one message with traits %v", msgs, want)
			}
		})
	}
}

// Dropped per item before merging, so a later item's NUL can't erase an earlier value.
func TestBatchIdentify_DroppedTraitKeepsEarlierValue(t *testing.T) {
	msgs := publishedIdentifies(t, acceptBatch(t,
		identify(t, "user-1", "", "", map[string]any{"name": "Ada"}),
		identify(t, "user-1", "", "", map[string]any{"name": "A\x00da"}),
	))
	want := map[string]any{"name": "Ada"}
	if len(msgs) != 1 || !reflect.DeepEqual(msgs[0].GetTraits().AsMap(), want) {
		t.Errorf("published %v, want one message with traits %v", msgs, want)
	}
}

// Merging must not build a message NATS refuses out of items that each fit: that
// would lose deliverable identifies. The item that doesn't fit starts a new message,
// which later items join.
func TestBatchIdentify_MergeStaysPublishable(t *testing.T) {
	blob := func(kib int) string { return strings.Repeat("x", kib<<10) }
	for _, tc := range []struct {
		name     string
		traits   []map[string]any
		wantKeys [][]string
	}{
		{
			name:     "two items that only fit apart",
			traits:   []map[string]any{{"a": blob(600)}, {"b": blob(600)}, {"c": "small"}},
			wantKeys: [][]string{{"a"}, {"b", "c"}},
		},
		{
			name:     "three items that only fit in pairs",
			traits:   []map[string]any{{"a": blob(400)}, {"b": blob(400)}, {"c": blob(400)}},
			wantKeys: [][]string{{"a", "b"}, {"c"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items := make([]*sdkprofilesv1.IdentifyRequest, len(tc.traits))
			for i, traits := range tc.traits {
				items[i] = identify(t, "user-1", "", "", traits)
			}

			spy := acceptBatch(t, items...)
			msgs := publishedIdentifies(t, spy)
			if len(msgs) != len(tc.wantKeys) {
				t.Fatalf("published %d messages, want %d", len(msgs), len(tc.wantKeys))
			}
			for i, want := range tc.wantKeys {
				if size := len(spy.published[i].Data); size >= 1<<20 {
					t.Errorf("message %d is %d bytes, over NATS's default 1 MiB limit", i, size)
				}
				if got := slices.Sorted(maps.Keys(msgs[i].GetTraits().GetFields())); !slices.Equal(got, want) {
					t.Errorf("message %d traits have keys %v, want %v", i, got, want)
				}
			}
		})
	}
}
