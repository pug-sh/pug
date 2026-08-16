package geo

import "strings"

// countryCodes is the ISO 3166-1 alpha-2 set: the 249 officially assigned codes
// plus XK, the user-assigned code Cloudflare returns for Kosovo. Hardcoded
// rather than embedded — the list changes every few years, and a build-time
// literal keeps the lookup allocation-free.
const countryCodes = "" +
	"AD AE AF AG AI AL AM AO AQ AR AS AT AU AW AX AZ BA BB BD BE BF BG BH BI BJ " +
	"BL BM BN BO BQ BR BS BT BV BW BY BZ CA CC CD CF CG CH CI CK CL CM CN CO CR " +
	"CU CV CW CX CY CZ DE DJ DK DM DO DZ EC EE EG EH ER ES ET FI FJ FK FM FO FR " +
	"GA GB GD GE GF GG GH GI GL GM GN GP GQ GR GS GT GU GW GY HK HM HN HR HT HU " +
	"ID IE IL IM IN IO IQ IR IS IT JE JM JO JP KE KG KH KI KM KN KP KR KW KY KZ " +
	"LA LB LC LI LK LR LS LT LU LV LY MA MC MD ME MF MG MH MK ML MM MN MO MP MQ " +
	"MR MS MT MU MV MW MX MY MZ NA NC NE NF NG NI NL NO NP NR NU NZ OM PA PE PF " +
	"PG PH PK PL PM PN PR PS PT PW PY QA RE RO RS RU RW SA SB SC SD SE SG SH SI " +
	"SJ SK SL SM SN SO SR SS ST SV SX SY SZ TC TD TF TG TH TJ TK TL TM TN TO TR " +
	"TT TV TW TZ UA UG UM US UY UZ VA VC VE VG VI VN VU WF WS XK YE YT ZA ZM ZW"

var countrySet = func() map[string]struct{} {
	fields := strings.Fields(countryCodes)
	set := make(map[string]struct{}, len(fields))
	for _, code := range fields {
		set[code] = struct{}{}
	}
	return set
}()

// IsCountryCode reports whether v is a country code the geo enricher can
// produce. $country survives from the client payload whenever enrichment
// resolves nothing, so a two-letter shape check is not enough to trust it.
func IsCountryCode(v string) bool {
	_, ok := countrySet[v]
	return ok
}
