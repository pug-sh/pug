package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"maps"
	"math/big"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	coreauth "github.com/pug-sh/pug/internal/core/auth"
	coreoauth "github.com/pug-sh/pug/internal/core/auth/oauth"
	"github.com/pug-sh/pug/internal/core/email/secret"
	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
)

const connNonce = "nonce-0123456789abcdef"

// fakeIdP is an OIDC provider that signs whatever claims the test sets next.
type fakeIdP struct {
	srv *httptest.Server
	key *rsa.PrivateKey

	mu      sync.Mutex
	next    jwt.MapClaims
	secrets []string
	// Runs while a sign-in is exchanging its code.
	onToken func()
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &fakeIdP{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                                idp.srv.URL,
			"authorization_endpoint":                idp.srv.URL + "/auth",
			"token_endpoint":                        idp.srv.URL + "/token",
			"jwks_uri":                              idp.srv.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "k1", "use": "sig", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		idp.mu.Lock()
		onToken := idp.onToken
		idp.mu.Unlock()
		if onToken != nil {
			onToken()
		}
		idp.mu.Lock()
		secret := r.Form.Get("client_secret")
		if _, basic, ok := r.BasicAuth(); ok {
			secret = basic
		}
		idp.secrets = append(idp.secrets, secret)
		claims := jwt.MapClaims{
			"iss": idp.srv.URL, "aud": "client", "nonce": connNonce,
			"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
		}
		maps.Copy(claims, idp.next)
		idp.mu.Unlock()
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		token.Header["kid"] = "k1"
		signed, err := token.SignedString(key)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"access_token": "a", "token_type": "Bearer", "id_token": signed})
	})
	idp.srv = httptest.NewServer(mux)
	t.Cleanup(idp.srv.Close)
	return idp
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (idp *fakeIdP) lastSecret() string {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	return idp.secrets[len(idp.secrets)-1]
}

type connFixture struct {
	*ssoFixture
	auth   *coreauth.Service
	orgID  string
	cipher *secret.Cipher
	client *http.Client
	// The paths the connection client fetched.
	fetched *pathRecorder
}

type pathRecorder struct {
	base  http.RoundTripper
	mu    sync.Mutex
	paths []string
}

func (r *pathRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.paths = append(r.paths, req.URL.Path)
	r.mu.Unlock()
	return r.base.RoundTrip(req)
}

func (r *pathRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paths = nil
}

func (r *pathRecorder) saw(path string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Contains(r.paths, path)
}

func newConnFixture(t *testing.T, domains ...string) (*connFixture, map[string]string) {
	t.Helper()
	f := newSSOFixture(t)
	cipher, err := secret.NewCipher(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))))
	if err != nil {
		t.Fatal(err)
	}
	// Loopback is the fake IdP's address.
	client := coreoauth.NewConnectionHTTPClient(true)
	fetched := &pathRecorder{base: client.Transport}
	client.Transport = fetched
	f.orgs.WithSSOConnections(cipher, func(ctx context.Context, issuer string) error {
		return coreoauth.DiscoverIssuer(ctx, client, issuer)
	})
	authSvc, err := coreauth.NewServiceForTest(f.ctx, f.db.PgRO, f.db.PgW, []byte("test-secret-key-for-jwt"), &stubPublisher{})
	if err != nil {
		t.Fatal(err)
	}
	authSvc.WithSSOConnections(cipher, client)

	orgID := f.org(domains[0], coreorgs.DomainSettings{MembersCanCreateOrgs: true})
	ids := map[string]string{}
	for _, d := range domains {
		claim, err := f.orgs.VerifyDomainByOperator(f.ctx, orgID, d)
		if err != nil {
			t.Fatal(err)
		}
		ids[d] = claim.ID
	}
	return &connFixture{ssoFixture: f, auth: authSvc, orgID: orgID, cipher: cipher, client: client, fetched: fetched}, ids
}

func (f *connFixture) connect(idp *fakeIdP, id, secret string, domainIDs ...string) coreorgs.SSOConnection {
	f.t.Helper()
	c, err := f.orgs.SetSSOConnection(f.ctx, f.orgID, coreorgs.SSOConnectionInput{
		ID: id, Label: "Acme SSO", IssuerURL: idp.srv.URL, ClientID: "client", ClientSecret: secret, DomainIDs: domainIDs,
	})
	if err != nil {
		f.t.Fatalf("SetSSOConnection: %v", err)
	}
	return c
}

func (f *connFixture) signIn(idp *fakeIdP, connID string, claims jwt.MapClaims) (coreauth.Session, error) {
	idp.mu.Lock()
	idp.next = claims
	idp.mu.Unlock()
	return f.auth.CompleteConnectionSignIn(f.ctx, connID, coreoauth.AuthorizationCode{
		Code: "code", CodeVerifier: strings.Repeat("v", 43), RedirectURI: "https://pug.example.com/oauth/callback", Nonce: connNonce,
	}, "", "")
}

