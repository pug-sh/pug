-- +goose Up

-- Bot tagging (docs/architecture/bot-detection.md): ingest sets bot/bot_reason
-- on automated web-SDK traffic, and both rollups gain a `bot` key column the
-- migration-011 way (joins ORDER BY in the same ALTER, no DEFAULT — code 36) so
-- a later toggle can exclude on the fast path: WHERE bot = 0 on the event
-- rollup, but session-level (HAVING max(bot) = 0 after the merge) on the
-- session rollup — its key is per event, so a session whose events straddle
-- bot=0/1 sits in two rows and a row predicate would return half a session.
-- Nothing was tagged before this, so pre-012 rows reading bot = 0 is exact and
-- there is no backfill. Keep in sync with internal/core/clickhouse/promoted_auto.go.

ALTER TABLE events
    ADD COLUMN IF NOT EXISTS bot        Bool DEFAULT false,
    ADD COLUMN IF NOT EXISTS bot_reason LowCardinality(String) DEFAULT '';

ALTER TABLE dashboard_event_rollup_daily
    ADD COLUMN IF NOT EXISTS bot UInt8,
    MODIFY ORDER BY (project_id, kind, dim_name, day, dim_value, cookieless, bot);

ALTER TABLE dashboard_event_rollup_daily_mv MODIFY QUERY
SELECT
    project_id,
    toDate(occur_time) AS day,
    kind,
    dim.1 AS dim_name,
    dim.2 AS dim_value,
    count() AS cnt,
    uniqState(distinct_id) AS uniq_state,
    toUInt8(startsWith(distinct_id, 'cookieless-')) AS cookieless,
    toUInt8(bot) AS bot
FROM events
ARRAY JOIN [
    ('$__total__', ''),
    ('$country',        coalesce(country, '')),
    ('$region',         coalesce(region, '')),
    ('$city',           coalesce(city, '')),
    ('$os',             coalesce(os, '')),
    ('$browser',        coalesce(browser, '')),
    ('$device',         coalesce(device, '')),
    ('$platform',       coalesce(platform, '')),
    ('$utmSource',      coalesce(utm_source, '')),
    ('$utmMedium',      coalesce(utm_medium, '')),
    ('$utmCampaign',    coalesce(utm_campaign, '')),
    ('$pathname',       coalesce(pathname, '')),
    ('$hostname',       coalesce(hostname, '')),
    ('$referrerDomain', coalesce(referrer_domain, '')),
    ('$channel',        coalesce(channel, '')),
    ('$locale',         coalesce(locale, '')),
    ('$screenSize',     coalesce(screen_size, '')),
    ('$utmTerm',        coalesce(utm_term, '')),
    ('$utmContent',     coalesce(utm_content, '')),
    ('$browserVersion', coalesce(browser_version, '')),
    ('$osVersion',      coalesce(os_version, ''))
] AS dim
GROUP BY project_id, day, kind, dim_name, dim_value, cookieless, bot;

ALTER TABLE dashboard_session_rollup
    ADD COLUMN IF NOT EXISTS bot UInt8,
    MODIFY ORDER BY (project_id, kind, session_id, bot);

