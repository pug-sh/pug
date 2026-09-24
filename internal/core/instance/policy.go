package instance

import (
	"errors"
	"net/mail"
	"sort"
	"strings"
)

// Policy contains deployment-wide account and organization rules. It is immutable
// after startup so all requests handled by one server replica use the same rules.
type Policy struct {
	mode   string
	admins map[string]struct{}
}

func OpenPolicy() Policy { return Policy{mode: "open"} }

// ParsePolicy accepts the two deployment settings and fails startup for ambiguous
// or unusable admin configuration. Addresses are compared case-insensitively.
func ParsePolicy(mode, emailList string) (Policy, error) {
	if mode == "" {
		mode = "open"
	}
	if mode != "open" && mode != "managed" {
		return Policy{}, errors.New("PUG_ORG_CREATION_MODE must be open or managed")
	}
	p := Policy{mode: mode, admins: make(map[string]struct{})}
	if strings.TrimSpace(emailList) != "" {
		for raw := range strings.SplitSeq(emailList, ",") {
			email := strings.TrimSpace(raw)
			address, err := mail.ParseAddress(email)
			if err != nil || address.Address != email {
				return Policy{}, errors.New("PUG_INSTANCE_ADMIN_EMAILS contains an invalid address")
			}
			email = strings.ToLower(email)
			if _, exists := p.admins[email]; exists {
				return Policy{}, errors.New("PUG_INSTANCE_ADMIN_EMAILS contains a duplicate address")
			}
			p.admins[email] = struct{}{}
		}
	}
	if mode == "managed" && len(p.admins) == 0 {
		return Policy{}, errors.New("PUG_INSTANCE_ADMIN_EMAILS is required in managed mode")
	}
	return p, nil
}

func (p Policy) Managed() bool { return p.mode == "managed" }

func (p Policy) Mode() string {
	if p.mode == "" {
		return "open"
	}
	return p.mode
}

func (p Policy) AllowsAdmin(email string) bool {
	_, ok := p.admins[strings.ToLower(email)]
	return ok
}

// AdminEmails is for server-side enforcement. Never include it in an RPC response.
func (p Policy) AdminEmails() []string {
	emails := make([]string, 0, len(p.admins))
	for email := range p.admins {
		emails = append(emails, email)
	}
	sort.Strings(emails)
	return emails
}
