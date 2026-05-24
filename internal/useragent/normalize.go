package useragent

import (
	"strings"

	uaparser "github.com/ua-parser/uap-go/uaparser"
)

var browserFamilyNames = map[string]string{
	"Chrome":        "Google Chrome",
	"Chrome Mobile": "Google Chrome",
	"Edge":          "Microsoft Edge",
	"Edge Mobile":   "Microsoft Edge",
	"Mobile Safari": "Safari",
}

var osFamilyNames = map[string]string{
	"Mac OS X": "macOS",
}

// linuxFamilies are ua-parser OS families that navigator.userAgentData collapses to
// "Linux". Chrome OS is intentionally excluded — the SDK reports it as its own platform.
var linuxFamilies = map[string]bool{
	"Ubuntu":     true,
	"Debian":     true,
	"Fedora":     true,
	"Red Hat":    true,
	"CentOS":     true,
	"SUSE":       true,
	"openSUSE":   true,
	"Gentoo":     true,
	"Arch Linux": true,
	"Linux Mint": true,
	"Mageia":     true,
	"Mandriva":   true,
	"Slackware":  true,
	"PCLinuxOS":  true,
	"Kubuntu":    true,
}

func normalizeBrowserFamily(family string) string {
	if name, ok := browserFamilyNames[family]; ok {
		return name
	}
	return family
}

func normalizeOSFamily(family string) string {
	if name, ok := osFamilyNames[family]; ok {
		return name
	}
	if linuxFamilies[family] {
		return "Linux"
	}
	return family
}

func joinVersion(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, ".")
}

func inferMobile(ua, osFamily string) string {
	switch osFamily {
	case "iOS":
		// iPadOS reports as iOS but is a tablet; navigator.userAgentData.mobile is
		// false for tablets, so only phones (and iPod touch) count as mobile.
		if strings.Contains(ua, "iPad") {
			return "false"
		}
		return "true"
	case "Android":
		if strings.Contains(ua, "Mobile") {
			return "true"
		}
		return "false"
	default:
		return "false"
	}
}

func propertiesFromUAParser(client *uaparser.Client, ua string) Properties {
	props := make(Properties, 6)

	if client.UserAgent.Family != "" && client.UserAgent.Family != "Other" {
		props[PropBrowser] = normalizeBrowserFamily(client.UserAgent.Family)
		if client.UserAgent.Major != "" {
			props[PropBrowserVersion] = client.UserAgent.Major
		}
	}

	osFamily := client.Os.Family
	if osFamily != "" && osFamily != "Other" {
		props[PropOS] = normalizeOSFamily(osFamily)
		if v := joinVersion(client.Os.Major, client.Os.Minor, client.Os.Patch); v != "" {
			props[PropOSVersion] = v
		}
	}

	if client.Device.Family != "" && client.Device.Family != "Other" {
		props[PropDevice] = client.Device.Family
	}

	if len(props) == 0 {
		return nil
	}

	mobile := "false"
	if osFamily != "" && osFamily != "Other" {
		mobile = inferMobile(ua, osFamily)
	}
	props[PropMobile] = mobile

	return props
}
