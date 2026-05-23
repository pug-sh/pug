package useragent

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

const (
	headerSecCHUA                = "Sec-Ch-Ua"
	headerSecCHUAMobile          = "Sec-Ch-Ua-Mobile"
	headerSecCHUAPlatform        = "Sec-Ch-Ua-Platform"
	headerSecCHUAPlatformVersion = "Sec-Ch-Ua-Platform-Version"
	headerSecCHUAModel           = "Sec-Ch-Ua-Model"
)

var secCHBrandRe = regexp.MustCompile(`"([^"]+)";v="([^"]+)"`)

// parseClientHints extracts low- and high-entropy User-Agent Client Hints request
// headers into the same property shape the web SDK reads from navigator.userAgentData.
func parseClientHints(h http.Header) Properties {
	chUA := h.Get(headerSecCHUA)
	if chUA == "" {
		return nil
	}

	props := make(Properties)

	for _, match := range secCHBrandRe.FindAllStringSubmatch(chUA, -1) {
		brand := match[1]
		if strings.HasPrefix(brand, "Not") {
			continue
		}
		props[PropBrowser] = brand
		props[PropBrowserVersion] = match[2]
		break
	}

	if platform := unquoteCH(h.Get(headerSecCHUAPlatform)); platform != "" && !strings.EqualFold(platform, "unknown") {
		props[PropOS] = platform
	}

	if platformVersion := unquoteCH(h.Get(headerSecCHUAPlatformVersion)); platformVersion != "" {
		props[PropOSVersion] = platformVersion
	}

	if model := unquoteCH(h.Get(headerSecCHUAModel)); model != "" {
		props[PropDevice] = model
	}

	if mobile := h.Get(headerSecCHUAMobile); mobile != "" {
		props[PropMobile] = secCHMobile(mobile)
	}

	if len(props) == 0 {
		return nil
	}
	return props
}

func unquoteCH(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

func secCHMobile(v string) string {
	v = strings.TrimPrefix(strings.TrimSpace(v), "?")
	if b, err := strconv.ParseBool(v); err == nil {
		return strconv.FormatBool(b)
	}
	return v
}
