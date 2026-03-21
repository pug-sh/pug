package useragent

import (
	"net/http"

	uaparser "github.com/ua-parser/uap-go/uaparser"
)

// Keys populated in the Properties map returned by Parse.
const (
	PropBrowser        = "browser"
	PropBrowserVersion = "browserVersion"
	PropOS             = "os"
	PropOSVersion      = "osVersion"
	PropDevice         = "device"
)

// Properties is a set of user-agent properties resolved for a request.
type Properties map[string]string

// Parser parses User-Agent headers into device properties.
// Initialize once at application startup via NewParser.
type Parser struct {
	parser *uaparser.Parser
}

// NewParser creates a new Parser. Call once at startup.
func NewParser() (*Parser, error) {
	p, err := uaparser.New()
	if err != nil {
		return nil, err
	}
	return &Parser{parser: p}, nil
}

// Parse extracts device properties from the User-Agent request header.
// Returns nil if the header is absent.
func (p *Parser) Parse(h http.Header) Properties {
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