ALTER TABLE dashboard_session_rollup_mv MODIFY QUERY
SELECT
    project_id,
    kind,
    session_id,
    minState(occur_time) AS start_state,
    maxState(occur_time) AS end_state,
    countState() AS event_count_state,
    argMinState(url, occur_time) AS entry_url_state,
    argMaxState(url, occur_time) AS exit_url_state,
    argMinState(toString(country), occur_time) AS entry_country_state,
    argMaxState(toString(country), occur_time) AS exit_country_state,
    argMinState(toString(region), occur_time) AS entry_region_state,
    argMaxState(toString(region), occur_time) AS exit_region_state,
    argMinState(city, occur_time) AS entry_city_state,
    argMaxState(city, occur_time) AS exit_city_state,
    argMinState(toString(os), occur_time) AS entry_os_state,
    argMaxState(toString(os), occur_time) AS exit_os_state,
    argMinState(toString(browser), occur_time) AS entry_browser_state,
    argMaxState(toString(browser), occur_time) AS exit_browser_state,
    argMinState(toString(device), occur_time) AS entry_device_state,
    argMaxState(toString(device), occur_time) AS exit_device_state,
    argMinState(toString(platform), occur_time) AS entry_platform_state,
    argMaxState(toString(platform), occur_time) AS exit_platform_state,
    argMinState(toString(utm_source), occur_time) AS entry_utm_source_state,
    argMaxState(toString(utm_source), occur_time) AS exit_utm_source_state,
    argMinState(toString(utm_medium), occur_time) AS entry_utm_medium_state,
    argMaxState(toString(utm_medium), occur_time) AS exit_utm_medium_state,
    argMinState(toString(utm_campaign), occur_time) AS entry_utm_campaign_state,
    argMaxState(toString(utm_campaign), occur_time) AS exit_utm_campaign_state,
    argMinState(pathname, occur_time) AS entry_pathname_state,
    argMaxState(pathname, occur_time) AS exit_pathname_state,
    argMinState(toString(referrer_domain), occur_time) AS entry_referrer_domain_state,
    argMaxState(toString(referrer_domain), occur_time) AS exit_referrer_domain_state,
    argMinState(toString(channel), occur_time) AS entry_channel_state,
    argMaxState(toString(channel), occur_time) AS exit_channel_state,
    argMinState(toString(utm_term), occur_time) AS entry_utm_term_state,
    argMaxState(toString(utm_term), occur_time) AS exit_utm_term_state,
    argMinState(toString(utm_content), occur_time) AS entry_utm_content_state,
    argMaxState(toString(utm_content), occur_time) AS exit_utm_content_state,
    toUInt8(bot) AS bot
FROM (
    SELECT
        project_id,
        session_id,
        '' AS kind,
        occur_time,
        url,
        country,
        region,
        city,
        os,
        browser,
        device,
        platform,
        utm_source,
        utm_medium,
        utm_campaign,
        pathname,
        referrer_domain,
        channel,
        utm_term,
        utm_content,
        bot
    FROM events

    UNION ALL

    SELECT
        project_id,
        session_id,
        kind,
        occur_time,
        url,
        country,
        region,
        city,
        os,
        browser,
        device,
        platform,
        utm_source,
        utm_medium,
        utm_campaign,
        pathname,
        referrer_domain,
        channel,
        utm_term,
        utm_content,
        bot
    FROM events
) AS scoped_events
GROUP BY project_id, kind, session_id, bot;

-- +goose Down

-- Restore the 011 event-rollup and 010 session-rollup MV queries, then drop the
-- events columns. The rollup `bot` key columns cannot be dropped (sorting key);
-- new rows take the type default 0 — fine for dev.

ALTER TABLE dashboard_event_rollup_daily_mv MODIFY QUERY
SELECT
    project_id,
    toDate(occur_time) AS day,
    kind,
    dim.1 AS dim_name,
    dim.2 AS dim_value,
    count() AS cnt,
    uniqState(distinct_id) AS uniq_state,
    toUInt8(startsWith(distinct_id, 'cookieless-')) AS cookieless
FROM events
ARRAY JOIN [
    ('$__total__', ''),
    ('$country',        coalesce(country, '')),
    ('$region',         coalesce(region, '')),
    ('$city',           coalesce(city, '')),
    ('$os',             coalesce(os, '')),
    ('$browser',        coalesce(browser, '')),
    ('$device',         coalesce(device, '')),
    ('$platform',       coalesce(platform, '')),
    ('$utmSource',      coalesce(utm_source, '')),
    ('$utmMedium',      coalesce(utm_medium, '')),
    ('$utmCampaign',    coalesce(utm_campaign, '')),
    ('$pathname',       coalesce(pathname, '')),
    ('$hostname',       coalesce(hostname, '')),
    ('$referrerDomain', coalesce(referrer_domain, '')),
    ('$channel',        coalesce(channel, '')),
    ('$locale',         coalesce(locale, '')),
    ('$screenSize',     coalesce(screen_size, '')),
    ('$utmTerm',        coalesce(utm_term, '')),
    ('$utmContent',     coalesce(utm_content, '')),
    ('$browserVersion', coalesce(browser_version, '')),
    ('$osVersion',      coalesce(os_version, ''))
] AS dim
GROUP BY project_id, day, kind, dim_name, dim_value, cookieless;

