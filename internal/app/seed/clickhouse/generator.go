package seed

import (
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"
)

var osNames = map[string]string{
	"ios":     "iOS",
	"android": "Android",
}

var webOSNames = []string{"Mac OS X", "Windows", "Linux", "Chrome OS"}

var osVersions = map[string][]string{
	"ios":     {"17", "16", "15"},
	"android": {"14", "13", "12", "11"},
	"web":     {"10", "11", "12", "13", "14"},
}

var browsers = []string{"Chrome", "Safari", "Firefox", "Edge"}
var browserVersions = []string{"120", "121", "122", "17", "123"}

var mobileDevices = map[string][]string{
	"ios":     {"iPhone", "iPad"},
	"android": {"Pixel 8", "Samsung Galaxy S24", "OnePlus 12"},
}

var appVersions = []string{"1.0.0", "1.1.0", "1.2.0", "1.3.0", "2.0.0"}
var locales = []string{"en-US", "en-GB", "pt-BR", "de-DE", "fr-FR", "es-ES", "ja-JP"}

type geoEntry struct {
	continent  string
	country    string
	region     string
	city       string
	postalCode string
	timezone   string
	latitude   string
	longitude  string
}

// geoPool is a curated set of realistic locations with weighted distribution
// (higher-traffic countries appear more times).
var geoPool = []geoEntry{
	// United States (highest weight)
	{"Americas", "United States", "California", "San Francisco", "94105", "America/Los_Angeles", "37.7749", "-122.4194"},
	{"Americas", "United States", "California", "Los Angeles", "90001", "America/Los_Angeles", "34.0522", "-118.2437"},
	{"Americas", "United States", "New York", "New York City", "10001", "America/New_York", "40.7128", "-74.0060"},
	{"Americas", "United States", "Texas", "Austin", "78701", "America/Chicago", "30.2672", "-97.7431"},
	{"Americas", "United States", "Washington", "Seattle", "98101", "America/Los_Angeles", "47.6062", "-122.3321"},
	{"Americas", "United States", "Illinois", "Chicago", "60601", "America/Chicago", "41.8781", "-87.6298"},
	// United Kingdom
	{"Europe", "United Kingdom", "England", "London", "EC1A", "Europe/London", "51.5074", "-0.1278"},
	{"Europe", "United Kingdom", "England", "Manchester", "M1", "Europe/London", "53.4808", "-2.2426"},
	// Germany
	{"Europe", "Germany", "Bavaria", "Munich", "80331", "Europe/Berlin", "48.1351", "11.5820"},
	{"Europe", "Germany", "Berlin", "Berlin", "10115", "Europe/Berlin", "52.5200", "13.4050"},
	// France
	{"Europe", "France", "Île-de-France", "Paris", "75001", "Europe/Paris", "48.8566", "2.3522"},
	// Brazil
	{"Americas", "Brazil", "São Paulo", "São Paulo", "01310", "America/Sao_Paulo", "-23.5505", "-46.6333"},
	{"Americas", "Brazil", "Rio de Janeiro", "Rio de Janeiro", "20040", "America/Sao_Paulo", "-22.9068", "-43.1729"},
	// India
	{"Asia", "India", "Maharashtra", "Mumbai", "400001", "Asia/Kolkata", "19.0760", "72.8777"},
	{"Asia", "India", "Karnataka", "Bengaluru", "560001", "Asia/Kolkata", "12.9716", "77.5946"},
	// Japan
	{"Asia", "Japan", "Tokyo", "Tokyo", "100-0001", "Asia/Tokyo", "35.6762", "139.6503"},
	// Australia
	{"Oceania", "Australia", "New South Wales", "Sydney", "2000", "Australia/Sydney", "-33.8688", "151.2093"},
	// Canada
	{"Americas", "Canada", "Ontario", "Toronto", "M5H", "America/Toronto", "43.6510", "-79.3470"},
	// Netherlands
	{"Europe", "Netherlands", "North Holland", "Amsterdam", "1012", "Europe/Amsterdam", "52.3676", "4.9041"},
	// Singapore
	{"Asia", "Singapore", "Central", "Singapore", "018956", "Asia/Singapore", "1.3521", "103.8198"},
}

