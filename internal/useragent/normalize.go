package useragent

import (
	"regexp"
	"strconv"
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

// OS versions a browser sends whatever the visitor runs, so they name no release.
var frozenOSVersions = map[string]map[string]bool{
	// Firefox caps at 10.15, Safari and Chromium send 10_15_7, Big Sur's compat mode sent 10.16.
	"Mac OS X": {"10.15": true, "10.15.7": true, "10.16": true},
	// iOS 26's WebKit has sent 18_6, 18_6_2 and 18_7, a WKWebView's included.
	"iOS": {"18.6": true, "18.6.2": true, "18.7": true},
	// NT 10.0 is Windows 10 and 11 alike.
	"Windows":   {"10": true},
	"Chrome OS": {"14541.0.0": true},
}

// Safari 26 froze the iOS token at 18.x, but on iOS, Safari's own version still names the release.
var iosSafariVersion = regexp.MustCompile(`\sVersion/((\d+)(?:\.\d+)*).*\sSafari/`)

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

// Chromium's reduced UA calls every Android release 10 and every device K.
func isReducedAndroid(ua string) bool {
	return strings.Contains(ua, "; Android 10; K)") || strings.Contains(ua, "; Android 10; K; wv)")
}

func osVersion(os *uaparser.Os, ua string) string {
	switch os.Family {
	case "iOS":
		if m := iosSafariVersion.FindStringSubmatch(ua); m != nil {
			if major, _ := strconv.Atoi(m[2]); major >= 26 {
				return m[1]
			}
			// Safari before 26 runs on iOS before 26, where 18.6 and 18.7 are real releases.
			return joinVersion(os.Major, os.Minor, os.Patch)
		}
	case "Android":
		if isReducedAndroid(ua) {
			return ""
		}
	}
	v := joinVersion(os.Major, os.Minor, os.Patch)
	if frozenOSVersions[os.Family][v] {
		return ""
	}
	return v
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
		// A WKWebView has no version of its own: uap-go reads one from any Version/ token, an app's AppVersion/ included.
		if client.UserAgent.Major != "" && client.UserAgent.Family != "Mobile Safari UI/WKWebView" {
			props[PropBrowserVersion] = client.UserAgent.Major
		}
	}

	osFamily := client.Os.Family
	if osFamily != "" && osFamily != "Other" {
		props[PropOS] = normalizeOSFamily(osFamily)
		if v := osVersion(client.Os, ua); v != "" {
			props[PropOSVersion] = v
		}
	}

	if client.Device.Family != "" && client.Device.Family != "Other" && !isReducedAndroid(ua) {
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
