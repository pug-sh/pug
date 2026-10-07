// Package domainname normalizes every domain SSO compares: verified domains,
// emailDomains, Google's hd and email addresses.
package domainname

import (
	"errors"
	"strings"
	"unicode/utf8"
)

var ErrInvalid = errors.New("not a valid domain name")

// Normalize trims and lowercases raw, drops one trailing dot, and requires an ASCII
// hostname of at least two labels whose last label is not all digits.
func Normalize(raw string) (string, error) {
	// Before lowercasing, which folds some non-ASCII runes into ASCII (the Kelvin sign into k).
	if strings.ContainsFunc(raw, func(r rune) bool { return r >= utf8.RuneSelf }) {
		return "", ErrInvalid
	}
	d := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
	if d == "" || len(d) > 253 {
		return "", ErrInvalid
	}
	labels := strings.Split(d, ".")
	if len(labels) < 2 {
		return "", ErrInvalid
	}
	for _, label := range labels {
		if !validLabel(label) {
			return "", ErrInvalid
		}
	}
	if strings.Trim(labels[len(labels)-1], "0123456789") == "" {
		return "", ErrInvalid
	}
	return d, nil
}

// Of returns the normalized domain of an email address, or "" when it has none.
func Of(email string) string {
	at := strings.LastIndexByte(email, '@')
	if at < 0 {
		return ""
	}
	d, err := Normalize(email[at+1:])
	if err != nil {
		return ""
	}
	return d
}

func validLabel(label string) bool {
	if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for i := range len(label) {
		c := label[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}
