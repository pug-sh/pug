package seed

// ---------------------------------------------------------------------------
// Geography
// ---------------------------------------------------------------------------

type geoEntry struct {
	continent  string
	country    string
	region     string
	city       string
	postalCode string
	timezone   string
	locale     string
	latitude   float64
	longitude  float64
	weight     int
}

// geoPool uses the same codes Cloudflare sends in CF-IPContinent /
// CF-IPCountry headers (two-letter continent codes, ISO 3166-1 alpha-2).
var geoPool = []geoEntry{
	// North America
	{"NA", "US", "California", "San Francisco", "94105", "America/Los_Angeles", "en-US", 37.7749, -122.4194, 5},
	{"NA", "US", "California", "Los Angeles", "90001", "America/Los_Angeles", "en-US", 34.0522, -118.2437, 5},
	{"NA", "US", "New York", "New York City", "10001", "America/New_York", "en-US", 40.7128, -74.0060, 7},
	{"NA", "US", "Texas", "Austin", "78701", "America/Chicago", "en-US", 30.2672, -97.7431, 4},
	{"NA", "US", "Texas", "Houston", "77002", "America/Chicago", "en-US", 29.7604, -95.3698, 3},
	{"NA", "US", "Washington", "Seattle", "98101", "America/Los_Angeles", "en-US", 47.6062, -122.3321, 4},
	{"NA", "US", "Illinois", "Chicago", "60601", "America/Chicago", "en-US", 41.8781, -87.6298, 4},
	{"NA", "US", "Florida", "Miami", "33101", "America/New_York", "en-US", 25.7617, -80.1918, 3},
	{"NA", "US", "Massachusetts", "Boston", "02108", "America/New_York", "en-US", 42.3601, -71.0589, 3},
	{"NA", "US", "Colorado", "Denver", "80202", "America/Denver", "en-US", 39.7392, -104.9903, 3},
	{"NA", "US", "Georgia", "Atlanta", "30303", "America/New_York", "en-US", 33.7490, -84.3880, 3},
	{"NA", "CA", "Ontario", "Toronto", "M5H", "America/Toronto", "en-CA", 43.6510, -79.3470, 4},
	{"NA", "CA", "British Columbia", "Vancouver", "V6B", "America/Vancouver", "en-CA", 49.2827, -123.1207, 2},
	{"NA", "MX", "Mexico City", "Mexico City", "06000", "America/Mexico_City", "es-MX", 19.4326, -99.1332, 3},
	// Europe
	{"EU", "GB", "England", "London", "EC1A", "Europe/London", "en-GB", 51.5074, -0.1278, 6},
	{"EU", "GB", "England", "Manchester", "M1", "Europe/London", "en-GB", 53.4808, -2.2426, 2},
	{"EU", "DE", "Bavaria", "Munich", "80331", "Europe/Berlin", "de-DE", 48.1351, 11.5820, 2},
	{"EU", "DE", "Berlin", "Berlin", "10115", "Europe/Berlin", "de-DE", 52.5200, 13.4050, 3},
	{"EU", "FR", "Île-de-France", "Paris", "75001", "Europe/Paris", "fr-FR", 48.8566, 2.3522, 4},
	{"EU", "NL", "North Holland", "Amsterdam", "1012", "Europe/Amsterdam", "nl-NL", 52.3676, 4.9041, 2},
	{"EU", "ES", "Madrid", "Madrid", "28001", "Europe/Madrid", "es-ES", 40.4168, -3.7038, 3},
	{"EU", "ES", "Catalonia", "Barcelona", "08001", "Europe/Madrid", "es-ES", 41.3874, 2.1686, 2},
	{"EU", "IT", "Lombardy", "Milan", "20121", "Europe/Rome", "it-IT", 45.4642, 9.1900, 2},
	{"EU", "SE", "Stockholm", "Stockholm", "111 29", "Europe/Stockholm", "sv-SE", 59.3293, 18.0686, 2},
	{"EU", "PL", "Masovia", "Warsaw", "00-001", "Europe/Warsaw", "pl-PL", 52.2297, 21.0122, 2},
	{"EU", "IE", "Leinster", "Dublin", "D01", "Europe/Dublin", "en-IE", 53.3498, -6.2603, 2},
	{"EU", "CH", "Zurich", "Zurich", "8001", "Europe/Zurich", "de-CH", 47.3769, 8.5417, 1},
	{"EU", "DK", "Capital Region", "Copenhagen", "1050", "Europe/Copenhagen", "da-DK", 55.6761, 12.5683, 1},
	{"EU", "PT", "Lisbon", "Lisbon", "1100", "Europe/Lisbon", "pt-PT", 38.7223, -9.1393, 2},
	{"EU", "CZ", "Prague", "Prague", "110 00", "Europe/Prague", "cs-CZ", 50.0755, 14.4378, 1},
	// South America
	{"SA", "BR", "São Paulo", "São Paulo", "01310", "America/Sao_Paulo", "pt-BR", -23.5505, -46.6333, 4},
	{"SA", "BR", "Rio de Janeiro", "Rio de Janeiro", "20040", "America/Sao_Paulo", "pt-BR", -22.9068, -43.1729, 2},
	{"SA", "AR", "Buenos Aires", "Buenos Aires", "C1002", "America/Argentina/Buenos_Aires", "es-AR", -34.6037, -58.3816, 2},
	{"SA", "CO", "Bogotá", "Bogotá", "110111", "America/Bogota", "es-CO", 4.7110, -74.0721, 2},
	{"SA", "CL", "Santiago", "Santiago", "8320000", "America/Santiago", "es-CL", -33.4489, -70.6693, 1},
	// Asia
	{"AS", "IN", "Maharashtra", "Mumbai", "400001", "Asia/Kolkata", "en-IN", 19.0760, 72.8777, 4},
	{"AS", "IN", "Karnataka", "Bengaluru", "560001", "Asia/Kolkata", "en-IN", 12.9716, 77.5946, 4},
	{"AS", "IN", "Delhi", "New Delhi", "110001", "Asia/Kolkata", "en-IN", 28.6139, 77.2090, 3},
	{"AS", "JP", "Tokyo", "Tokyo", "100-0001", "Asia/Tokyo", "ja-JP", 35.6762, 139.6503, 4},
	{"AS", "JP", "Osaka", "Osaka", "530-0001", "Asia/Tokyo", "ja-JP", 34.6937, 135.5023, 2},
	{"AS", "SG", "Central", "Singapore", "018956", "Asia/Singapore", "en-SG", 1.3521, 103.8198, 3},
	{"AS", "KR", "Seoul", "Seoul", "04524", "Asia/Seoul", "ko-KR", 37.5665, 126.9780, 3},
	{"AS", "AE", "Dubai", "Dubai", "00000", "Asia/Dubai", "ar-AE", 25.2048, 55.2708, 2},
	{"AS", "ID", "Jakarta", "Jakarta", "10110", "Asia/Jakarta", "id-ID", -6.2088, 106.8456, 2},
	{"AS", "PH", "Metro Manila", "Manila", "1000", "Asia/Manila", "en-PH", 14.5995, 120.9842, 2},
	{"AS", "TH", "Bangkok", "Bangkok", "10100", "Asia/Bangkok", "th-TH", 13.7563, 100.5018, 2},
	{"AS", "VN", "Ho Chi Minh City", "Ho Chi Minh City", "700000", "Asia/Ho_Chi_Minh", "vi-VN", 10.8231, 106.6297, 1},
	{"AS", "HK", "Hong Kong", "Hong Kong", "999077", "Asia/Hong_Kong", "zh-HK", 22.3193, 114.1694, 2},
	{"AS", "TR", "Istanbul", "Istanbul", "34000", "Europe/Istanbul", "tr-TR", 41.0082, 28.9784, 2},
	{"AS", "IL", "Tel Aviv", "Tel Aviv", "61000", "Asia/Jerusalem", "he-IL", 32.0853, 34.7818, 1},
	// Oceania & Africa
	{"OC", "AU", "New South Wales", "Sydney", "2000", "Australia/Sydney", "en-AU", -33.8688, 151.2093, 3},
	{"OC", "AU", "Victoria", "Melbourne", "3000", "Australia/Melbourne", "en-AU", -37.8136, 144.9631, 2},
	{"OC", "NZ", "Auckland", "Auckland", "1010", "Pacific/Auckland", "en-NZ", -36.8509, 174.7645, 1},
	{"AF", "ZA", "Gauteng", "Johannesburg", "2000", "Africa/Johannesburg", "en-ZA", -26.2041, 28.0473, 2},
	{"AF", "NG", "Lagos", "Lagos", "100001", "Africa/Lagos", "en-NG", 6.5244, 3.3792, 2},
	{"AF", "EG", "Cairo", "Cairo", "11511", "Africa/Cairo", "ar-EG", 30.0444, 31.2357, 1},
}
