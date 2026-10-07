package domainname_test

import (
	"errors"
	"testing"

	"github.com/pug-sh/pug/internal/domainname"
)

func TestNormalize(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"acme.com", "acme.com"},
		{" ACME.com. ", "acme.com"},
		{"eng.acme.co.uk", "eng.acme.co.uk"},
		{"xn--bcher-kva.de", "xn--bcher-kva.de"},
		{"a-b.io", "a-b.io"},
	} {
		got, err := domainname.Normalize(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

func TestNormalizeRejects(t *testing.T) {
	for _, in := range []string{
		"", "localhost", "com", ".acme.com", "acme..com", "-acme.com", "acme-.com",
		"acme.com..", "acme_corp.com", "bücher.de", "1.2.3.4", "acme.com/x", "user@acme.com",
		"Kacme.com", "İbm.com",
	} {
		if got, err := domainname.Normalize(in); !errors.Is(err, domainname.ErrInvalid) {
			t.Errorf("Normalize(%q) = %q, %v; want ErrInvalid", in, got, err)
		}
	}
}

func TestOf(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"bob@Acme.com", "acme.com"},
		{"weird@name@acme.io", "acme.io"},
		{"no-at-sign", ""},
		{"bob@localhost", ""},
		{"bob@", ""},
		{"eve@Key.com", ""},
	} {
		if got := domainname.Of(tc.in); got != tc.want {
			t.Errorf("Of(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
