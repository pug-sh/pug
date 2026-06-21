package seed

// ---------------------------------------------------------------------------
// Device profiles
// ---------------------------------------------------------------------------

type deviceProfile struct {
	platform        string // $platform: web | ios | android
	os              string
	osVersions      []string
	browser         string // empty = no $browser (native app traffic)
	browserVersions []string
	device          string // empty = no $device (real UA parses yield none for Windows/Linux desktops)
	mobile          bool
	screens         [][2]int
	weight          int
}

var desktopScreens = [][2]int{{1920, 1080}, {1440, 900}, {1366, 768}, {2560, 1440}, {1536, 864}}
var iphoneScreens = [][2]int{{390, 844}, {393, 852}, {430, 932}, {375, 812}}
var androidScreens = [][2]int{{360, 800}, {412, 915}, {384, 854}}
var ipadScreens = [][2]int{{820, 1180}, {1024, 1366}}

var deviceProfiles = []deviceProfile{
	// Desktop web.
	{platform: "web", os: "Windows", osVersions: []string{"10", "11"}, browser: "Chrome", browserVersions: []string{"124", "125", "126"}, screens: desktopScreens, weight: 22},
	{platform: "web", os: "Windows", osVersions: []string{"10", "11"}, browser: "Edge", browserVersions: []string{"124", "125"}, screens: desktopScreens, weight: 6},
	{platform: "web", os: "Windows", osVersions: []string{"10", "11"}, browser: "Firefox", browserVersions: []string{"126", "127"}, screens: desktopScreens, weight: 4},
	{platform: "web", os: "Mac OS X", osVersions: []string{"13.6", "14.4", "14.5"}, browser: "Safari", browserVersions: []string{"17.3", "17.4"}, device: "Mac", screens: desktopScreens, weight: 8},
	{platform: "web", os: "Mac OS X", osVersions: []string{"13.6", "14.4", "14.5"}, browser: "Chrome", browserVersions: []string{"124", "125", "126"}, device: "Mac", screens: desktopScreens, weight: 7},
	{platform: "web", os: "Linux", osVersions: []string{""}, browser: "Chrome", browserVersions: []string{"124", "125"}, screens: desktopScreens, weight: 2},
	{platform: "web", os: "Linux", osVersions: []string{""}, browser: "Firefox", browserVersions: []string{"126", "127"}, screens: desktopScreens, weight: 2},
	{platform: "web", os: "Chrome OS", osVersions: []string{"124"}, browser: "Chrome", browserVersions: []string{"124", "125"}, screens: desktopScreens, weight: 1},
	// Mobile web.
	{platform: "web", os: "iOS", osVersions: []string{"16.7", "17.4", "17.5"}, browser: "Mobile Safari", browserVersions: []string{"16.7", "17.4"}, device: "iPhone", mobile: true, screens: iphoneScreens, weight: 12},
	{platform: "web", os: "Android", osVersions: []string{"13", "14"}, browser: "Chrome Mobile", browserVersions: []string{"124", "125"}, device: "Pixel 8", mobile: true, screens: androidScreens, weight: 5},
	{platform: "web", os: "Android", osVersions: []string{"13", "14"}, browser: "Chrome Mobile", browserVersions: []string{"124", "125"}, device: "Samsung Galaxy S24", mobile: true, screens: androidScreens, weight: 5},
	// Native apps.
	{platform: "ios", os: "iOS", osVersions: []string{"16.7", "17.4", "17.5"}, device: "iPhone 15 Pro", mobile: true, screens: iphoneScreens, weight: 6},
	{platform: "ios", os: "iOS", osVersions: []string{"16.7", "17.4"}, device: "iPhone 14", mobile: true, screens: iphoneScreens, weight: 5},
	{platform: "ios", os: "iOS", osVersions: []string{"15.8", "16.7"}, device: "iPhone 13", mobile: true, screens: iphoneScreens, weight: 3},
	{platform: "ios", os: "iOS", osVersions: []string{"17.4", "17.5"}, device: "iPad Air", mobile: true, screens: ipadScreens, weight: 2},
	{platform: "android", os: "Android", osVersions: []string{"14"}, device: "Pixel 8", mobile: true, screens: androidScreens, weight: 4},
	{platform: "android", os: "Android", osVersions: []string{"13", "14"}, device: "Samsung Galaxy S24", mobile: true, screens: androidScreens, weight: 4},
	{platform: "android", os: "Android", osVersions: []string{"12", "13"}, device: "Samsung Galaxy A54", mobile: true, screens: androidScreens, weight: 3},
	{platform: "android", os: "Android", osVersions: []string{"13", "14"}, device: "OnePlus 12", mobile: true, screens: androidScreens, weight: 2},
}

var sdkVersions = map[string]string{"web": "0.3.2", "ios": "0.2.1", "android": "0.2.0"}

// App versions live in episodes.go (appReleases): they follow a release
// schedule with adoption curves rather than a static list.

// ---------------------------------------------------------------------------
// Bots
// ---------------------------------------------------------------------------

var botProfiles = []struct {
	browser  string
	verified bool
}{
	{"Googlebot", true},
	{"Bingbot", true},
	{"AhrefsBot", false},
	{"SemrushBot", false},
	{"GPTBot", true},
	{"", false}, // headless scraper, no recognizable UA
}
