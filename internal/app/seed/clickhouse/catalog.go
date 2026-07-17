package seed

import (
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// Product catalog
// ---------------------------------------------------------------------------

type product struct {
	id       string
	sku      string
	name     string
	slug     string
	category string
	brand    string
	price    float64
}

var catalogSeed = []struct {
	name     string
	category string
	brand    string
	price    float64
}{
	{"Grain-Free Salmon Kibble 12lb", "food", "Wagwell", 64.99},
	{"Puppy Chicken & Rice Kibble 8lb", "food", "Wagwell", 48.99},
	{"Senior Turkey & Sweet Potato Recipe", "food", "Wagwell", 57.99},
	{"Wet Food Variety 12-Pack", "food", "Wagwell", 36.00},
	{"Freeze-Dried Raw Beef Patties", "food", "Wagwell", 42.00},
	{"Limited Ingredient Duck Kibble 10lb", "food", "Wagwell", 68.99},
	{"Small Breed Whitefish Kibble 6lb", "food", "Wagwell", 39.99},
	{"Peanut Butter Training Bites", "treats", "The Barkery", 12.00},
	{"Bully Sticks 6-Pack", "treats", "The Barkery", 24.00},
	{"Freeze-Dried Liver Treats", "treats", "The Barkery", 16.00},
	{"Dental Chews 30-Count", "treats", "The Barkery", 28.00},
	{"Sweet Potato Jerky", "treats", "The Barkery", 14.00},
	{"Calming Soft Chews", "treats", "The Barkery", 22.00},
	{"Birthday Cookie Box", "treats", "The Barkery", 18.00},
	{"Indestructible Chew Ring", "toys", "Fetch & Co", 19.00},
	{"Squeaky Mallard", "toys", "Fetch & Co", 14.00},
	{"Rope Tug Trio", "toys", "Fetch & Co", 16.00},
	{"Treat Puzzle Board", "toys", "Fetch & Co", 26.00},
	{"Fetch Launcher", "toys", "Fetch & Co", 32.00},
	{"Plush Hedgehog", "toys", "Fetch & Co", 12.00},
	{"Floating Fetch Stick", "toys", "Fetch & Co", 15.00},
	{"Emotional Support Tennis Ball", "toys", "Fetch & Co", 9.00},
	{"Squirrel Decoy (Squeaky)", "toys", "Fetch & Co", 17.00},
	{"The Forbidden Sock (Now Allowed)", "toys", "Fetch & Co", 8.00},
	{"Oatmeal & Aloe Shampoo", "grooming", "Snout & About", 18.00},
	{"Deshedding Brush", "grooming", "Snout & About", 24.00},
	{"Quiet Nail Grinder", "grooming", "Snout & About", 35.00},
	{"Paw Balm", "grooming", "Snout & About", 12.00},
	{"Ear Cleaning Wipes 100ct", "grooming", "Snout & About", 14.00},
	{"Detangling Conditioner", "grooming", "Snout & About", 16.00},
	{"No-Pull Harness", "gear", "Tailwind", 38.00},
	{"Reflective Night Leash", "gear", "Tailwind", 28.00},
	{"Personalized Collar", "gear", "Tailwind", 25.00},
	{"Travel Water Bottle", "gear", "Tailwind", 22.00},
	{"Poop Bag Dispenser with 300 Bags", "gear", "Tailwind", 15.00},
	{"Winter Dog Coat", "gear", "Tailwind", 45.00},
	{"Car Seat Hammock", "gear", "Tailwind", 49.00},
	{"GPS Tracker Tag", "gear", "Tailwind", 89.00},
	{"Orthopedic Memory-Foam Bed", "home", "Cozy Hound", 129.00},
	{"Donut Cuddler Bed", "home", "Cozy Hound", 79.00},
	{"Your Side of the Bed (Dog Bed)", "home", "Cozy Hound", 99.00},
	{"Elevated Slow-Feeder Bowls", "home", "Cozy Hound", 42.00},
	{"Washable Crate Mat", "home", "Cozy Hound", 34.00},
	{"Pet Camera Treat Tosser", "home", "Cozy Hound", 149.00},
	{"Washable Sofa Cover", "home", "Cozy Hound", 56.00},
	{"Ceramic Bowl Set", "home", "Cozy Hound", 30.00},
}

var (
	catalog        []product
	catalogWeights []int
	categories     []string
	byCategory     map[string][]product
)

func init() {
	byCategory = make(map[string][]product)
	for i, c := range catalogSeed {
		slug := strings.ToLower(c.name)
		slug = strings.NewReplacer(" ", "-", "&", "and", "--", "-").Replace(slug)
		p := product{
			id:       fmt.Sprintf("prod-%04d", i+1),
			sku:      fmt.Sprintf("PUG-%s-%03d", strings.ToUpper(c.category[:3]), i+1),
			name:     c.name,
			slug:     slug,
			category: c.category,
			brand:    c.brand,
			price:    c.price,
		}
		catalog = append(catalog, p)
		// Cheaper items sell more often.
		catalogWeights = append(catalogWeights, int(2500/c.price)+5)
		if len(byCategory[p.category]) == 0 {
			categories = append(categories, p.category)
		}
		byCategory[p.category] = append(byCategory[p.category], p)
	}
}

const storeURL = "https://pugandpals.example.com"

// ---------------------------------------------------------------------------
// Marketing vocab
// ---------------------------------------------------------------------------

var utmEntries = []struct {
	source, medium string
	referrer       string
	term, content  string
}{
	{"google", "cpc", "https://www.google.com", "dog treats", ""},
	{"google", "cpc", "https://www.google.com", "puppy harness", "ad-exact"},
	{"facebook", "paid_social", "https://facebook.com", "", "carousel-1"},
	{"instagram", "paid_social", "https://instagram.com", "", "story-2"},
	{"tiktok", "paid_social", "https://tiktok.com", "", "spark-ad"},
	{"pinterest", "social", "https://pinterest.com", "", ""},
	{"youtube", "video", "https://www.youtube.com", "", "unboxing-review"},
	{"newsletter", "email", "", "", "weekly-digest"},
}

var utmCampaigns = []string{"summer-sale", "new-arrivals", "retargeting", "brand", "back-in-stock", "pug-club-launch"}

var organicReferrers = []string{
	"", "", "", "", "", // most sessions are direct
	"https://www.google.com",
	"https://www.google.com",
	"https://www.bing.com",
	"https://duckduckgo.com",
	"https://instagram.com",
	"https://reddit.com",
	"https://news.ycombinator.com",
	"https://www.youtube.com",
	// Self-referral: attribution.Derive blanks it (same host as $url), so the
	// session classifies Direct — keeps the blanking path exercised in demo data.
	storeURL + "/",
}

// The shoppers are dogs. Most searches are sensible; some are very much not.
var searchQueries = []string{
	"dog food", "grain free kibble", "puppy food", "training treats",
	"bully sticks", "dental chews", "chew toy", "puzzle toy", "squeaky toy",
	"harness", "leash", "collar", "dog bed", "crate mat", "slow feeder",
	"dog shampoo", "nail grinder", "paw balm", "winter coat", "gps tracker",
	"puppy starter kit", "birthday treats",
	// dog-typed
	"ball", "ball ball ball", "tennis ball", "a new toy", "new owner",
	"replacement owner (mine said no)", "the good treats",
	"treats that smell like more treats", "squirrel", "cheese", "bacon",
	"mailman deterrent", "belly rub machine", "the forbidden sock",
	"why vacuum loud", "snacks for being good boy", "cat's bed (for me)",
}

var helpArticles = []struct{ id, title, category string }{
	{"shipping-times", "Shipping times & tracking", "shipping"},
	{"returns-exchanges", "Returns & exchanges (chewed items final sale)", "returns"},
	{"food-transition-guide", "Switching your dog's food safely", "feeding"},
	{"harness-sizing", "Finding the right harness size", "sizing"},
	{"pug-club-faq", "Pug Club membership FAQ", "membership"},
	{"who-is-good-boy", "Who is a good boy? (You. It's you.)", "faq"},
	{"paw-checkout-guide", "Checking out with paws: a tutorial", "accessibility"},
}

var pushCampaigns = []string{"abandoned-cart", "new-arrivals", "treat-tuesday", "back-in-stock", "price-drop", "club-renewal", "walkies-reminder", "squirrel-season-prep"}

var coupons = []struct {
	id, code string
	discount float64
}{
	{"cpn-welcome10", "WELCOME10", 10.00},
	{"cpn-goodboy15", "GOODBOY15", 15.00},
	{"cpn-club20", "CLUB20", 20.00},
	{"cpn-freeship", "FREESHIP", 7.95},
	{"cpn-zoomies5", "ZOOMIES5", 5.00},
}

// Pug Club membership tiers. Every dog qualifies for Goodest Boy; upgrading
// is a formality.
type clubPlan struct {
	id     string
	name   string
	amount float64
	weight int
}

var clubPlans = []clubPlan{
	{"good-boy-monthly", "Good Boy", 9.99, 6},
	{"goodest-boy-annual", "Goodest Boy", 89.00, 3},
	{"very-long-boy-family", "Very Long Boy", 14.99, 1}, // multi-dog households
}

var clickTexts = []string{"Add to cart", "Buy now", "Apply", "Sizing guide", "View all", "Sign up", "Checkout", "Bring me this", ""}
var cssTags = []string{"BUTTON", "A", "DIV", "SPAN", "INPUT", "LABEL", "LI"}