var pages = []string{"/", "/home", "/products", "/cart", "/checkout", "/profile", "/search", "/about", "/pricing", "/blog"}
var pageTitles = map[string]string{
	"/":         "Home",
	"/home":     "Home",
	"/products": "Products",
	"/cart":     "Your Cart",
	"/checkout": "Checkout",
	"/profile":  "Your Profile",
	"/search":   "Search",
	"/about":    "About Us",
	"/pricing":  "Pricing",
	"/blog":     "Blog",
}
var referrers = []string{
	"", "", "", "", // empty strings reduce referrer frequency
	"https://google.com",
	"https://www.google.com",
	"https://twitter.com",
	"https://t.co",
	"https://github.com",
	"https://linkedin.com",
	"https://reddit.com",
}
var utmSources = []string{"google", "twitter", "newsletter", "github", "linkedin"}
var utmMediums = []string{"cpc", "social", "email", "organic", "referral"}
var utmCampaigns = []string{"launch", "q1-promo", "retargeting", "brand", "product-hunt"}
var screenSizes = [][2]string{
	{"1920", "1080"},
	{"1440", "900"},
	{"1366", "768"},
	{"2560", "1440"},
	{"375", "812"},
	{"390", "844"},
	{"414", "896"},
	{"360", "800"},
}
var sdkVersion = "0.1.0"
var cssTags = []string{"BUTTON", "A", "DIV", "SPAN", "INPUT", "LABEL", "LI"}

// journey defines an ordered sequence of event kinds for a session.
// Steps are executed in order; timestamps are spread across the session window.
type journey struct {
	steps []string
}

// Web journeys model realistic user behaviour on a web app.
var webJourneys = []journey{
	// browse
	{[]string{"page_view", "scroll", "click", "page_view", "scroll"}},
	{[]string{"page_view", "scroll", "page_view", "click", "scroll", "page_view"}},
	// checkout funnel
	{[]string{"page_view", "click", "add_to_cart", "checkout_started", "checkout_completed"}},
	{[]string{"page_view", "search", "click", "add_to_cart", "checkout_started", "checkout_completed"}},
	// partial checkout (abandoned)
	{[]string{"page_view", "click", "add_to_cart", "checkout_started"}},
	// signup / auth
	{[]string{"page_view", "scroll", "form_start", "form_submit", "signup"}},
	{[]string{"page_view", "form_start", "form_submit", "login"}},
	// search & browse
	{[]string{"page_view", "search", "click", "page_view", "scroll"}},
	// video engagement
	{[]string{"page_view", "scroll", "video_play", "video_pause"}},
	// notification-driven
	{[]string{"notification_received", "notification_clicked", "page_view", "click"}},
	// frustration
	{[]string{"page_view", "click", "dead_click", "rage_click"}},
}

// Mobile journeys model realistic in-app behaviour.
var mobileJourneys = []journey{
	// standard browse
	{[]string{"app_open", "page_view", "scroll", "page_view", "scroll", "app_close"}},
	{[]string{"app_open", "page_view", "click", "page_view", "app_close"}},
	// purchase funnel
	{[]string{"app_open", "search", "add_to_cart", "checkout_started", "checkout_completed", "app_close"}},
	// abandoned purchase
	{[]string{"app_open", "search", "add_to_cart", "checkout_started", "app_close"}},
	// notification-driven open
	{[]string{"notification_received", "notification_clicked", "app_open", "page_view", "click", "app_close"}},
	// auth
	{[]string{"app_open", "login", "page_view", "scroll", "logout", "app_close"}},
	// error
	{[]string{"app_open", "page_view", "error_occurred", "app_close"}},
	// video
	{[]string{"app_open", "page_view", "video_play", "video_pause", "app_close"}},
	// share
	{[]string{"app_open", "page_view", "scroll", "share", "app_close"}},
}

const (
	distinctIDPool  = 10_000
	sessionPoolSize = 50_000

	// sessions last between 1 and 30 minutes
	minSessionMs = 60_000
	maxSessionMs = 30 * 60_000
)

type event struct {
	eventID          string
	distinctID       string
	sessionID        string
	kind             string
	occurTime        time.Time
	autoProperties   map[string]string
	customProperties map[string]string
}