func (f *connFixture) customerID(email string) string {
	f.t.Helper()
	c, err := f.read.GetCustomerByEmail(f.ctx, email)
	if err != nil {
		f.t.Fatalf("get customer %s: %v", email, err)
	}
	return c.ID
}

// A connection signs in only its listed domains, and an edit takes effect at the next sign-in.
func TestConnectionSignsInOnlyItsDomains(t *testing.T) {
	f, ids := newConnFixture(t, "acme.com", "globex.com")
	idp := newFakeIdP(t)
	conn := f.connect(idp, "", "secret-1", ids["acme.com"])

	d, err := f.auth.DiscoverSignIn(f.ctx, "bob@ACME.com")
	if err != nil || d.Domain != "acme.com" || d.RequireSSO || len(d.Providers) != 1 || d.Providers[0].ConnectionID != conn.ID {
		t.Fatalf("DiscoverSignIn = %+v, %v; want the connection", d, err)
	}
	if d, err := f.auth.DiscoverSignIn(f.ctx, "carol@globex.com"); err != nil || len(d.Providers) != 0 {
		t.Fatalf("DiscoverSignIn(globex) = %+v, %v; want no provider", d, err)
	}

	session, err := f.signIn(idp, conn.ID, jwt.MapClaims{"sub": "s-bob", "email": "bob@acme.com"})
	if err != nil {
		t.Fatalf("sign-in without email_verified on a listed domain: %v", err)
	}
	if got := f.provenDomainOf(session.RefreshToken); got != "acme.com" {
		t.Fatalf("proven domain = %q, want acme.com", got)
	}
	for _, email := range []string{"carol@globex.com", "jane@gmail.com"} {
		_, err := f.signIn(idp, conn.ID, jwt.MapClaims{"sub": "s-" + email, "email": email, "email_verified": true})
		if !errors.Is(err, coreoauth.ErrEmailNotOnConnection) {
			t.Fatalf("%s: err = %v, want ErrEmailNotOnConnection", email, err)
		}
	}

	if idp.lastSecret() != "secret-1" {
		t.Fatalf("secret = %q", idp.lastSecret())
	}
	f.connect(idp, conn.ID, "secret-2", ids["acme.com"])
	if _, err := f.signIn(idp, conn.ID, jwt.MapClaims{"sub": "s-bob", "email": "bob@acme.com"}); err != nil {
		t.Fatal(err)
	}
	if idp.lastSecret() != "secret-2" {
		t.Fatalf("secret after the edit = %q, want secret-2", idp.lastSecret())
	}

	// The connection's sign-in counts for Require SSO.
	if _, err := f.orgs.UpdateDomain(f.ctx, f.orgID, ids["acme.com"], true); err != nil {
		t.Fatalf("UpdateDomain: %v", err)
	}
	if d, err := f.auth.DiscoverSignIn(f.ctx, "bob@acme.com"); err != nil || !d.RequireSSO {
		t.Fatalf("DiscoverSignIn = %+v, %v; want Require SSO", d, err)
	}
	if _, err := f.signIn(idp, conn.ID, jwt.MapClaims{"sub": "s-bob", "email": "bob@acme.com"}); err != nil {
		t.Fatalf("sign-in under Require SSO: %v", err)
	}
}

func TestConnectionCantReachAnAccountOffItsDomains(t *testing.T) {
	f, ids := newConnFixture(t, "acme.com", "globex.com")
	idp := newFakeIdP(t)
	conn := f.connect(idp, "", "secret", ids["acme.com"], ids["globex.com"])
	if _, err := f.signIn(idp, conn.ID, jwt.MapClaims{"sub": "s-carol", "email": "carol@globex.com"}); err != nil {
		t.Fatal(err)
	}

	f.connect(idp, conn.ID, "", ids["acme.com"])
	_, err := f.signIn(idp, conn.ID, jwt.MapClaims{"sub": "s-carol", "email": "mallory@acme.com"})
	if !errors.Is(err, coreoauth.ErrEmailNotOnConnection) {
		t.Fatalf("err = %v, want ErrEmailNotOnConnection", err)
	}
}

