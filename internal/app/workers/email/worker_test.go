package email

import "testing"

// TestNeedsTenantCache pins the operator-only boot path: when
// PUG_EMAIL_PROVIDER_SECRET_KEY is unset the worker must skip Redis
// connection setup. A regression that always returned true would re-introduce
// the Redis dependency that commit 8d2ff34 explicitly removed.
func TestNeedsTenantCache(t *testing.T) {
	cases := []struct {
		name string
		key  string
		want bool
	}{
		{"empty_operator_only", "", false},
		{"set_tenant_cache_needed", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := needsTenantCache(tc.key); got != tc.want {
				t.Fatalf("needsTenantCache(%q) = %v, want %v", tc.key, got, tc.want)
			}
		})
	}
}