// buildSessionPool pre-generates all sessions for the pool.
// Each session gets a journey (ordered event sequence); events are spread
// across the session window with jitter. Event IDs and session IDs are left
// empty — they are assigned fresh on each use via randomSessionFromPool.
func buildSessionPool(start, end time.Time) [][]event {
	platforms := []string{"ios", "android", "web"}
	totalMs := end.Sub(start).Milliseconds()

	pool := make([][]event, 0, sessionPoolSize)

	for range sessionPoolSize {
		platform := platforms[rand.IntN(len(platforms))]
		sessionStart := start.Add(time.Duration(rand.Int64N(totalMs)) * time.Millisecond)
		durationMs := minSessionMs + rand.Int64N(maxSessionMs-minSessionMs)
		sessionEnd := sessionStart.Add(time.Duration(durationMs) * time.Millisecond)
		if sessionEnd.After(end) {
			sessionEnd = end
		}

		distinctID := fmt.Sprintf("user-%05d", rand.IntN(distinctIDPool))
		stableProps := buildSessionProps(platform)

		j := pickJourney(platform)
		n := len(j.steps)
		if n == 0 {
			continue
		}

		// spread events evenly across the session window
		windowMs := sessionEnd.Sub(sessionStart).Milliseconds()
		stepMs := windowMs / int64(n)

		sess := make([]event, 0, n)
		for i, kind := range j.steps {
			occurTime := sessionStart.Add(time.Duration(int64(i)*stepMs+rand.Int64N(max(stepMs, 1))) * time.Millisecond)
			if occurTime.After(sessionEnd) {
				occurTime = sessionEnd
			}

			autoProps := copyProps(stableProps)
			if platform == "web" {
				addPerEventWebProps(autoProps, i == 0)
			}

			sess = append(sess, event{
				distinctID:       distinctID,
				kind:             kind,
				occurTime:        occurTime,
				autoProperties:   autoProps,
				customProperties: customPropsForKind(kind),
			})
		}
		pool = append(pool, sess)
	}

	return pool
}

// sessionWindow tracks a single session's time range and platform for overlap detection.
type sessionWindow struct {
	start    time.Time
	end      time.Time
	platform string
}

// userSessionTracker records assigned session windows per user so that overlapping
// sessions can be detected and assigned a different platform.
type userSessionTracker struct {
	sessions map[string][]sessionWindow
}

func newUserSessionTracker() *userSessionTracker {
	return &userSessionTracker{sessions: make(map[string][]sessionWindow)}
}

// overlappingPlatforms returns the set of platforms already occupying [start, end]
// for the given user.
func (t *userSessionTracker) overlappingPlatforms(distinctID string, start, end time.Time) map[string]bool {
	out := map[string]bool{}
	for _, w := range t.sessions[distinctID] {
		if start.Before(w.end) && end.After(w.start) {
			out[w.platform] = true
		}
	}
	return out
}

func (t *userSessionTracker) register(distinctID, platform string, start, end time.Time) {
	t.sessions[distinctID] = append(t.sessions[distinctID], sessionWindow{start, end, platform})
}

// buildSession constructs a fresh session for the given user, platform, and time window.
// Used when an overlapping session needs to be rebuilt on a different platform.
func buildSession(distinctID, platform string, sessionStart, sessionEnd time.Time) []event {
	stableProps := buildSessionProps(platform)
	j := pickJourney(platform)
	n := len(j.steps)
	if n == 0 {
		return nil
	}
	windowMs := sessionEnd.Sub(sessionStart).Milliseconds()
	stepMs := windowMs / int64(n)
	sess := make([]event, 0, n)
	for i, kind := range j.steps {
		occurTime := sessionStart.Add(time.Duration(int64(i)*stepMs+rand.Int64N(max(stepMs, 1))) * time.Millisecond)
		if occurTime.After(sessionEnd) {
			occurTime = sessionEnd
		}
		autoProps := copyProps(stableProps)
		if platform == "web" {
			addPerEventWebProps(autoProps, i == 0)
		}
		sess = append(sess, event{
			distinctID:       distinctID,
			kind:             kind,
			occurTime:        occurTime,
			autoProperties:   autoProps,
			customProperties: customPropsForKind(kind),
		})
	}
	return sess
}

