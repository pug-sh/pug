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
	PropMobile         = "$mobile"
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

// Parse extracts browser, OS, and device properties from the User-Agent request
// header using ua-parser, normalizing names and versions to match the web SDK's
// navigator.userAgentData shape (e.g. "Google Chrome", "macOS"). It is the server-side
// fallback for browsers that do not expose navigator.userAgentData — Firefox, Safari,
// and all iOS browsers — which send no Client Hints; for Chromium browsers the SDK
// supplies these properties directly and they take precedence during enrichment.
// Returns nil if the receiver is nil, the User-Agent header is absent, or the
// user-agent string cannot be meaningfully parsed.
func (p *Parser) Parse(h http.Header) Properties {
	if p == nil {
		return nil
	}
	uaStr := h.Get("User-Agent")
	if uaStr == "" {
		return nil
	}
	return propertiesFromUAParser(p.parser.Parse(uaStr), uaStr)
}