func TestConnectionIssuerChangeDoesNotReuseSubs(t *testing.T) {
	f, ids := newConnFixture(t, "acme.com")
	oldIdP := newFakeIdP(t)
	conn := f.connect(oldIdP, "", "secret", ids["acme.com"])
	if _, err := f.signIn(oldIdP, conn.ID, jwt.MapClaims{"sub": "1", "email": "bob@acme.com"}); err != nil {
		t.Fatal(err)
	}
	bob := f.customerID("bob@acme.com")

	newIdP := newFakeIdP(t)
	f.connect(newIdP, conn.ID, "secret-2", ids["acme.com"])
	if _, err := f.signIn(newIdP, conn.ID, jwt.MapClaims{"sub": "1", "email": "eve@acme.com"}); err != nil {
		t.Fatal(err)
	}
	if eve := f.customerID("eve@acme.com"); eve == bob {
		t.Fatal("the new issuer's sub 1 reached the old issuer's account")
	}
	if _, err := f.signIn(newIdP, conn.ID, jwt.MapClaims{"sub": "9", "email": "bob@acme.com"}); err != nil {
		t.Fatal(err)
	}
	var linked string
	if err := f.db.PgW.QueryRow(f.ctx, "select customer_id from customer_identities where provider = $1 and provider_subject = '9'",
		string(coreoauth.ConnectionProviderName(conn.ID))).Scan(&linked); err != nil || linked != bob {
		t.Fatalf("bob relinked to %q, %v; want %q", linked, err, bob)
	}
}