// randomSessionFromPool picks a random session from the pool and returns it
// with a fresh session_id, fresh event_ids, and a new random start time within
// [start, end]. Re-anchoring prevents clustering when pool sessions are reused
// across many insertions (same pool entry → same occur_times → same user gets
// N identical notification_received at T, then N notification_clicked at T+step).
// If the re-anchored session would overlap an existing session for the same user
// on the same platform, the session is rebuilt on a different platform so
// concurrent sessions always represent different devices.
func randomSessionFromPool(pool [][]event, start, end time.Time, tracker *userSessionTracker) []event {
	src := pool[rand.IntN(len(pool))]

	firstTime := src[0].occurTime
	lastTime := src[len(src)-1].occurTime
	sessionDuration := lastTime.Sub(firstTime)
	available := end.Sub(start) - sessionDuration

	var newStart time.Time
	if available > 0 {
		newStart = start.Add(time.Duration(rand.Int64N(available.Milliseconds())) * time.Millisecond)
	} else {
		newStart = start
	}
	newEnd := newStart.Add(sessionDuration)
	offset := newStart.Sub(firstTime)

	distinctID := src[0].distinctID
	platform := src[0].autoProperties["$platform"]

	// If this window overlaps an existing session for the same user on the same platform,
	// rebuild on a different platform (a user can use multiple devices simultaneously,
	// but not the same device twice).
	if occupied := tracker.overlappingPlatforms(distinctID, newStart, newEnd); occupied[platform] {
		var alt []string
		for _, p := range []string{"ios", "android", "web"} {
			if !occupied[p] {
				alt = append(alt, p)
			}
		}
		if len(alt) > 0 {
			platform = alt[rand.IntN(len(alt))]
			sess := buildSession(distinctID, platform, newStart, newEnd)
			sessionID := uuid.New().String()
			for i := range sess {
				sess[i].eventID = uuid.New().String()
				sess[i].sessionID = sessionID
			}
			tracker.register(distinctID, platform, newStart, newEnd)
			return sess
		}
	}

	tracker.register(distinctID, platform, newStart, newEnd)

	sessionID := uuid.New().String()
	out := make([]event, len(src))
	for i, e := range src {
		e.eventID = uuid.New().String()
		e.sessionID = sessionID
		e.occurTime = e.occurTime.Add(offset)
		e.autoProperties = copyProps(e.autoProperties)
		e.customProperties = customPropsForKind(e.kind)
		out[i] = e
	}
	return out
}

func pickJourney(platform string) journey {
	if platform == "web" {
		return webJourneys[rand.IntN(len(webJourneys))]
	}
	return mobileJourneys[rand.IntN(len(mobileJourneys))]
}

// buildSessionProps returns auto-props that stay consistent for all events in a session.
func buildSessionProps(platform string) map[string]string {
	props := map[string]string{
		"$platform":    platform,
		"$app_version": appVersions[rand.IntN(len(appVersions))],
		"$locale":      locales[rand.IntN(len(locales))],
	}

	if platform == "web" {
		osName := webOSNames[rand.IntN(len(webOSNames))]
		props["$os"] = osName
		props["$osVersion"] = osVersions["web"][rand.IntN(len(osVersions["web"]))]
		props["$browser"] = browsers[rand.IntN(len(browsers))]
		props["$browserVersion"] = browserVersions[rand.IntN(len(browserVersions))]
		props["$sdkVersion"] = sdkVersion

		screen := screenSizes[rand.IntN(len(screenSizes))]
		props["$screenWidth"] = screen[0]
		props["$screenHeight"] = screen[1]
		isMobile := screen[0] == "375" || screen[0] == "390" || screen[0] == "414" || screen[0] == "360"
		if isMobile {
			props["$mobile"] = "true"
		} else {
			props["$mobile"] = "false"
		}

		// UTM params — only ~15% of sessions are acquisition sessions
		if rand.Float32() < 0.15 {
			props["$utmSource"] = utmSources[rand.IntN(len(utmSources))]
			props["$utmMedium"] = utmMediums[rand.IntN(len(utmMediums))]
			props["$utmCampaign"] = utmCampaigns[rand.IntN(len(utmCampaigns))]
		}
	} else {
		props["$os"] = osNames[platform]
		props["$osVersion"] = osVersions[platform][rand.IntN(len(osVersions[platform]))]
		devList := mobileDevices[platform]
		props["$device"] = devList[rand.IntN(len(devList))]
	}

	geo := geoPool[rand.IntN(len(geoPool))]
	props["$continent"] = geo.continent
	props["$country"] = geo.country
	props["$region"] = geo.region
	props["$city"] = geo.city
	props["$postalCode"] = geo.postalCode
	props["$timezone"] = geo.timezone
	props["$latitude"] = geo.latitude
	props["$longitude"] = geo.longitude

	return props
}

