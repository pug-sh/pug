package profiles

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"connectrpc.com/authn"
	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"
	"github.com/jackc/pgx/v5"
	"github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/core/authz"
	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	sdkprofilesv1 "github.com/pug-sh/pug/internal/gen/proto/sdk/profiles/v1"
	"github.com/pug-sh/pug/internal/gen/proto/sdk/profiles/v1/sdkprofilesv1connect"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
)

const (
	servedPublicKey  = "pub_served"
	servedPrivateKey = "prv_served"
)

type stubKeyLookup struct{}

func (stubKeyLookup) GetProjectByPublicApiKey(_ context.Context, key string) (dbread.Project, error) {
	if key == servedPublicKey {
		return dbread.Project{ID: "proj-test"}, nil
	}
	return dbread.Project{}, pgx.ErrNoRows
}

func (stubKeyLookup) GetProjectByPrivateApiKey(_ context.Context, key string) (dbread.Project, error) {
	if key == servedPrivateKey {
		return dbread.Project{ID: "proj-test"}, nil
	}
	return dbread.Project{}, pgx.ErrNoRows
}

func (stubKeyLookup) InvalidateProjectKeys(context.Context, string, ...string) {}

// stubRoleLookup satisfies the authz interceptor. An API-key request skips the role
// check, so this must never run.
type stubRoleLookup struct{}

func (stubRoleLookup) GetMemberRole(context.Context, string, string) (coreorgs.Role, error) {
	return coreorgs.Role(""), errors.New("role lookup must not run on an API-key request")
}

// serve mounts the handler as server.start does, so a request crosses the real SDK
// auth and production interceptors.
func serve(t *testing.T, spy *spyJetStream) sdkprofilesv1connect.ProfilesSDKServiceClient {
	t.Helper()
	authorizer, err := authz.NewAuthorizer()
	if err != nil {
		t.Fatalf("authz.NewAuthorizer: %v", err)
	}
	otelInterceptor, err := otelconnect.NewInterceptor()
	if err != nil {
		t.Fatalf("otelconnect.NewInterceptor: %v", err)
	}
	path, handler := sdkprofilesv1connect.NewProfilesSDKServiceHandler(NewServer(spy),
		rpc.HandlerOptions(otelInterceptor, authorizer, stubRoleLookup{}))

	mux := http.NewServeMux()
	mux.Handle(path, rpc.WithSDKCORS(authn.NewMiddleware(rpc.WithSDKAuth(stubKeyLookup{})).Wrap(handler)))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return sdkprofilesv1connect.NewProfilesSDKServiceClient(srv.Client(), srv.URL)
}

// sdk-go falls back to Identify on Unimplemented, so a BatchIdentify that stopped being
// reachable would go unnoticed. A refused request must also publish nothing.
func TestBatchIdentify_Served(t *testing.T) {
	valid := identify(t, "user-1", "", "", map[string]any{"plan": "pro"})
	for _, tc := range []struct {
		name          string
		key           string
		items         []*sdkprofilesv1.IdentifyRequest
		wantCode      connect.Code // 0 means success
		wantPublished int
		wantSkipped   int
	}{
		{name: "public key", key: servedPublicKey, items: []*sdkprofilesv1.IdentifyRequest{valid}, wantPublished: 1},
		{name: "private key", key: servedPrivateKey, items: []*sdkprofilesv1.IdentifyRequest{valid}, wantPublished: 1},
		{name: "no key", items: []*sdkprofilesv1.IdentifyRequest{valid}, wantCode: connect.CodeUnauthenticated},
		{name: "unknown key", key: "pub_unknown", items: []*sdkprofilesv1.IdentifyRequest{valid}, wantCode: connect.CodeUnauthenticated},
		{
			name:          "one invalid item",
			key:           servedPrivateKey,
			items:         []*sdkprofilesv1.IdentifyRequest{valid, identify(t, "cookieless-x", "", "", nil)},
			wantPublished: 1,
			wantSkipped:   1,
		},
		{
			name:          "one empty external id",
			key:           servedPrivateKey,
			items:         []*sdkprofilesv1.IdentifyRequest{valid, identify(t, "", "", "", nil)},
			wantPublished: 1,
			wantSkipped:   1,
		},
		{
			name:     "1001 items",
			key:      servedPrivateKey,
			items:    slices.Repeat([]*sdkprofilesv1.IdentifyRequest{valid}, 1001),
			wantCode: connect.CodeInvalidArgument,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spy := &spyJetStream{}
			client := serve(t, spy)
			req := batch(tc.items...)
			if tc.key != "" {
				req.Header().Set(rpc.HeaderAPIKey, tc.key)
			}

			res, err := client.BatchIdentify(context.Background(), req)

			switch {
			case tc.wantCode != 0:
				if got := connect.CodeOf(err); err == nil || got != tc.wantCode {
					t.Fatalf("code = %v (err %v), want %v", got, err, tc.wantCode)
				}
			case err != nil:
				t.Fatalf("unexpected error: %v", err)
			default:
				if got, want := res.Msg.GetAccepted(), uint32(len(tc.items)-tc.wantSkipped); got != want {
					t.Errorf("accepted = %d, want %d", got, want)
				}
				if got := len(res.Msg.GetInvalidIdentifies()); got != tc.wantSkipped {
					t.Errorf("invalid_identifies has %d entries, want %d", got, tc.wantSkipped)
				}
			}
			if msgs := publishedIdentifies(t, spy); len(msgs) != tc.wantPublished {
				t.Errorf("published %d messages, want %d", len(msgs), tc.wantPublished)
			}
		})
	}
}