func TestConnectionSignInOffOrUnknown(t *testing.T) {
	f, ids := newConnFixture(t, "acme.com")
	idp := newFakeIdP(t)
	conn := f.connect(idp, "", "secret", ids["acme.com"])

	off, err := coreauth.NewServiceForTest(f.ctx, f.db.PgRO, f.db.PgW, []byte("test-secret-key-for-jwt"), &stubPublisher{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := off.CompleteConnectionSignIn(f.ctx, conn.ID, coreoauth.AuthorizationCode{}, "", ""); !errors.Is(err, coreoauth.ErrOAuthProviderDisabled) {
		t.Fatalf("connections off: err = %v, want ErrOAuthProviderDisabled", err)
	}
	if d, err := off.DiscoverSignIn(f.ctx, "bob@acme.com"); err != nil || len(d.Providers) != 0 {
		t.Fatalf("connections off: DiscoverSignIn = %+v, %v; want no connection", d, err)
	}
	if _, err := f.signIn(idp, strings.Repeat("0", 20), jwt.MapClaims{}); !errors.Is(err, coreoauth.ErrOAuthProviderDisabled) {
		t.Fatalf("unknown connection: err = %v, want ErrOAuthProviderDisabled", err)
	}
}

// Discovery, the token endpoint and the signing keys all go through the connection
// client, which holds the address guard.
func TestConnectionFetchesUseTheConnectionClient(t *testing.T) {
	f, ids := newConnFixture(t, "acme.com")
	idp := newFakeIdP(t)
	conn := f.connect(idp, "", "secret", ids["acme.com"])
	f.fetched.reset()
	if _, err := f.signIn(idp, conn.ID, jwt.MapClaims{"sub": "s-bob", "email": "bob@acme.com"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/.well-known/openid-configuration", "/token", "/jwks"} {
		if !f.fetched.saw(p) {
			t.Errorf("%s did not go through the connection client", p)
		}
	}
}

// A change that commits while a sign-in exchanges its code wins, so the sign-in can't
// relink a sub the change unlinked or prove a domain it took off.
func TestConnectionChangeDuringSignIn(t *testing.T) {
	for _, change := range []string{"new issuer", "domain taken off", "deleted"} {
		t.Run(change, func(t *testing.T) {
			f, ids := newConnFixture(t, "acme.com", "globex.com")
			oldIdP, newIdP := newFakeIdP(t), newFakeIdP(t)
			conn := f.connect(oldIdP, "", "secret", ids["acme.com"], ids["globex.com"])
			if _, err := f.signIn(oldIdP, conn.ID, jwt.MapClaims{"sub": "1", "email": "bob@acme.com"}); err != nil {
				t.Fatal(err)
			}
			changed := make(chan error, 1)
			oldIdP.mu.Lock()
			oldIdP.onToken = func() {
				in := coreorgs.SSOConnectionInput{ID: conn.ID, Label: "Acme SSO", IssuerURL: oldIdP.srv.URL, ClientID: "client", DomainIDs: []string{ids["acme.com"], ids["globex.com"]}}
				switch change {
				case "new issuer":
					in.IssuerURL, in.ClientSecret = newIdP.srv.URL, "secret-2"
				case "domain taken off":
					in.DomainIDs = []string{ids["globex.com"]}
				default:
					changed <- f.orgs.DeleteSSOConnection(f.ctx, f.orgID, conn.ID)
					return
				}
				_, err := f.orgs.SetSSOConnection(f.ctx, f.orgID, in)
				changed <- err
			}
			oldIdP.mu.Unlock()
			_, err := f.signIn(oldIdP, conn.ID, jwt.MapClaims{"sub": "1", "email": "bob@acme.com"})
			if changeErr := <-changed; changeErr != nil {
				t.Fatal(changeErr)
			}
			if !errors.Is(err, coreoauth.ErrOAuthProviderDisabled) {
				t.Fatalf("err = %v, want ErrOAuthProviderDisabled", err)
			}
			if change == "domain taken off" {
				return
			}
			var n int
			if err := f.db.PgW.QueryRow(f.ctx, "select count(*) from customer_identities where provider = $1",
				string(coreoauth.ConnectionProviderName(conn.ID))).Scan(&n); err != nil || n != 0 {
				t.Fatalf("linked identities = %d, %v; want 0", n, err)
			}
		})
	}
}

// With Google configured, as on cloud, a domain without a connection or a listed
// provider gets Google, and a domain with a connection gets only the connection.
func TestDiscoverSignInWithGoogleConfigured(t *testing.T) {
	f, ids := newConnFixture(t, "acme.com")
	conn := f.connect(newFakeIdP(t), "", "secret", ids["acme.com"])
	cfg := coreoauth.Config{Providers: []coreoauth.ProviderConfig{
		{ID: "google", Type: coreoauth.ProviderTypeOIDC, ClientID: "g", IssuerURL: "https://accounts.google.com"},
	}}
	svc, err := coreauth.NewService(f.ctx, f.db.PgRO, f.db.PgW, []byte("test-secret-key-for-jwt"), &stubPublisher{}, cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	svc.WithSSOConnections(f.cipher, f.client)

	d, err := svc.DiscoverSignIn(f.ctx, "bob@acme.com")
	if err != nil || len(d.Providers) != 1 || d.Providers[0].ConnectionID != conn.ID {
		t.Fatalf("acme.com: %+v, %v; want only the connection", d, err)
	}
	d, err = svc.DiscoverSignIn(f.ctx, "carol@globex.com")
	if err != nil || len(d.Providers) != 1 || d.Providers[0].Config.ID != "google" {
		t.Fatalf("globex.com: %+v, %v; want Google", d, err)
	}
	if d, err := svc.DiscoverSignIn(f.ctx, "dave@localhost"); err != nil || d.Domain != "" || len(d.Providers) != 0 {
		t.Fatalf("localhost: %+v, %v; want no domain and no provider", d, err)
	}

	// A failed connection lookup still returns the config providers.
	canceled, cancel := context.WithCancel(f.ctx)
	cancel()
	if p, err := svc.ProvidersFor(canceled, "acme.com"); err == nil || len(p) != 1 || p[0].Config.ID != "google" {
		t.Fatalf("failed lookup: %+v, %v; want Google and the error", p, err)
	}
}

// An issuer change still in flight when a first sign-in reaches its re-check wins: the share
// lock makes the sign-in wait, so it can't keep a link the change deleted.
func TestConnectionIssuerChangeInFlight(t *testing.T) {
	f, ids := newConnFixture(t, "acme.com")
	idp := newFakeIdP(t)
	conn := f.connect(idp, "", "secret", ids["acme.com"])
	provider := string(coreoauth.ConnectionProviderName(conn.ID))

	// What SetSSOConnection does on a new issuer, held open.
	change, err := f.db.PgW.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = change.Rollback(f.ctx) }()
	for _, q := range []string{
		"select 1 from org_sso_connections where id = $1 for update",
		"update org_sso_connections set issuer_url = 'https://new.example.com' where id = $1",
	} {
		if _, err := change.Exec(f.ctx, q, conn.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := change.Exec(f.ctx, "delete from customer_identities where provider = $1", provider); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := f.signIn(idp, conn.ID, jwt.MapClaims{"sub": "1", "email": "bob@acme.com"})
		done <- err
	}()
	for start := time.Now(); ; {
		var waiting bool
		if err := f.db.PgW.QueryRow(f.ctx, "select exists (select 1 from pg_locks where not granted)").Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("sign-in finished while the issuer change was in flight: err = %v", err)
		case <-time.After(10 * time.Millisecond):
		}
		if time.Since(start) > 10*time.Second {
			t.Fatal("sign-in neither waited nor finished")
		}
	}
	if err := change.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, coreoauth.ErrOAuthProviderDisabled) {
		t.Fatalf("err = %v, want ErrOAuthProviderDisabled", err)
	}
	var n int
	if err := f.db.PgW.QueryRow(f.ctx, "select count(*) from customer_identities where provider = $1", provider).Scan(&n); err != nil || n != 0 {
		t.Fatalf("linked identities = %d, %v; want 0", n, err)
	}
}
