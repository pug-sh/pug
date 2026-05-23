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

// Parse extracts browser, OS, and device properties from User-Agent Client Hints
// request headers when present, then fills any remaining gaps from the User-Agent
// header using ua-parser with values normalized to match the web SDK's UA-CH shape.
// Returns nil if the receiver is nil or no properties could be resolved.
func (p *Parser) Parse(h http.Header) Properties {
	if p == nil {
		return nil
	}

	props := parseClientHints(h)

	uaStr := h.Get("User-Agent")
	if uaStr != "" {
		uaProps := propertiesFromUAParser(p.parser.Parse(uaStr), uaStr)
		if props == nil {
			props = uaProps
		} else if uaProps != nil {
			for k, v := range uaProps {
				if _, exists := props[k]; !exists {
					props[k] = v
				}
			}
		}
	}

	if len(props) == 0 {
		return nil
	}
	return props
}
