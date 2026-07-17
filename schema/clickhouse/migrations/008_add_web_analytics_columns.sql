-- +goose Up

-- Web-analytics promoted columns (docs/architecture/web-analytics.md).
-- Types follow the migration-001 conventions: LowCardinality for
-- arbitrary-but-bounded tenant text (the utm_campaign precedent), plain String
-- for genuinely high-cardinality values (pathname/referrer/page_title, like
-- url/city). ADD COLUMN with DEFAULT '' is metadata-only — no part rewrite —
-- so THIS statement is instant on a live table; the backfill mutation below is
-- the opposite (it rewrites every part, mutations_sync = 2). Keep in sync with
-- internal/core/clickhouse/promoted_auto.go (promotedAutoColumns).
ALTER TABLE events
    ADD COLUMN IF NOT EXISTS pathname        String DEFAULT '',
    ADD COLUMN IF NOT EXISTS hostname        LowCardinality(String) DEFAULT '',
    ADD COLUMN IF NOT EXISTS referrer        String DEFAULT '',
    ADD COLUMN IF NOT EXISTS referrer_domain LowCardinality(String) DEFAULT '',
    ADD COLUMN IF NOT EXISTS channel         LowCardinality(String) DEFAULT '',
    ADD COLUMN IF NOT EXISTS locale          LowCardinality(String) DEFAULT '',
    ADD COLUMN IF NOT EXISTS screen_size     LowCardinality(String) DEFAULT '',
    ADD COLUMN IF NOT EXISTS utm_term        LowCardinality(String) DEFAULT '',
    ADD COLUMN IF NOT EXISTS utm_content     LowCardinality(String) DEFAULT '',
    ADD COLUMN IF NOT EXISTS page_title      String DEFAULT '';

