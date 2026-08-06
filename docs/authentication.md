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

See [`config.example.json`](../config.example.json) for Google and another OIDC provider. Google uses the same generic OIDC path as every other provider. Provider IDs must be unique lowercase identifiers. Unknown fields, unsupported versions, duplicate IDs, missing values, and non-HTTPS remote issuers stop the server at startup with a configuration error. `http://localhost` issuers are accepted for development.

## Generic OIDC

Create an OIDC client in the identity provider and configure this exact redirect URI:

```text
https://YOUR_PUG_DASHBOARD/oauth/callback
```

Enable Authorization Code flow and PKCE (`S256`). Configure `clientSecret` only when the provider requires a confidential client. Pug keeps it in the server configuration, never returns it from `GetAuthConfig`, and performs the code exchange on the server. Public clients such as a typical Keycloak SPA omit it. The provider must expose standard OIDC discovery metadata and return these claims in its ID token:

- `sub`
- `email`
- `email_verified: true`

`name` and `picture` are optional. Pug verifies the ID token's signature, issuer, audience, expiry, and verified-email claim on the server. It stores the durable external identity as the canonical issuer plus `sub`, so renaming the configured provider ID does not create another account.

The dashboard requests `openid profile email` by default. Override `scopes` only when the provider needs a different set; `openid` is always required. Pug does not request or retain an external refresh token, map provider groups to Pug roles, or initiate provider-wide logout in this first implementation.

## Google

Configure Google as an OIDC provider with issuer `https://accounts.google.com`, its client ID, and its client secret, as shown in the example config. Register the same `/oauth/callback` redirect URI in the Google OAuth client. The secret remains server-side; Google otherwise uses the same OIDC flow as every other provider.

The legacy `PUG_OAUTH_GOOGLE_CLIENT_ID` configuration and Google-specific ID-token endpoint have been removed. This is a breaking change: existing installations using that variable must move Google into `PUG_CONFIG_FILE`.
