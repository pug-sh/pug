package useragent

import (
	"fmt"
	"net/http"

	uaparser "github.com/ua-parser/uap-go/uaparser"
)

// Keys populated in the Properties map returned by Parse.
const (
	PropBrowser        = "$browser"
	PropBrowserVersion = "$browserVersion"
	PropOS             = "$os"
	PropOSVersion      = "$osVersion"
	PropDevice         = "$device"
)

// Properties is a set of user-agent properties resolved for a request.
type Properties map[string]string

// Parser parses User-Agent headers into browser, OS, and device properties.
// Initialize once at application startup via NewParser.
type Parser struct {
	parser *uaparser.Parser
}

// NewParser creates a new Parser. Call once at startup.
func NewParser() (*Parser, error) {
	p, err := uaparser.New()
	if err != nil {
		return nil, fmt.Errorf("useragent: failed to initialize parser: %w", err)
	}
	return &Parser{parser: p}, nil
}

// Parse extracts browser, OS, and device properties from the User-Agent request header.
// Returns nil if the receiver is nil, the header is absent, or the user-agent string
// cannot be meaningfully parsed.
func (p *Parser) Parse(h http.Header) Properties {
	if p == nil {
		return nil
	}
	uaStr := h.Get("User-Agent")
	if uaStr == "" {
		return nil
	}

	client := p.parser.Parse(uaStr)

	props := make(Properties, 5)
	if client.UserAgent.Family != "" && client.UserAgent.Family != "Other" {
		props[PropBrowser] = client.UserAgent.Family
		if client.UserAgent.Major != "" {
			props[PropBrowserVersion] = client.UserAgent.Major
		}
	}
	if client.Os.Family != "" && client.Os.Family != "Other" {
		props[PropOS] = client.Os.Family
		if client.Os.Major != "" {
			props[PropOSVersion] = client.Os.Major
		}
	}
	if client.Device.Family != "" && client.Device.Family != "Other" {
		props[PropDevice] = client.Device.Family
	}

	if len(props) == 0 {
		return nil
	}

	return props
}
