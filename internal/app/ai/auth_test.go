package ai

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/golang-jwt/jwt/v5"

	coreauth "github.com/pug-sh/pug/internal/core/auth"
)

var testJWTKey = []byte("test-secret")

// mintTestJWT mirrors coreauth.generateJWT's claims (bare RegisteredClaims).
func mintTestJWT(t *testing.T, key []byte, mutate func(*jwt.RegisteredClaims)) string {
	t.Helper()
	claims := jwt.RegisteredClaims{
		Audience:  jwt.ClaimStrings{coreauth.Audience},
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		ID:        "jti_test",
		IssuedAt:  jwt.NewNumericDate(time.Now()),
		Issuer:    coreauth.Issuer,
		Subject:   "cust_test",
	}
	if mutate != nil {
		mutate(&claims)
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return token
}

func authReq(t *testing.T, headers map[string]string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "/ai.dashboards.v1.DashboardAssistantService/Turn", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}

func wantUnauthenticated(t *testing.T, err error, contains string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("not a connect error: %v", err)
	}
	if cerr.Code() != connect.CodeUnauthenticated {
		t.Fatalf("code = %v", cerr.Code())
	}
	if !strings.Contains(cerr.Message(), contains) {
		t.Fatalf("message = %q, want substring %q", cerr.Message(), contains)
	}
}

func TestWithAssistantAuth_MissingAuthorizationHeader(t *testing.T) {
	authFn := WithAssistantAuth(testJWTKey)
	_, err := authFn(context.Background(), authReq(t, map[string]string{"x-project-id": "prj_1"}))
	wantUnauthenticated(t, err, "Authorization header not present")
}

func TestWithAssistantAuth_NonBearerScheme(t *testing.T) {
	authFn := WithAssistantAuth(testJWTKey)
	_, err := authFn(context.Background(), authReq(t, map[string]string{
		"Authorization": "Basic abc", "x-project-id": "prj_1",
	}))
	wantUnauthenticated(t, err, "must start with Bearer")
}

func TestWithAssistantAuth_EmptyBearer(t *testing.T) {
	authFn := WithAssistantAuth(testJWTKey)
	_, err := authFn(context.Background(), authReq(t, map[string]string{
		"Authorization": "Bearer ", "x-project-id": "prj_1",
	}))
	wantUnauthenticated(t, err, "Bearer token is empty")
}

func TestWithAssistantAuth_GarbageToken(t *testing.T) {
	authFn := WithAssistantAuth(testJWTKey)
	_, err := authFn(context.Background(), authReq(t, map[string]string{
		"Authorization": "Bearer not.a.jwt", "x-project-id": "prj_1",
	}))
	wantUnauthenticated(t, err, "invalid authorization")
}

func TestWithAssistantAuth_WrongSecret(t *testing.T) {
	authFn := WithAssistantAuth(testJWTKey)
	token := mintTestJWT(t, []byte("other-secret"), nil)
	_, err := authFn(context.Background(), authReq(t, map[string]string{
		"Authorization": "Bearer " + token, "x-project-id": "prj_1",
	}))
	wantUnauthenticated(t, err, "invalid authorization")
}

func TestWithAssistantAuth_ExpiredToken(t *testing.T) {
	authFn := WithAssistantAuth(testJWTKey)
	token := mintTestJWT(t, testJWTKey, func(c *jwt.RegisteredClaims) {
		c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute))
	})
	_, err := authFn(context.Background(), authReq(t, map[string]string{
		"Authorization": "Bearer " + token, "x-project-id": "prj_1",
	}))
	wantUnauthenticated(t, err, "invalid authorization")
}

func TestWithAssistantAuth_WrongIssuerOrAudience(t *testing.T) {
	authFn := WithAssistantAuth(testJWTKey)
	for name, mutate := range map[string]func(*jwt.RegisteredClaims){
		"issuer":   func(c *jwt.RegisteredClaims) { c.Issuer = "someone/else" },
		"audience": func(c *jwt.RegisteredClaims) { c.Audience = jwt.ClaimStrings{"other/audience"} },
		"no-exp":   func(c *jwt.RegisteredClaims) { c.ExpiresAt = nil },
	} {
		token := mintTestJWT(t, testJWTKey, mutate)
		_, err := authFn(context.Background(), authReq(t, map[string]string{
			"Authorization": "Bearer " + token, "x-project-id": "prj_1",
		}))
		if err == nil {
			t.Fatalf("%s: expected rejection", name)
		}
	}
}

func TestWithAssistantAuth_MissingProjectID(t *testing.T) {
	authFn := WithAssistantAuth(testJWTKey)
	token := mintTestJWT(t, testJWTKey, nil)
	_, err := authFn(context.Background(), authReq(t, map[string]string{
		"Authorization": "Bearer " + token,
	}))
	wantUnauthenticated(t, err, "x-project-id header is required")
}

func TestWithAssistantAuth_ValidTokenYieldsCaller(t *testing.T) {
	authFn := WithAssistantAuth(testJWTKey)
	token := mintTestJWT(t, testJWTKey, nil)
	info, err := authFn(context.Background(), authReq(t, map[string]string{
		"Authorization": "Bearer " + token, "x-project-id": "prj_1",
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	caller, ok := info.(*Caller)
	if !ok {
		t.Fatalf("info is %T", info)
	}
	if caller.JWT != token || caller.ProjectID != "prj_1" {
		t.Fatalf("caller = %+v", caller)
	}
}
