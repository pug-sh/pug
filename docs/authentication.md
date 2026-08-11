# Authentication providers

Pug supports password and magic-link sign-in without extra configuration. External sign-in providers are configured on the server in a versioned JSON file; the dashboard reads the safe browser settings from Pug's public auth API.

Set `PUG_CONFIG_FILE` to the file mounted in the server container:

```yaml
services:
  server:
    environment:
      PUG_CONFIG_FILE: /etc/pug/config.json
    volumes:
      - ./config.json:/etc/pug/config.json:ro
```

See [`config.example.json`](../config.example.json) for Google and another OIDC provider. Google uses the same generic OIDC path as every other provider. Unknown fields, unsupported versions, duplicate IDs, missing values, and non-HTTPS remote issuers stop the server at startup with a configuration error. `http://localhost` issuers are accepted for development.

A provider `id` must be a unique lowercase identifier, and it is **permanent**: it is stored as the `provider` value on every account linked through it. Renaming an `id` orphans those links — affected users fall back to matching on their verified email, which signs them in but leaves a stale row behind. Pick the id once (`google`, `okta`, `company_sso`) and change the `displayName` instead when you want different wording on the sign-in button.

## Generic OIDC

Create an OIDC client in the identity provider and configure this exact redirect URI:

```text
https://YOUR_PUG_DASHBOARD/oauth/callback
```

Enable Authorization Code flow and PKCE (`S256`). Configure `clientSecret` only when the provider requires a confidential client. Pug keeps it in the server configuration, never returns it from `GetAuthConfig`, and performs the code exchange on the server. Public clients such as a typical Keycloak SPA omit it. The provider must expose standard OIDC discovery metadata and return these claims in its ID token. Discovery runs on the first sign-in rather than at startup, so an issuer that is unreachable when the server boots does not stop it from starting, and starts working again on its own once it recovers — sign-ins against it fail with `OAUTH_PROVIDER_UNAVAILABLE` meanwhile:

- `sub`
- `email`
- `email_verified: true`

`name` and `picture` are optional. Pug verifies the ID token's signature, issuer, audience, expiry, `nonce`, and verified-email claim on the server. The external identity is stored as the provider `id` plus the token's `sub`; since each provider is one issuer and client, `sub` is unique within it.

The browser must send a `nonce` on the authorization request and pass the same value to `CompleteOIDCSignIn`; Pug rejects the sign-in if it does not match the ID token's `nonce` claim. Pug does not mint or store the nonce, so that check confirms the token belongs to the request that carried it — the binding to the browser that started the flow comes from PKCE and from the `state` value the browser must generate, store, and re-check on the callback.

Pug requires the `redirect_uri` to use the path `/oauth/callback`, with no query or fragment, over HTTPS except on `localhost`/`127.0.0.1`/`::1`; when the browser sends an `Origin` header it must be same-origin with that URI. The host itself is pinned by the redirect URI you register at the IdP, not by Pug.

The dashboard requests `openid profile email` by default. Override `scopes` only when the provider needs a different set; `openid` and `email` are always required, since sign-in resolves accounts on a verified email claim. Pug does not request or retain an external refresh token, map provider groups to Pug roles, or initiate provider-wide logout in this first implementation.

## Google

Configure Google as an OIDC provider with issuer `https://accounts.google.com`, its client ID, and its client secret, as shown in the example config. Register the same `/oauth/callback` redirect URI in the Google OAuth client. The secret remains server-side; Google otherwise uses the same OIDC flow as every other provider.

The legacy `PUG_OAUTH_GOOGLE_CLIENT_ID` configuration and Google-specific ID-token endpoint have been removed. This is a breaking change, and the variable is now ignored rather than rejected — an install that upgrades without setting `PUG_CONFIG_FILE` starts cleanly with no external providers and Google sign-in absent — the server logs a startup warning naming the ignored variable.

Name the Google entry `"id": "google"` to keep existing Google accounts linked: that is the value they were already stored under, so they resolve directly with no migration. Under any other id they still sign in — via the verified-email fallback — but pick up a second identity row.