// addPerEventWebProps adds URL/referrer which vary per page navigation within a session.
func addPerEventWebProps(props map[string]string, isFirst bool) {
	page := pages[rand.IntN(len(pages))]
	props["$url"] = "https://example.com" + page
	props["$pageTitle"] = pageTitles[page]
	if isFirst {
		if r := referrers[rand.IntN(len(referrers))]; r != "" {
			props["$referrer"] = r
		}
	}
}

func copyProps(src map[string]string) map[string]string {
	dst := make(map[string]string, len(src)+4)
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func customPropsForKind(kind string) map[string]string {
	switch kind {
	case "page_view":
		return map[string]string{}
	case "click":
		return map[string]string{
			"class": fmt.Sprintf("btn-%s", []string{"primary", "secondary", "link", "icon"}[rand.IntN(4)]),
			"id":    fmt.Sprintf("el-%04d", rand.IntN(200)),
			"tag":   cssTags[rand.IntN(len(cssTags))],
			"text":  []string{"Buy now", "Add to cart", "Learn more", "Sign up", "Close", "Submit", ""}[rand.IntN(7)],
			"x":     fmt.Sprintf("%d", rand.IntN(1920)),
			"y":     fmt.Sprintf("%d", rand.IntN(1080)),
		}
	case "rage_click":
		return map[string]string{
			"click_count": fmt.Sprintf("%d", 3+rand.IntN(5)),
			"element":     cssTags[rand.IntN(len(cssTags))],
			"x":           fmt.Sprintf("%d", rand.IntN(1920)),
			"y":           fmt.Sprintf("%d", rand.IntN(1080)),
		}
	case "dead_click":
		return map[string]string{
			"element": cssTags[rand.IntN(len(cssTags))],
			"text":    []string{"Buy now", "Submit", "Continue", "Next", "Confirm", ""}[rand.IntN(6)],
			"x":       fmt.Sprintf("%d", rand.IntN(1920)),
			"y":       fmt.Sprintf("%d", rand.IntN(1080)),
		}
	case "scroll":
		percent := rand.IntN(101)
		return map[string]string{
			"percent":  fmt.Sprintf("%d", percent),
			"scroll_y": fmt.Sprintf("%d", percent*50),
		}
	case "form_start":
		forms := []string{"login", "signup", "checkout", "newsletter", "contact"}
		f := forms[rand.IntN(len(forms))]
		return map[string]string{"form_id": f + "-form", "form_name": f}
	case "form_submit":
		forms := []string{"login", "signup", "checkout", "newsletter", "contact"}
		f := forms[rand.IntN(len(forms))]
		return map[string]string{"action": "/api/" + f, "form_id": f + "-form", "form_name": f}
	case "purchase", "add_to_cart", "checkout_started", "checkout_completed":
		return map[string]string{
			"product_id": fmt.Sprintf("prod-%04d", rand.IntN(500)),
			"amount":     fmt.Sprintf("%.2f", rand.Float64()*500),
			"currency":   "USD",
		}
	case "notification_received", "notification_clicked", "notification_dismissed":
		return map[string]string{
			"campaign_id":       fmt.Sprintf("camp-%04d", rand.IntN(100)),
			"notification_type": []string{"push", "in-app", "email"}[rand.IntN(3)],
		}
	case "search":
		terms := []string{"shoes", "laptop", "coffee", "book", "headphones", "shirt", "camera"}
		return map[string]string{"query": terms[rand.IntN(len(terms))]}
	case "video_play", "video_pause":
		return map[string]string{
			"video_id":   fmt.Sprintf("vid-%04d", rand.IntN(200)),
			"position_s": fmt.Sprintf("%d", rand.IntN(3600)),
		}
	case "error_occurred":
		codes := []string{"500", "404", "403", "timeout", "network"}
		return map[string]string{"error_code": codes[rand.IntN(len(codes))]}
	default:
		return map[string]string{}
	}
}
