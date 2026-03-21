package useragent

import (
	"net/http"

	ua "github.com/medama-io/go-useragent"
)

// Auto-property keys used by the UA parser.
const (
	PropBrowser        = "browser"
	PropBrowserVersion = "browserVersion"
	PropOS             = "os"
	PropDevice         = "device"
)

// Properties is a set of user-agent properties resolved for a request.
type Properties map[string]string

// Parser parses User-Agent headers into device properties.
// Initialize once at application startup via NewParser.
type Parser struct {
	parser *ua.Parser
}

// NewParser creates a new Parser. Call once at startup.
func NewParser() *Parser {
	return &Parser{parser: ua.NewParser()}
}

// Parse extracts device properties from the User-Agent request header.
// Returns an empty map if the header is absent or unrecognised.
func (p *Parser) Parse(h http.Header) Properties {
	uaStr := h.Get("User-Agent")
	if uaStr == "" {
		return nil
	}

	agent := p.parser.Parse(uaStr)

	props := make(Properties, 4)
	if b := string(agent.Browser()); b != "" {
		props[PropBrowser] = b
	}
	if v := agent.BrowserVersion(); v != "" {
		props[PropBrowserVersion] = v
	}
	if o := string(agent.OS()); o != "" {
		props[PropOS] = o
	}
	if d := string(agent.Device()); d != "" {
		props[PropDevice] = d
	}

	if len(props) == 0 {
		return nil
	}

	return props
}
