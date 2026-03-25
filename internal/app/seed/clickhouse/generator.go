package seed

import (
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"
)

var eventKinds = []string{
	"page_view",
	"button_click",
	"app_open",
	"app_close",
	"purchase",
	"add_to_cart",
	"checkout_started",
	"checkout_completed",
	"notification_received",
	"notification_clicked",
	"notification_dismissed",
	"login",
	"logout",
	"signup",
	"profile_updated",
	"search",
	"video_play",
	"video_pause",
	"share",
	"error_occurred",
}

var platforms = []string{"ios", "android", "web"}
var osVersions = map[string][]string{
	"ios":     {"17.0", "17.1", "17.2", "16.6", "16.5"},
	"android": {"14", "13", "12", "11"},
	"web":     {"chrome/120", "safari/17", "firefox/121", "edge/120"},
}
var appVersions = []string{"1.0.0", "1.1.0", "1.2.0", "1.3.0", "2.0.0"}
var locales = []string{"en-US", "en-GB", "pt-BR", "de-DE", "fr-FR", "es-ES", "ja-JP"}
var deviceTypes = map[string][]string{
	"ios":     {"iPhone", "iPad"},
	"android": {"phone", "tablet"},
	"web":     {"desktop", "mobile"},
}

const distinctIDPool = 10_000

type event struct {
	eventID          string
	distinctID       string
	sessionID        string
	kind             string
	occurTime        time.Time
	autoProperties   map[string]string
	customProperties map[string]string
}

func randomEvent(start, end time.Time) event {
	kind := eventKinds[rand.IntN(len(eventKinds))]
	platform := platforms[rand.IntN(len(platforms))]
	osVers := osVersions[platform]
	devTypes := deviceTypes[platform]

	autoProps := map[string]string{
		"$platform":    platform,
		"$os_version":  osVers[rand.IntN(len(osVers))],
		"$app_version": appVersions[rand.IntN(len(appVersions))],
		"$locale":      locales[rand.IntN(len(locales))],
		"$device_type": devTypes[rand.IntN(len(devTypes))],
	}

	customProps := customPropsForKind(kind)

	delta := end.Sub(start)
	occurTime := start.Add(time.Duration(rand.Int64N(int64(delta))))

	return event{
		eventID:          uuid.New().String(),
		distinctID:       fmt.Sprintf("user-%05d", rand.IntN(distinctIDPool)),
		sessionID:        uuid.New().String(),
		kind:             kind,
		occurTime:        occurTime,
		autoProperties:   autoProps,
		customProperties: customProps,
	}
}

func customPropsForKind(kind string) map[string]string {
	switch kind {
	case "page_view":
		pages := []string{"/home", "/products", "/cart", "/checkout", "/profile", "/search", "/about"}
		return map[string]string{"page_url": pages[rand.IntN(len(pages))]}
	case "button_click":
		buttons := []string{"buy_now", "add_to_cart", "subscribe", "learn_more", "sign_up", "close"}
		return map[string]string{"button_id": buttons[rand.IntN(len(buttons))]}
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
