package geo

import (
	"net/http"
	"testing"
)

func TestCloudflareProvider_Locate(t *testing.T) {
	tests := []struct {
		name    string
		headers http.Header
		want    Location
	}{
		{
			"all headers present",
			http.Header{
				"Cf-Connecting-Ip": {"1.2.3.4"},
				"Cf-Ipcontinent":   {"NA"},
				"Cf-Ipcountry":     {"US"},
				"Cf-Region":        {"California"},
				"Cf-Ipcity":        {"San Francisco"},
				"Cf-Postal-Code":   {"94105"},
				"Cf-Metro-Code":    {"807"},
				"Cf-Iplatitude":    {"37.7749"},
				"Cf-Iplongitude":   {"-122.4194"},
				"Cf-Timezone":      {"America/Los_Angeles"},
			},
			// CF-Connecting-IP is present but the IP is never emitted (never persisted).
			Location{
				PropContinent: "NA", PropCountry: "US",
				PropRegion: "California", PropCity: "San Francisco",
				PropPostalCode: "94105", PropMetroCode: "807",
				PropLatitude: "37.7749", PropLongitude: "-122.4194", PropTimezone: "America/Los_Angeles",
			},
		},
		{
			"no headers",
			http.Header{},
			Location{},
		},
		{
			"partial — country only",
			http.Header{"Cf-Ipcountry": {"DE"}},
			Location{PropCountry: "DE"},
		},
		{
			"all sentinels filtered",
			http.Header{"Cf-Ipcountry": {"XX"}, "Cf-Region": {"Unknown"}, "Cf-Ipcity": {"Unknown"}},
			Location{},
		},
		{
			"sentinel T1 filtered",
			http.Header{"Cf-Ipcountry": {"T1"}},
			Location{},
		},
		{
			"ip header ignored — timezone only",
			http.Header{"Cf-Connecting-Ip": {"8.8.8.8"}, "Cf-Timezone": {"Europe/Berlin"}},
			Location{PropTimezone: "Europe/Berlin"},
		},
		{
			"metro targeting headers",
			http.Header{
				"Cf-Ipcountry":   {"US"},
				"Cf-Postal-Code": {"10001"},
				"Cf-Metro-Code":  {"501"},
			},
			Location{PropCountry: "US", PropPostalCode: "10001", PropMetroCode: "501"},
		},
	}
	p := CloudflareProvider{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := p.Locate(tt.headers)
			if len(got) != len(tt.want) {
				t.Fatalf("Locate() returned %d keys, want %d\ngot:  %v\nwant: %v", len(got), len(tt.want), got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("Locate()[%q] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}
