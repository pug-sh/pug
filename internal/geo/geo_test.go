package geo

import (
	"net/http"
	"testing"
)

func TestClientIP(t *testing.T) {
	tests := []struct {
		name    string
		headers http.Header
		want    string
	}{
		{
			"CF-Connecting-IP takes priority",
			http.Header{
				"Cf-Connecting-Ip": {"1.1.1.1"},
				"True-Client-Ip":   {"2.2.2.2"},
				"X-Forwarded-For":  {"3.3.3.3"},
			},
			"1.1.1.1",
		},
		{
			"True-Client-IP fallback",
			http.Header{
				"True-Client-Ip":  {"2.2.2.2"},
				"X-Forwarded-For": {"3.3.3.3"},
			},
			"2.2.2.2",
		},
		{
			"X-Forwarded-For fallback",
			http.Header{"X-Forwarded-For": {"3.3.3.3"}},
			"3.3.3.3",
		},
		{
			"X-Forwarded-For comma-separated — takes first",
			http.Header{"X-Forwarded-For": {"3.3.3.3, 4.4.4.4, 5.5.5.5"}},
			"3.3.3.3",
		},
		{
			"X-Forwarded-For with spaces",
			http.Header{"X-Forwarded-For": {" 3.3.3.3 , 4.4.4.4"}},
			"3.3.3.3",
		},
		{
			"no headers",
			http.Header{},
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClientIP(tt.headers); got != tt.want {
				t.Errorf("ClientIP() = %q, want %q", got, tt.want)
			}
		})
	}
}