ALTER TABLE dashboard_session_rollup_mv MODIFY QUERY
SELECT
    project_id,
    kind,
    session_id,
    minState(occur_time) AS start_state,
    maxState(occur_time) AS end_state,
    countState() AS event_count_state,
    argMinState(url, occur_time) AS entry_url_state,
    argMaxState(url, occur_time) AS exit_url_state,
    argMinState(toString(country), occur_time) AS entry_country_state,
    argMaxState(toString(country), occur_time) AS exit_country_state,
    argMinState(toString(region), occur_time) AS entry_region_state,
    argMaxState(toString(region), occur_time) AS exit_region_state,
    argMinState(city, occur_time) AS entry_city_state,
    argMaxState(city, occur_time) AS exit_city_state,
    argMinState(toString(os), occur_time) AS entry_os_state,
    argMaxState(toString(os), occur_time) AS exit_os_state,
    argMinState(toString(browser), occur_time) AS entry_browser_state,
    argMaxState(toString(browser), occur_time) AS exit_browser_state,
    argMinState(toString(device), occur_time) AS entry_device_state,
    argMaxState(toString(device), occur_time) AS exit_device_state,
    argMinState(toString(platform), occur_time) AS entry_platform_state,
    argMaxState(toString(platform), occur_time) AS exit_platform_state,
    argMinState(toString(utm_source), occur_time) AS entry_utm_source_state,
    argMaxState(toString(utm_source), occur_time) AS exit_utm_source_state,
    argMinState(toString(utm_medium), occur_time) AS entry_utm_medium_state,
    argMaxState(toString(utm_medium), occur_time) AS exit_utm_medium_state,
    argMinState(toString(utm_campaign), occur_time) AS entry_utm_campaign_state,
    argMaxState(toString(utm_campaign), occur_time) AS exit_utm_campaign_state,
    argMinState(pathname, occur_time) AS entry_pathname_state,
    argMaxState(pathname, occur_time) AS exit_pathname_state,
    argMinState(toString(referrer_domain), occur_time) AS entry_referrer_domain_state,
    argMaxState(toString(referrer_domain), occur_time) AS exit_referrer_domain_state,
    argMinState(toString(channel), occur_time) AS entry_channel_state,
    argMaxState(toString(channel), occur_time) AS exit_channel_state,
    argMinState(toString(utm_term), occur_time) AS entry_utm_term_state,
    argMaxState(toString(utm_term), occur_time) AS exit_utm_term_state,
    argMinState(toString(utm_content), occur_time) AS entry_utm_content_state,
    argMaxState(toString(utm_content), occur_time) AS exit_utm_content_state
FROM (
    SELECT
        project_id,
        session_id,
        '' AS kind,
        occur_time,
        url,
        country,
        region,
        city,
        os,
        browser,
        device,
        platform,
        utm_source,
        utm_medium,
        utm_campaign,
        pathname,
        referrer_domain,
        channel,
        utm_term,
        utm_content
    FROM events

    UNION ALL

    SELECT
        project_id,
        session_id,
        kind,
        occur_time,
        url,
        country,
        region,
        city,
        os,
        browser,
        device,
        platform,
        utm_source,
        utm_medium,
        utm_campaign,
        pathname,
        referrer_domain,
        channel,
        utm_term,
        utm_content
    FROM events
) AS scoped_events
GROUP BY project_id, kind, session_id;

ALTER TABLE events
    DROP COLUMN IF EXISTS bot,
    DROP COLUMN IF EXISTS bot_reason;