-- Derivation backfill for pre-008 rows, mirroring internal/attribution
-- (attribution.Derive) — the Go enricher is the source of truth; this is its
-- one-shot SQL mirror for historical rows, pinned by
-- TestIntegrationWebAnalytics/mutation_008_matches_attribution_derive. Reading residual
-- auto_properties map keys is legitimate HERE (historical rows were never
-- split); the 009/010 rollup blocks must read only promoted columns.
--
-- Every assignment is guarded if(col != '', col, <derived>), making the
-- mutation idempotent — which is what lets it double as the manual repair when
-- rows land underived (a rollout window, or an aborted 009): re-run it, then
-- 009's DELETE + delta backfill. This is the only copy; 009 derives nothing and
-- reads an events table this has already derived.
-- ALTER ... UPDATE evaluates every assignment against the ORIGINAL row, so the
-- auto_properties rewrite at the bottom cannot starve the locale assignment
-- that reads the key it drops.
-- Notes on Go↔SQL mirroring:
--   * The host is domainRFC() over a userinfo-stripped string, NOT domain():
--     domain() resolves an IPv6 literal ("https://[::1]/p") to '' where
--     url.Parse resolves "::1", so domainRFC is required for that case. The
--     userinfo strip is a SEPARATE fix and load-bearing on its own: on
--     "https://user:pw@evil.com@real.com/p" domainRFC resolves evil.com where
--     Go's url.Parse resolves real.com (net/url splits the authority at the
--     LAST '@'), so without the strip history would book a host the visitor
--     never reached. The strip mirrors Go because "[^/?#]*@" is GREEDY: it
--     consumes through to the last '@' before any /?#, exactly Go's
--     strings.LastIndex(authority, "@") — do not make it lazy or add '@' to the
--     class, either reverts to first-'@' semantics and reintroduces the bug.
--     One spelling serves both url and referrer: the scheme group is optional,
--     so a protocol-relative "//host/p" strips identically, and the class
--     cannot reach past the authority into a path/query/fragment that merely
--     contains an '@'.
--   * Whether a value resolves at all turns on WHERE an invalid %-escape sits.
--     url.Parse UNESCAPES the path and the fragment, so a bad escape in either
--     fails the whole parse and Derive yields nothing — not even a hostname —
--     while RawQuery is stored verbatim, so the same bytes in the query void
--     only their own key (see the per-value guard below). path() and fragment()
--     are joined by '#' before the one match(): '#' is neither '%' nor a hex
--     digit, so it validates both regions at once without letting an escape
--     straddle the boundary Go validates separately.
--   * extractURLParameter reads the whole string INCLUDING the #fragment, which
--     url.Parse splits off before RawQuery ever exists — so a hash-router SPA's
--     "/#/dashboard?utm_source=newsletter" is Email here and Direct live unless
--     the fragment is cut first. cutFragment is applied at every extraction.
--   * Each extracted value is decoded only when its escaping is valid, else ''.
--     decodeURLComponent passes "100%off" through unchanged, but Go's
--     ParseQuery errors on that pair and DROPS the key (Query() keeps the
--     others), so an undecodable value must vanish rather than store raw. RE2
--     has no lookahead, hence the explicit alternation.
--   * '+' in query values is rewritten to '%20' before decodeURLComponent so
--     form-encoded spaces decode the way Go's url.Values does. The escape guard
--     runs on the RAW value, which is what ParseQuery validates.
--   * pathname is the DECODED path (Derive uses url.URL.Path, not
--     EscapedPath), so "/café" and "/caf%C3%A9" collapse onto one Pages-panel
--     row instead of fragmenting the permanent rollup. path() is fed
--     cutFragment(url) like every other extraction: it excludes the fragment
--     from its RESULT but still SCANS into it, so on a bare-origin hash route
--     ("https://app.example.com#/dashboard", no path segment before '#') its
--     first '/' is the fragment's — storing "/dashboard" where Derive, reading
--     url.URL.Path off the already-split fragment, yields "/".
--   * arrayMap over one-element arrays binds each derived input to a name
--     exactly once (mutations have no CTEs); arrayElement(..., 1) unwraps.
--   * `own` (the self-referral comparand) prefers the URL's host over the
--     hostname COLUMN, mirroring Derive: the column can echo a client-sent
--     $hostname, which must never steer the server-only referrer_domain. A
--     no-op for pre-008 rows (hostname is always '' there, so the url branch
--     wins anyway) — it matters when this mutation re-derives a post-008 row
--     whose referrer_domain is legitimately '' (a self-referral), where the
--     column-first order would un-blank what Derive blanked.
--   * The referrer's "www." strip is spelled out rather than using
--     domainWithoutWWW, which compares "www." CASE-SENSITIVELY and so leaves
--     "WWW.Shop.com" intact — lowercasing afterwards would store
--     "www.shop.com" where Go (stripOneWWW after ToLower) stores "shop.com",
--     defeating self-referral blanking and listing the tenant's own domain as
--     its top referrer. Lower-then-strip mirrors Go and matches `own` below.
--   * Host lowercasing uses lowerUTF8, NOT ASCII lower(): Go folds the host
--     with Unicode-aware strings.ToLower (parsePageURL / referrerDomain), so an
--     uppercase non-ASCII host ("ÜBER.example.de") must fold to
--     "über.example.de" here too. ASCII lower() leaves "Ü" intact and stores
--     "Über.example.de", splitting one site across two permanent
--     hostname/referrer_domain rollup dim_values (live vs backfilled) from a
--     one-shot irreversible mutation. Unreachable from a browser's
--     location.href (hosts arrive punycoded + lowercased) but reachable from a
--     non-browser SDK; pinned by the non_ascii_uppercase_host/referrer corpus
--     rows. The utm_source/utm_medium classification lowercasing below stays
--     ASCII lower() deliberately: it is only matched against the all-ASCII
--     channel source sets, so its casing can never change a channel and
--     mirroring ToLower there would be a no-op the parity test cannot observe.
--   * The locale trim is an explicit regexp, not trimBoth, which strips ONLY
--     ASCII 0x20. Go's strings.TrimSpace strips the whole unicode.IsSpace set —
--     [\t\n\v\f\r\x{0085}] plus \pZ (Zs/Zl/Zp) is exactly that set — and a
--     surviving separator fragments the Languages panel twice over: "en-US\n"
--     stores as-is AND misses the length(p)=2 region re-caser ("US\n" is 3
--     bytes), landing "en-us\n" where live traffic lands "en-US". The class is
--     codepoint-aware, so it cannot shear a continuation byte off "à".
--   * $screenWidth/$screenHeight read the Int64 slot, OR a String slot parsed
--     with toInt64OrNull, OR the Float64 slot (a JS SDK sends dims as doubles)
--     stringified then parsed the same way — mirroring handler.go's
--     autoPropInt64, which reads through autoprop.String (FormatFloat for a
--     double) and ParseInt, so an integral double resolves and a fractional
--     one does not. Client
--     auto-properties are never re-typed on the way in, so an SDK sending
--     stringValue would otherwise derive '' for all history while live traffic
--     derived "1920x1080". toInt64OrNull rejects " 123"/"12.5" exactly as Go's
--     strconv.ParseInt does, and a non-positive value fails the > 0 gate.
--   * A referrer resolves only when it carries an authority ("//host"), which
--     is what Go's url.Parse requires before it reports a Hostname — bare
--     ClickHouse domain() is LENIENT and would extract a host from a schemeless
--     "www.google.com/search" that Go reads as a path, so the two would
--     disagree on referrer_domain (and hence channel) without this gate.
--     A referrer that was sent but does not resolve is a signal, not silence:
--     refunres feeds the Unassigned rule so it can never book as Direct
--     (attribution.referrerDomain's `unresolved`; see channel.go rule 12).
--   * $locale is the ONE promoted key this mutation transforms rather than
--     copies, so its map residue is dropped: MergeIntoAutoProperties is
--     map-wins, so leaving it would answer "en-us" from the events API where
--     insights answer "en-US" off the normalized column. The verbatim-copied
--     keys ($referrer/$utmTerm/$utmContent/$pageTitle) merge to an identical
--     value and are deliberately left alone.
--   * The channel multiIf mirrors the Go rule table in
--     internal/attribution/channel.go EXACTLY and must be kept in sync;
--     TestIntegrationWebAnalytics/mutation_008_matches_attribution_derive fails
--     on any drift, including a reordering of the branches (cpm is both a paid
--     and a display medium, so rule ORDER alone decides its channel). A taxonomy
--     edit in channel.go is therefore a MIGRATION, not a refactor: update this
--     multiIf, then re-run this mutation + 009's DELETE and delta backfill so
--     historical rows reclassify to match live traffic.
-- Known limitations — divergences from Go this gate does NOT mirror, all
-- unreachable from a browser's location.href, so they are accepted rather than
-- chased into a longer statement:
--   - host parsing: an invalid port ("https://x.com:abc/p", which Go rejects
--     and this derives from), and a trailing-dot host ("https://x.com./p",
--     which Go resolves to "x.com." and domainRFC to '', so this derives
--     nothing).
--   - UTM extraction, where extractURLParameter is a byte-scanner and Go's
--     url.ParseQuery is a parser: a legacy ';' separator
--     ("?utm_source=google;utm_medium=cpc") — Go 1.17+ drops the pair and
--     yields "", this reads "google;utm_medium=cpc"; a bare '?' inside the
--     query ("?a=1?utm_source=google") — this reads "google", Go yields "";
--     and an encoded key ("?utm%5Fsource=google") — Go decodes it to
--     utm_source, this does not. Each mis-books a UTM (hence a channel) into
--     history that live traffic would never produce, but none occurs in a
--     real SDK-emitted URL.
-- mutations_sync = 2 blocks until the mutation finishes on every replica
-- (equivalent to 1 on a non-replicated table), so migration order is real.
ALTER TABLE events UPDATE
    pathname = if(pathname != '', pathname,
        if(match(url, '(?i)^https?://')
           AND domainRFC(replaceRegexpOne(url, '^(([a-zA-Z][a-zA-Z0-9+.-]*:)?//)[^/?#]*@', '\\1')) != ''
           AND NOT match(concat(path(url), '#', fragment(url)), '%(?:[^0-9A-Fa-f]|[0-9A-Fa-f][^0-9A-Fa-f]|[0-9A-Fa-f]?$)'),
           if(path(cutFragment(url)) = '', '/', decodeURLComponent(path(cutFragment(url)))), '')),
    hostname = if(hostname != '', hostname,
        if(match(url, '(?i)^https?://')
           AND domainRFC(replaceRegexpOne(url, '^(([a-zA-Z][a-zA-Z0-9+.-]*:)?//)[^/?#]*@', '\\1')) != ''
           AND NOT match(concat(path(url), '#', fragment(url)), '%(?:[^0-9A-Fa-f]|[0-9A-Fa-f][^0-9A-Fa-f]|[0-9A-Fa-f]?$)'),
           lowerUTF8(domainRFC(replaceRegexpOne(url, '^(([a-zA-Z][a-zA-Z0-9+.-]*:)?//)[^/?#]*@', '\\1'))), '')),
    referrer = if(referrer != '', referrer,
        coalesce(CAST(auto_properties['$referrer'] AS Nullable(String)), '')),
    referrer_domain = if(referrer_domain != '', referrer_domain,
        arrayElement(arrayMap((ref, own) ->
            arrayElement(arrayMap(dom -> if(own != '' AND dom = own, '', dom),
                [if(match(ref, '(?i)^([a-z][a-z0-9+.-]*:)?//')
                    AND domainRFC(replaceRegexpOne(ref, '^(([a-zA-Z][a-zA-Z0-9+.-]*:)?//)[^/?#]*@', '\\1')) != ''
                    AND NOT match(concat(path(ref), '#', fragment(ref)), '%(?:[^0-9A-Fa-f]|[0-9A-Fa-f][^0-9A-Fa-f]|[0-9A-Fa-f]?$)'),
                    arrayElement(arrayMap(h -> if(startsWith(h, 'www.'), substring(h, 5), h),
                        [lowerUTF8(domainRFC(replaceRegexpOne(ref, '^(([a-zA-Z][a-zA-Z0-9+.-]*:)?//)[^/?#]*@', '\\1')))]), 1),
                    '')]), 1),
            [if(referrer != '', referrer, coalesce(CAST(auto_properties['$referrer'] AS Nullable(String)), ''))],
            [arrayElement(arrayMap(h -> if(startsWith(h, 'www.'), substring(h, 5), h),
                [lowerUTF8(if(match(url, '(?i)^https?://')
                          AND domainRFC(replaceRegexpOne(url, '^(([a-zA-Z][a-zA-Z0-9+.-]*:)?//)[^/?#]*@', '\\1')) != ''
                          AND NOT match(concat(path(url), '#', fragment(url)), '%(?:[^0-9A-Fa-f]|[0-9A-Fa-f][^0-9A-Fa-f]|[0-9A-Fa-f]?$)'),
                          domainRFC(replaceRegexpOne(url, '^(([a-zA-Z][a-zA-Z0-9+.-]*:)?//)[^/?#]*@', '\\1')),
                          hostname))]), 1)]), 1)),
    channel = if(channel != '', channel,
        if(NOT (match(url, '(?i)^https?://')
                AND domainRFC(replaceRegexpOne(url, '^(([a-zA-Z][a-zA-Z0-9+.-]*:)?//)[^/?#]*@', '\\1')) != ''
                AND NOT match(concat(path(url), '#', fragment(url)), '%(?:[^0-9A-Fa-f]|[0-9A-Fa-f][^0-9A-Fa-f]|[0-9A-Fa-f]?$)')), '',
        arrayElement(arrayMap((ref, src, med, camp, term, cont, refunres) ->
            arrayElement(arrayMap((issearch, issocial, isvideo, ispaid) -> multiIf(
                ispaid AND issearch, 'Paid Search',
                ispaid AND issocial, 'Paid Social',
                ispaid AND isvideo, 'Paid Video',
                med IN ('display', 'banner', 'expandable', 'interstitial', 'cpm'), 'Display',
                ispaid, 'Paid Other',
                issearch OR med = 'organic', 'Organic Search',
                issocial OR med IN ('social', 'social-network', 'social-media', 'sm'), 'Organic Social',
                isvideo OR med = 'video', 'Organic Video',
                src IN ('email', 'e-mail', 'e_mail', 'newsletter') OR med IN ('email', 'e-mail', 'e_mail', 'newsletter'), 'Email',
                med = 'affiliate', 'Affiliate',
                ref != '', 'Referral',
                src != '' OR med != '' OR camp != '' OR term != '' OR cont != '' OR refunres, 'Unassigned',
                'Direct'),
                [(src IN ('google', 'bing', 'duckduckgo', 'ddg', 'yahoo', 'baidu', 'yandex', 'ecosia', 'brave', 'startpage', 'perplexity', 'qwant', 'kagi')
                  OR match(src, '(^|\\.)google\\.[a-z]{2,3}(\\.[a-z]{2,3})?$')
                  OR arrayExists(d -> src = d OR endsWith(src, concat('.', d)), ['bing.com', 'duckduckgo.com', 'yahoo.com', 'yahoo.co.jp', 'baidu.com', 'yandex.com', 'yandex.ru', 'ecosia.org', 'search.brave.com', 'startpage.com', 'perplexity.ai', 'qwant.com', 'kagi.com'])
                  OR ref = 'google'
                  OR match(ref, '(^|\\.)google\\.[a-z]{2,3}(\\.[a-z]{2,3})?$')
                  OR arrayExists(d -> ref = d OR endsWith(ref, concat('.', d)), ['bing.com', 'duckduckgo.com', 'yahoo.com', 'yahoo.co.jp', 'baidu.com', 'yandex.com', 'yandex.ru', 'ecosia.org', 'search.brave.com', 'startpage.com', 'perplexity.ai', 'qwant.com', 'kagi.com']))],
                [(src IN ('facebook', 'fb', 'instagram', 'ig', 'twitter', 'x', 'linkedin', 'tiktok', 'pinterest', 'reddit', 'threads', 'bluesky', 'bsky', 'hackernews', 'hn', 'whatsapp', 'telegram', 'mastodon')
                  OR arrayExists(d -> src = d OR endsWith(src, concat('.', d)), ['facebook.com', 'fb.com', 'fb.me', 'messenger.com', 'instagram.com', 'twitter.com', 'x.com', 't.co', 'linkedin.com', 'lnkd.in', 'tiktok.com', 'pinterest.com', 'pin.it', 'reddit.com', 'redd.it', 'threads.net', 'bsky.app', 'news.ycombinator.com', 'mastodon.social', 'whatsapp.com', 'telegram.org', 't.me'])
                  OR arrayExists(d -> ref = d OR endsWith(ref, concat('.', d)), ['facebook.com', 'fb.com', 'fb.me', 'messenger.com', 'instagram.com', 'twitter.com', 'x.com', 't.co', 'linkedin.com', 'lnkd.in', 'tiktok.com', 'pinterest.com', 'pin.it', 'reddit.com', 'redd.it', 'threads.net', 'bsky.app', 'news.ycombinator.com', 'mastodon.social', 'whatsapp.com', 'telegram.org', 't.me']))],
                [(src IN ('youtube', 'yt', 'vimeo', 'twitch', 'dailymotion')
                  OR arrayExists(d -> src = d OR endsWith(src, concat('.', d)), ['youtube.com', 'youtu.be', 'vimeo.com', 'twitch.tv', 'dailymotion.com'])
                  OR arrayExists(d -> ref = d OR endsWith(ref, concat('.', d)), ['youtube.com', 'youtu.be', 'vimeo.com', 'twitch.tv', 'dailymotion.com']))],
                [match(med, '^(.*cp.*|ppc|retargeting|paid.*)$')]), 1),
            [if(referrer_domain != '', referrer_domain,
                arrayElement(arrayMap((ref2, own2) ->
                    arrayElement(arrayMap(dom2 -> if(own2 != '' AND dom2 = own2, '', dom2),
                        [if(match(ref2, '(?i)^([a-z][a-z0-9+.-]*:)?//')
                            AND domainRFC(replaceRegexpOne(ref2, '^(([a-zA-Z][a-zA-Z0-9+.-]*:)?//)[^/?#]*@', '\\1')) != ''
                            AND NOT match(concat(path(ref2), '#', fragment(ref2)), '%(?:[^0-9A-Fa-f]|[0-9A-Fa-f][^0-9A-Fa-f]|[0-9A-Fa-f]?$)'),
                            arrayElement(arrayMap(h -> if(startsWith(h, 'www.'), substring(h, 5), h),
                                [lowerUTF8(domainRFC(replaceRegexpOne(ref2, '^(([a-zA-Z][a-zA-Z0-9+.-]*:)?//)[^/?#]*@', '\\1')))]), 1),
                            '')]), 1),
                    [if(referrer != '', referrer, coalesce(CAST(auto_properties['$referrer'] AS Nullable(String)), ''))],
                    [arrayElement(arrayMap(h -> if(startsWith(h, 'www.'), substring(h, 5), h),
                        [lowerUTF8(if(match(url, '(?i)^https?://')
                                  AND domainRFC(replaceRegexpOne(url, '^(([a-zA-Z][a-zA-Z0-9+.-]*:)?//)[^/?#]*@', '\\1')) != ''
                                  AND NOT match(concat(path(url), '#', fragment(url)), '%(?:[^0-9A-Fa-f]|[0-9A-Fa-f][^0-9A-Fa-f]|[0-9A-Fa-f]?$)'),
                                  domainRFC(replaceRegexpOne(url, '^(([a-zA-Z][a-zA-Z0-9+.-]*:)?//)[^/?#]*@', '\\1')),
                                  hostname))]), 1)]), 1))],
            [lower(if(utm_source != '', utm_source,
                arrayElement(arrayMap(v -> if(match(v, '%(?:[^0-9A-Fa-f]|[0-9A-Fa-f][^0-9A-Fa-f]|[0-9A-Fa-f]?$)'), '',
                    decodeURLComponent(replaceAll(v, '+', '%20'))), [extractURLParameter(cutFragment(url), 'utm_source')]), 1)))],
            [lower(if(utm_medium != '', utm_medium,
                arrayElement(arrayMap(v -> if(match(v, '%(?:[^0-9A-Fa-f]|[0-9A-Fa-f][^0-9A-Fa-f]|[0-9A-Fa-f]?$)'), '',
                    decodeURLComponent(replaceAll(v, '+', '%20'))), [extractURLParameter(cutFragment(url), 'utm_medium')]), 1)))],
            [if(utm_campaign != '', utm_campaign,
                arrayElement(arrayMap(v -> if(match(v, '%(?:[^0-9A-Fa-f]|[0-9A-Fa-f][^0-9A-Fa-f]|[0-9A-Fa-f]?$)'), '',
                    decodeURLComponent(replaceAll(v, '+', '%20'))), [extractURLParameter(cutFragment(url), 'utm_campaign')]), 1))],
            [multiIf(utm_term != '', utm_term,
                     coalesce(CAST(auto_properties['$utmTerm'] AS Nullable(String)), '') != '', coalesce(CAST(auto_properties['$utmTerm'] AS Nullable(String)), ''),
                     arrayElement(arrayMap(v -> if(match(v, '%(?:[^0-9A-Fa-f]|[0-9A-Fa-f][^0-9A-Fa-f]|[0-9A-Fa-f]?$)'), '',
                         decodeURLComponent(replaceAll(v, '+', '%20'))), [extractURLParameter(cutFragment(url), 'utm_term')]), 1))],
            [multiIf(utm_content != '', utm_content,
                     coalesce(CAST(auto_properties['$utmContent'] AS Nullable(String)), '') != '', coalesce(CAST(auto_properties['$utmContent'] AS Nullable(String)), ''),
                     arrayElement(arrayMap(v -> if(match(v, '%(?:[^0-9A-Fa-f]|[0-9A-Fa-f][^0-9A-Fa-f]|[0-9A-Fa-f]?$)'), '',
                         decodeURLComponent(replaceAll(v, '+', '%20'))), [extractURLParameter(cutFragment(url), 'utm_content')]), 1))],
            [arrayElement(arrayMap(rr -> rr != ''
                AND NOT (match(rr, '(?i)^([a-z][a-z0-9+.-]*:)?//')
                         AND domainRFC(replaceRegexpOne(rr, '^(([a-zA-Z][a-zA-Z0-9+.-]*:)?//)[^/?#]*@', '\\1')) != ''
                         AND NOT match(concat(path(rr), '#', fragment(rr)), '%(?:[^0-9A-Fa-f]|[0-9A-Fa-f][^0-9A-Fa-f]|[0-9A-Fa-f]?$)')),
                [if(referrer != '', referrer, coalesce(CAST(auto_properties['$referrer'] AS Nullable(String)), ''))]), 1)]), 1))),
    locale = if(locale != '', locale,
        arrayElement(arrayMap(raw -> if(raw = '', '',
            arrayStringConcat(arrayMap((p, i) -> multiIf(
                i = 1, lowerUTF8(p),
                length(p) = 2, upperUTF8(p),
                length(p) = 4, concat(upperUTF8(substring(p, 1, 1)), lowerUTF8(substring(p, 2))),
                lowerUTF8(p)),
                splitByChar('-', replaceAll(raw, '_', '-')),
                arrayEnumerate(splitByChar('-', replaceAll(raw, '_', '-')))), '-')),
            [replaceRegexpOne(replaceRegexpOne(coalesce(CAST(auto_properties['$locale'] AS Nullable(String)), ''),
                '^[\\t\\n\\v\\f\\r\\x{0085}\\pZ]+', ''), '[\\t\\n\\v\\f\\r\\x{0085}\\pZ]+$', '')]), 1)),
    screen_size = if(screen_size != '', screen_size,
        arrayElement(arrayMap((w, h) -> if(w > 0 AND h > 0, concat(toString(w), 'x', toString(h)), ''),
            [coalesce(variantElement(auto_properties['$screenWidth'], 'Int64'),
                      toInt64OrNull(variantElement(auto_properties['$screenWidth'], 'String')),
                      toInt64OrNull(toString(variantElement(auto_properties['$screenWidth'], 'Float64'))), 0)],
            [coalesce(variantElement(auto_properties['$screenHeight'], 'Int64'),
                      toInt64OrNull(variantElement(auto_properties['$screenHeight'], 'String')),
                      toInt64OrNull(toString(variantElement(auto_properties['$screenHeight'], 'Float64'))), 0)]), 1)),
    utm_source = if(utm_source != '', utm_source,
        if(match(url, '(?i)^https?://')
           AND domainRFC(replaceRegexpOne(url, '^(([a-zA-Z][a-zA-Z0-9+.-]*:)?//)[^/?#]*@', '\\1')) != ''
           AND NOT match(concat(path(url), '#', fragment(url)), '%(?:[^0-9A-Fa-f]|[0-9A-Fa-f][^0-9A-Fa-f]|[0-9A-Fa-f]?$)'),
           arrayElement(arrayMap(v -> if(match(v, '%(?:[^0-9A-Fa-f]|[0-9A-Fa-f][^0-9A-Fa-f]|[0-9A-Fa-f]?$)'), '',
               decodeURLComponent(replaceAll(v, '+', '%20'))), [extractURLParameter(cutFragment(url), 'utm_source')]), 1), '')),
    utm_medium = if(utm_medium != '', utm_medium,
        if(match(url, '(?i)^https?://')
           AND domainRFC(replaceRegexpOne(url, '^(([a-zA-Z][a-zA-Z0-9+.-]*:)?//)[^/?#]*@', '\\1')) != ''
           AND NOT match(concat(path(url), '#', fragment(url)), '%(?:[^0-9A-Fa-f]|[0-9A-Fa-f][^0-9A-Fa-f]|[0-9A-Fa-f]?$)'),
           arrayElement(arrayMap(v -> if(match(v, '%(?:[^0-9A-Fa-f]|[0-9A-Fa-f][^0-9A-Fa-f]|[0-9A-Fa-f]?$)'), '',
               decodeURLComponent(replaceAll(v, '+', '%20'))), [extractURLParameter(cutFragment(url), 'utm_medium')]), 1), '')),
    utm_campaign = if(utm_campaign != '', utm_campaign,
        if(match(url, '(?i)^https?://')
           AND domainRFC(replaceRegexpOne(url, '^(([a-zA-Z][a-zA-Z0-9+.-]*:)?//)[^/?#]*@', '\\1')) != ''
           AND NOT match(concat(path(url), '#', fragment(url)), '%(?:[^0-9A-Fa-f]|[0-9A-Fa-f][^0-9A-Fa-f]|[0-9A-Fa-f]?$)'),
           arrayElement(arrayMap(v -> if(match(v, '%(?:[^0-9A-Fa-f]|[0-9A-Fa-f][^0-9A-Fa-f]|[0-9A-Fa-f]?$)'), '',
               decodeURLComponent(replaceAll(v, '+', '%20'))), [extractURLParameter(cutFragment(url), 'utm_campaign')]), 1), '')),
    utm_term = if(utm_term != '', utm_term,
        multiIf(coalesce(CAST(auto_properties['$utmTerm'] AS Nullable(String)), '') != '',
                coalesce(CAST(auto_properties['$utmTerm'] AS Nullable(String)), ''),
                match(url, '(?i)^https?://')
                AND domainRFC(replaceRegexpOne(url, '^(([a-zA-Z][a-zA-Z0-9+.-]*:)?//)[^/?#]*@', '\\1')) != ''
                AND NOT match(concat(path(url), '#', fragment(url)), '%(?:[^0-9A-Fa-f]|[0-9A-Fa-f][^0-9A-Fa-f]|[0-9A-Fa-f]?$)'),
                arrayElement(arrayMap(v -> if(match(v, '%(?:[^0-9A-Fa-f]|[0-9A-Fa-f][^0-9A-Fa-f]|[0-9A-Fa-f]?$)'), '',
                    decodeURLComponent(replaceAll(v, '+', '%20'))), [extractURLParameter(cutFragment(url), 'utm_term')]), 1),
                '')),
    utm_content = if(utm_content != '', utm_content,
        multiIf(coalesce(CAST(auto_properties['$utmContent'] AS Nullable(String)), '') != '',
                coalesce(CAST(auto_properties['$utmContent'] AS Nullable(String)), ''),
                match(url, '(?i)^https?://')
                AND domainRFC(replaceRegexpOne(url, '^(([a-zA-Z][a-zA-Z0-9+.-]*:)?//)[^/?#]*@', '\\1')) != ''
                AND NOT match(concat(path(url), '#', fragment(url)), '%(?:[^0-9A-Fa-f]|[0-9A-Fa-f][^0-9A-Fa-f]|[0-9A-Fa-f]?$)'),
                arrayElement(arrayMap(v -> if(match(v, '%(?:[^0-9A-Fa-f]|[0-9A-Fa-f][^0-9A-Fa-f]|[0-9A-Fa-f]?$)'), '',
                    decodeURLComponent(replaceAll(v, '+', '%20'))), [extractURLParameter(cutFragment(url), 'utm_content')]), 1),
                '')),
    page_title = if(page_title != '', page_title,
        coalesce(CAST(auto_properties['$pageTitle'] AS Nullable(String)), '')),
    auto_properties = mapFilter((k, v) -> k != '$locale', auto_properties)
WHERE 1 SETTINGS mutations_sync = 2;

-- +goose Down

ALTER TABLE events
    DROP COLUMN IF EXISTS pathname,
    DROP COLUMN IF EXISTS hostname,
    DROP COLUMN IF EXISTS referrer,
    DROP COLUMN IF EXISTS referrer_domain,
    DROP COLUMN IF EXISTS channel,
    DROP COLUMN IF EXISTS locale,
    DROP COLUMN IF EXISTS screen_size,
    DROP COLUMN IF EXISTS utm_term,
    DROP COLUMN IF EXISTS utm_content,
    DROP COLUMN IF EXISTS page_title;
