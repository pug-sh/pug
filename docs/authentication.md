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

See [`config.example.json`](../config.example.json) for Google and generic OIDC examples. Provider IDs must be unique lowercase identifiers. Unknown fields, unsupported versions, duplicate IDs, missing values, and non-HTTPS remote issuers stop the server at startup with a configuration error. `http://localhost` issuers are accepted for development.

## Generic OIDC

Create a public SPA client in the identity provider and configure this exact redirect URI:

```text
https://YOUR_PUG_DASHBOARD/oauth/callback
```

Enable Authorization Code flow and PKCE (`S256`). Do not create or configure a client secret: the Pug dashboard is a browser application and cannot keep one secret. The provider must expose standard OIDC discovery metadata and return these claims in its ID token:

- `sub`
- `email`
- `email_verified: true`

`name` and `picture` are optional. Pug verifies the ID token's signature, issuer, audience, expiry, and verified-email claim on the server. It stores the durable external identity as the canonical issuer plus `sub`, so renaming the configured provider ID does not create another account.

The dashboard requests `openid profile email` by default. Override `scopes` only when the provider needs a different set; `openid` is always required. Pug does not request or retain an external refresh token, map provider groups to Pug roles, or initiate provider-wide logout in this first implementation.

## Existing Google configuration

`PUG_OAUTH_GOOGLE_CLIENT_ID` remains supported when `PUG_CONFIG_FILE` is unset. Do not set both: Pug rejects the ambiguous configuration at startup. New installations should put Google alongside any OIDC connections in the JSON file.
