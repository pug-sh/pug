# SSO: Google Workspace and verified domains

> **Status: phases 1 and 2 implemented, phase 3 is design.** Written
> 2026-09-25; phases 1 and 2 built 2026-09-26. The Email Verified test below is
> still to do, and it decides whether `hd` alone can prove a domain.

## Summary

This adds five things. Org admins control all of them.

1. **Verified domain.** The org proves it owns `acme.com`, with a DNS TXT record
   or through the server's operator.
2. **Auto-join.** An org setting, off by default. When an admin turns it on,
   anyone who signs in through SSO with an account on the org's domain joins the
   org, with a role the admin picks. They do **not** get the default org that
   sign-up creates today.
3. **Let members create their own orgs.** An org setting, on by default. When an
   admin turns it off, people with an email on the org's domain cannot create
   new orgs, and they get no default org at sign-up.
4. **Require SSO.** A per-domain switch. `@acme.com` accounts can then only sign
   in through SSO. Passwords and email links stop working for them.
5. **Company SSO connections.** An org admin connects the company's own identity
   providers (Okta, Entra ID, Keycloak…) through OIDC, from the UI, and picks
   the domains each one signs in. Sign-in asks for the email first and sends the
   person to the right one.

Several orgs can verify the same domain. Each one gets all of these settings,
except that a domain has only one SSO connection. When their limits differ, the
strictest wins.

Phase 1 ships 1 to 3. Phase 2 ships 4. Phase 3 ships 5. Everything works the same
on cloud and on self-hosted servers, and on every plan: Pug does not gate
features by plan.

In this doc, "SSO" means signing in through a provider that speaks for the
domain: Google Workspace, or the company's own identity provider.

## Why Google needs no per-org setup

Pug already has Google sign-in: one OAuth client in `PUG_CONFIG_FILE`, shared by
every org. That is all Google SSO needs.

Google adds an `hd` ("hosted domain") claim to the ID token of every Google
Workspace account. `hd: "acme.com"` means Acme's Workspace owns the account, and
Acme's IT admin can suspend it. Google lets a company add a domain to its
Workspace only after it proves it owns that domain. So nobody else can get a
token with `hd: "acme.com"`.

So Google SSO for Acme is Pug's existing Google sign-in plus one check: `hd` is
a domain Acme has verified in Pug. There is no client id or secret per org.

Two rules follow from this:

- **A Google account without `hd` is a personal account.** It can use any email,
  such as `bob@acme.com`. Google checked that address once, when the account was
  made. It does not know if Bob still works at Acme. We never treat it as proof
  of a domain.
- **`hd` is trusted only from Google's issuer** (`https://accounts.google.com`).

Pug still needs its own proof that the org owns the domain. Without it, anyone
could claim `acme.com` and pull Acme's people into their org.

One case needs a test before phase 1 ships. Google also lets someone sign up
for Workspace with just an email address and verify the domain later. The
domain can stay unverified for up to 21 days, or longer once billing is set up.
In July 2024, Google said attackers had created such "Email Verified" accounts
and used them to sign in to other apps with Sign in with Google. Google does not
say whether their tokens carry `hd`. If they do, anyone who can read one
`acme.com` inbox can get `hd: "acme.com"`. That is no stronger than an email
link (open question 1), and `hd` alone could not prove a domain. The test: sign
up for Workspace with only an email address, sign in to a test client, and read
the ID token.

## Cloud and self-hosted

It is the same code on both. Three things differ.

| | Cloud | Self-hosted |
|---|---|---|
| Google SSO | Works for every org with no setup, through Pug's shared Google client. The admin only verifies the domain and turns on the settings. | The operator adds their own Google client to `PUG_CONFIG_FILE` once. `hd` works the same with any client. |
| Company identity providers (Okta, Keycloak, Entra ID…) | Per org, set up by org admins (phase 3) | Per org (phase 3), or for the whole server in `PUG_CONFIG_FILE` with `emailDomains` |
| Verifying a domain | DNS TXT record. The pug team can also run `pug domains verify` for support. | DNS TXT record, or the operator runs `pug domains verify` |

Everything else is shared: the two org settings, no default org for people who
join, Require SSO, and company SSO connections.

### Google on a self-hosted server

The operator creates a Google OAuth client of type "Web application", with the
redirect URI `https://<pug host>/oauth/callback`. They add it to
`PUG_CONFIG_FILE` with the issuer `https://accounts.google.com`, as today.

Two limits come from Google:

- The redirect URI must be on a public domain name, such as `pug.acme.com`. The
  name may resolve only inside the company, because the browser does the
  redirect.
- The Pug server needs outbound HTTPS to Google, to exchange the code and to
  fetch Google's signing keys. An air-gapped server cannot use Google. It can use
  the company's own identity provider instead.

Tip: set the Google consent screen to "Internal". Then only accounts from the
company's own Workspace can sign in with Google.

### Company identity providers in `PUG_CONFIG_FILE`: `emailDomains`

A self-hosted operator can also add the company's provider for the whole server.
It then shows as a button on the sign-in page. These providers have no `hd`
claim, but the operator set them up, so the operator can say which email domains
they speak for. A new optional provider field does that:

```json
{
  "id": "okta",
  "type": "oidc",
  "displayName": "Acme SSO",
  "clientId": "…",
  "clientSecret": "…",
  "issuerUrl": "https://acme.okta.com",
  "emailDomains": ["acme.com"]
}
```

A sign-in through this provider, with a verified `@acme.com` email, proves
`acme.com`. It counts exactly like Google's `hd`. So auto-join, and Require SSO
in phase 2, work with it too.

1. List domains only on a provider the company runs. Never on a shared provider
   that anyone can sign up to.
2. Config validation rejects `emailDomains` on Google's issuer. Google proves
   domains through `hd` only.
3. A provider without `emailDomains` works as today. It never proves a domain.

Cloud does not use this field. Every provider in the file shows a button on the
one shared sign-in page, so one customer's Okta would show "Acme SSO" to
everyone. Cloud companies use phase 3's per-org connections instead.

### Providers that don't send `email_verified`

Pug refuses a sign-in unless the provider marks the email verified. Microsoft
Entra ID never sends `email_verified`. So today an Entra provider in
`PUG_CONFIG_FILE` fails every sign-in. Two rules fix that:

1. An email on a domain the provider speaks for needs no `email_verified`. That
   means a domain in the provider's `emailDomains` or, in phase 3, a domain its
   connection lists. The domain proof already makes the provider the authority
   for those addresses.
2. When `email_verified` is absent, Entra's optional `xms_edov` claim ("email
   domain owner verified") counts in its place. The Entra admin adds it to the
   app registration. This lets an Entra provider without `emailDomains` sign
   people in. Like `email_verified`, it is the provider's word, so it needs no
   issuer check.

An explicit `email_verified: false` is always refused. Pug never overrules a
provider that says an address is unverified. An explicit `xms_edov: false` is
refused too, even on a domain in `emailDomains`.

### Verifying a domain without DNS

DNS verification needs the Pug server to see the TXT record. Some self-hosted
servers cannot: they have no outbound DNS, or their DNS differs from the public
one. And on a self-hosted server the operator already controls everything, so a
DNS proof adds little.

So the operator can mark a domain verified directly:

```
./bin/pug domains verify <org-id> acme.com
./bin/pug domains show acme.com              # every org that added it
./bin/pug domains release <org-id> acme.com  # drop one org's claim, e.g. in a dispute
./bin/pug domains unenforce acme.com         # turn Require SSO off in every org (phase 2)
```

Like `pug billing`, it talks only to Postgres. Cloud has it too, and the pug team
uses it for support cases. The tab shows such a domain as "Verified by your
administrator".

Running `verify` on a domain the org already verified through DNS switches it to
operator-verified. A setting change then no longer re-checks its record.

The CLI never changes the org settings; those stay the org admins' choice. The
one exception is `unenforce`, the way back when SSO breaks and nobody on the
domain can sign in to turn Require SSO off.

Org admins cannot skip DNS. On a server with open sign-up, anyone can create an
org and become its admin.

### Existing self-hosted servers

Many self-hosted servers already list their company's identity provider in
`PUG_CONFIG_FILE`. Upgrading changes nothing for them:

- Their providers, provider ids and linked accounts keep working. Nothing is
  renamed or migrated.
- Sign-in works exactly as today until an admin verifies a domain and changes a
  setting. Every new setting starts at today's behavior: auto-join off, org
  creation allowed, Require SSO off.
- `emailDomains` is optional. A config file without it loads as before.
- One fix: an Entra ID provider fails every sign-in today. It starts working
  once it sends `xms_edov` or lists `emailDomains` (see "Providers that don't
  send `email_verified`").
- One new refusal: a provider email with non-ASCII characters can't create an
  account or link to one, because account lookups fold some of those characters
  into ASCII. An account already linked to the provider keeps signing in.

To use the new features:

1. The operator adds `emailDomains` to the company provider. Google needs
   nothing here, because it uses `hd`.
2. The operator runs `pug domains verify <org-id> acme.com`, or an org admin
   verifies through DNS.
3. An org admin turns on the settings they want.

After that, new people who sign in through SSO join the org and get no default
org. People who already have accounts join at their next SSO sign-in. They keep
the default orgs they got before; Pug never deletes those. Nobody is signed out
unless Require SSO is turned on.

One thing to watch: the config parser rejects unknown fields. Once
`emailDomains` is in the file, an older Pug version refuses to start. So add it
only after every server runs the new version, and remove it before any
rollback.

`PUG_CONFIG_FILE` stays the place for server-wide providers. Phase 3's per-org
connections live in Postgres and add to the file. They never replace it.

## UX

### Admin: the SSO & domains tab

A new tab, **Settings → SSO & domains**, for admins only. It holds the two org
settings, the list of domains and, in phase 3, the SSO connections.

```
SSO & domains
─────────────────────────────────────────────────────────────────
Auto-join                                                 [ off ]
  People who sign in through SSO with an email on a verified
  domain join this org.
  Role for new people   [ Viewer ▾ ]

Let members create their own orgs                         [ on  ]
  When off, people with an email on a verified domain can't
  create new orgs, and get no default org when they sign up.
  Admins of this org still can.
─────────────────────────────────────────────────────────────────
acme.com    ● Verified                                          🗑
  Require SSO                                             [ on  ]
acme.io     ○ Pending    [Verify now]                           🗑
─────────────────────────────────────────────────────────────────
[+ Add domain]
```

Turning auto-join on, or org creation off, needs a verified domain and re-checks
the org's DNS records (see "Re-checking the record"). Going back to the default
always works.

- **Auto-join** is off by default, and only an admin can turn it on. The role can
  be Viewer or Member; auto-join never gives Admin. Turning it off stops new
  joins. People who already joined stay.
- **Let members create their own orgs** is on by default, which is how Pug works
  today. Turning it off does not touch orgs that already exist.

If another org that verified the same domain has a stricter limit, the tab says
so next to the setting: "Also turned off by another org that verified
acme.com."

### Admin: add a domain

"Add domain" opens:

```
Add a domain

  Domain  [ acme.com                ]

  Add this record at your DNS provider, and keep it there:
    Type   TXT
    Name   _pug-verification.acme.com          [copy]
    Value  pug-verification=7f3c9a41…          [copy]

  DNS changes can take a few minutes to show up.

                                         [ Verify now ]
```

| Result | What the admin sees |
|---|---|
| Record found | The domain turns "Verified". |
| Record not found | "We couldn't find the record yet. Check it and try again." |

Each org gets its own value, so a second org adds a second TXT record. DNS allows
several records with the same name.

Pug checks the record again whenever the org turns on a setting that acts on
other people (see "Re-checking the record"). So admins must keep it.

There is no shortcut through another org that already verified the domain, even
for an admin of both. Such a claim would have no TXT record of its own. A later
re-check could never remove it, and it would outlive the admin who made it.

A pending domain stays in the list. The admin can press Verify again later.

Verifying a domain changes nothing while the org's settings are at their
defaults. The settings apply to every domain the org verified. A domain verified
while a setting is on gets it at once, even through `pug domains verify`.

### Admin: Require SSO

Each verified domain's row has a **Require SSO** switch (phase 2), under its
record.

```
acme.com                                          Verified   🗑
  Require SSO                                             [ off ]
    acme.com accounts must sign in through SSO.
    Passwords and email links stop working for them.
    People signed in another way are signed out within a day.
    So are SSO sessions started before Require SSO was available.
```

The switch says "through SSO" rather than naming a provider. The blocked screen
people see at sign-in names the providers (see "People signing in"). If another
org that verified `acme.com` has turned it on, the row shows "Also required by
another organization that verified acme.com."

The two org settings decide what happens in this org. Require SSO decides how
everyone with an `acme.com` email signs in to Pug, in any org. That is why it
sits on the domain.

"Require SSO" stays disabled until at least one person has signed in through SSO
with an `acme.com` account. The hint says: "Sign in once through SSO with an
account on acme.com to turn this on." This stops an admin from locking out a
company that does not use that provider. Require SSO has no admin exception, so
such a lockout would include the admins.

### People signing in

With auto-join on, and Viewer picked:

| Who | Does | Result |
|---|---|---|
| New person with an `acme.com` Workspace account | Continue with Google | Account created. Joins Acme as Viewer, plus every other org that verified `acme.com` and has auto-join on. No default org. Lands in Acme. |
| New person at Acme, which has a company SSO connection | Types their email, then Continue with Acme SSO | Same as the row above. |
| Existing Pug user `bob@acme.com`, has their own org | Signs in through SSO | Also joins Acme. Keeps their own org. Toast: "You joined Acme." |
| Existing user, signed in through SSO | Keeps using the app | Joins within a day, at the next session refresh. The org shows up the next time the app loads the org list. |
| Existing user, signed in before this shipped or without SSO | Keeps using the app | Joins at their next SSO sign-in. An admin can invite them to skip the wait. |
| Anyone using an email link or a password | Signs in | No auto-join (see open question 1). A new account gets a default org, as today. |
| Contractor `jane@gmail.com` | Anything | No change. Needs an invite, as today. |
| Someone removed from Acme, or who left | Keeps using the app, or signs in again | Added back at their next session refresh or SSO sign-in (see "Removal doesn't stick yet"). |

With auto-join off, nothing changes from today. A new account gets a default
org, and people join only by invite.

With **Let members create their own orgs** off:

| Who | Does | Result |
|---|---|---|
| New `acme.com` person, auto-join on | Signs in through SSO | Joins Acme, as above. |
| New `acme.com` person, auto-join off | Signs in | Account created, with no org. Screen: "You're not in an org yet. Ask an admin to invite you." |
| `bob@acme.com` | Wants a new org | The "New organization" button is hidden. The server also refuses: "Your company doesn't allow creating new orgs." |
| An admin of Acme | Creates an org | Allowed. |
| Contractor `jane@gmail.com`, a member of Acme | Creates an org | Allowed. Acme doesn't own her address. |

With **Require SSO** on (phase 2):

| Who | Does | Result |
|---|---|---|
| `bob@acme.com` | Password or email link | Blocked: "acme.com accounts sign in with Google." Button: Continue with Google. |
| `bob@acme.com` with a personal Google account (no `hd`) | Continue with Google | Blocked: "Use your acme.com Google Workspace account." |
| `bob@acme.com`, signed in without SSO before the switch | Keeps using the app | Signed out within a day, at the next session refresh. Signs in through SSO. |
| `bob@acme.com`, signed in through SSO | Keeps using the app | No change. |
| Invited `carol@acme.com` | Clicks the invite link | Asked to sign in through SSO. The invite is accepted after that. |

The blocked screen:

```
  acme.com accounts sign in with Google

  [ G  Continue with Google ]

  Use a different email
```

It shows one button per provider that can sign the domain in: the
`PUG_CONFIG_FILE` providers whose `emailDomains` list it or, when none does,
Google. The `SSO_REQUIRED` error carries that list, so the screen needs no
second call. The Google button sends `hd=acme.com` to Google, and
`login_hint=bob@acme.com` when the person typed their email, so Google's account
picker shows only Acme accounts. This is only a UI hint. The server still checks
the `hd` claim in the token.

The screen appears wherever the refusal happens: on the sign-in page, on the
email link's page, after a provider sign-in that didn't prove the domain ("Use
your acme.com Google Workspace account."), and on the sign-in page after a
refused session refresh.

### Members page

- Auto-joined people get a badge: `via acme.com`.
- For an auto-joined person, the remove dialog adds one line: "While auto-join
  is on, they rejoin at their next session refresh or SSO
  sign-in." (see "Removal doesn't stick yet").

## Rules

### Grants need proof, limits don't

A setting that **gives** access (auto-join) needs a sign-in that proves the
domain. A setting that **limits** (org creation, Require SSO) applies to every
account whose email is on the domain, however it signed in. A limit can't be
dodged by signing in a weaker way.

### What proves a domain

| Sign-in | Proves `acme.com`? |
|---|---|
| Google with `hd = acme.com` | Yes |
| Google without `hd` (personal account) | No |
| A `PUG_CONFIG_FILE` provider whose `emailDomains` lists `acme.com`, with an `…@acme.com` email | Yes |
| The connection that speaks for `acme.com` (phase 3, rule 1), with an `…@acme.com` email | Yes |
| Any other provider | No |
| Email link to `…@acme.com` | No, in v1 (open question 1) |
| Password | No |

### Auto-join

On a sign-in that proves domain D, auto-join runs for every org O that verified
D and has auto-join on:

1. If they are already a member of O, skip O. Their role never changes.
2. If O has a pending, unexpired invite for their email, skip O. The invite's
   role applies when they accept it. Otherwise auto-join would get there first.
   Accepting would then only mark the invite accepted, and the person would keep
   the auto-join role.
3. Otherwise, add them to O with O's auto-join role.

One sign-in can add a person to several orgs.

The account's own email must be on D too. A sign-in finds a returning person by
`sub`, so their account can be on another domain than the one the sign-in
proved. Require SSO for D would never cover that account, so auto-join skips it.

This runs inside the sign-in transaction. Account creation, auto-join and the new
session commit together or not at all. It is also safe when two sign-ins of one
person race.

### Sessions remember the proven domain

Most people stay signed in for months: a refresh token slides for 90 days with
use. So "join at the next sign-in" could mean never.

So a session remembers the domain its SSO sign-in proved. Each session refresh,
about once a day, runs auto-join again for that domain. When an admin turns
auto-join on, or another org verifies the domain, active SSO users join within a
day.

This still needs proof. Only sessions that started with an SSO sign-in carry a
domain. Password and email-link sessions carry none, and neither do sessions from
before this ships. Those people join at their next SSO sign-in.

At refresh, auto-join runs after the refresh commits. If it fails, Pug logs the
error and the refresh still succeeds. A failed refresh
would lock people out: the frontend keeps the session, and every request fails.

### Org creation

"Let members create their own orgs" is off for D when any org that verified D
turns it off. Then, for every account whose email is on D:

1. `OrgsService.Create` refuses with `ORG_CREATION_RESTRICTED`. Admins of any
   org that verified D are not blocked.
2. A new account gets no default org at sign-up.

Orgs that already exist are not touched. People on other domains, such as a
contractor on `gmail.com`, are not affected, because the company does not own
their addresses.

### No default org for people who join

Today every new account gets an org named "default" with one project. The new
rule, in `FinishSignup`:

```
joined = orgs this sign-in added the person to (by invite or auto-join)
if the account is new and joined is empty and org creation is allowed for its email:
    create the default org and project, as today
```

A new Acme person who auto-joins gets only the orgs that added them. While
auto-join is off, new Acme people get a default org, as before, unless org
creation is also off. Then they start with no org until someone invites them.

### Removal doesn't stick yet

While auto-join is on, a removed member, or one who left, is added back at their
next session refresh or SSO sign-in. The only way to keep one person out is to
leave auto-join off.

This is deferred. The fix is an exclusion row per removed person. `RemoveMember`
and `Leave` write it in the delete's transaction, auto-join skips excluded
people, and accepting a new invite deletes it. Auto-join then also needs to check
for the membership itself. Without that check, a refresh that runs while a
removal is still uncommitted waits for the delete, then adds the person back,
because it can't see the new exclusion yet.

### Require SSO (phase 2)

Require SSO is on for D when any org that verified D turns it on. It applies to
every account whose email is on D, in any org. That includes admins; there is no
exception. The company owns those addresses.

| Path | When D requires SSO |
|---|---|
| `SignInWithEmail` (password) | Error `SSO_REQUIRED` |
| `RequestMagicLink` | `SSO_REQUIRED`. No email is sent. |
| `CompleteMagicLink` (links sent earlier) | `SSO_REQUIRED`. The token is not used up. For an invite link the error says so, and the frontend passes the token on to the SSO sign-in (see "Invites through SSO"). |
| `CompleteOIDCSignIn` | Allowed only if the sign-in proves D. Checked for the token's email and for the account the sign-in resolves to. |
| `RefreshSession` | Renews only sessions whose SSO sign-in proved D. Refuses the rest with `Unauthenticated` and revokes their refresh token. |
| `SetPassword` | `SSO_REQUIRED` |
| `DemoSignIn` | Not checked, and neither are its sessions at refresh. A refresh token doesn't record how it was issued, so `RefreshSession` spots a demo session by its account's email, `DemoViewerEmail`. The demo viewer (`snoop@pug.sh`) needs no credentials anyway, and requiring SSO for `pug.sh` must not break the public demo. |

A company's second Workspace domain needs no extra rule. Google describes `hd`
as "the host domain of the user's Google Workspace email address", and a 2015
report shows a user on a secondary domain getting that domain as `hd`. So
`bob@acme.io` proves `acme.io`, even when the Workspace's main domain is
`acme.com`. If that turns out wrong, Require SSO just can't be turned on for
`acme.io`, because no sign-in would ever prove it.

The password and email-link checks look only at the email's domain, before any
account lookup. So `SSO_REQUIRED` tells a caller that `acme.com` requires SSO. It
does not tell them whether an account exists. `RequestMagicLink` keeps its "no
account oracle" promise.

`CompleteOIDCSignIn` also checks the account it resolves to. A returning identity
is found by its `sub`, not its email. So a personal Google account linked to
`bob@acme.com`, whose email later changed, would otherwise sign in to the
`acme.com` account unchecked. This check reveals nothing, because the caller has
already signed in at the provider.

Nothing is revoked in bulk when the switch turns on. A session that did not
start through SSO ends at its next refresh, which revokes its refresh token. Its
access token works until it expires, at most 24h, and then the person signs in
through SSO. The frontend drops a refused token anyway, so revoking it only
stops a copy held elsewhere from coming back after `unenforce`.

Sessions whose SSO sign-in proved D keep working, the admin's included. A
refresh token issued before phase 1 has no `proven_domain`, so its session ends
too, even if it started through SSO.

`RefreshSession` refuses such a session with `Unauthenticated`, not
`FailedPrecondition`. The frontend ends a session only on `Unauthenticated`, and
treats any other refresh error as temporary (`../app/src/network/transport.ts`).
With `FailedPrecondition`, every tab, old or new, would stay signed in with every
request failing.

If SSO breaks after Require SSO is on, for example when the provider's client
secret expires, nobody on the domain can sign in to fix it, admins included. The
way back is `pug domains unenforce acme.com`; on cloud, that is a support request.
The "sign in once" check only guards turning Require SSO on.

Two things are not touched:

- **Passwords.** They are kept, just refused. If Require SSO is turned off, they
  work again.
- **API keys.** They belong to projects, not people, so SDKs, the API and `/mcp`
  keep working.

### Invites through SSO

An invite link is an email link, so Require SSO refuses it too. It is not used
up, and the error says it was an invite. The frontend then offers the domain's
providers and sends the link's token as `CompleteOIDCSignIn.invite_token`. The
token rides that one sign-in attempt, so a later sign-in in the same tab never
carries it. If Require SSO refuses that attempt too, for example through a
personal account, the error again says it was an invite. The frontend can then
offer the providers once more.

The server accepts the invite only if it was sent to the email of the account
the sign-in resolves to. A token for another email fails the sign-in with
`INVITATION_WRONG_EMAIL`. A token that is not a pending invite (unknown,
expired, used, revoked, or a plain sign-in link) fails it with `INVALID_TOKEN`.
Nothing is created or linked in either case: the check runs in the sign-in
transaction.

### Several orgs for one company

Nothing is copied when an admin creates another org. The new org verifies the
domain itself, with DNS or through the operator. After that, the domain is fully
part of both orgs, and each has all the settings.

When the settings differ, each one resolves like this:

| Setting | With two orgs |
|---|---|
| Auto-join | Each org decides for itself. A person can join both, with each org's role. |
| Let members create their own orgs | Off if either org turns it off. Admins of either org can still create orgs. |
| Require SSO | On if either org turns it on. |
| SSO connection (phase 3) | One per domain, across all orgs. The connection that lists the domain signs it in for every org that verified it (phase 3, rule 1). |

The strictest limit wins, so a second org can never weaken a limit the first one
set. To lift a limit, every org that set it must turn it off.

Each verification stands on its own. Removing the domain from one org leaves it
verified in the other.

### Re-checking the record

Pug checks an org's TXT record again whenever that org turns on something that
acts on other people:

| Change | Records re-checked |
|---|---|
| Auto-join turned on, or raised from Viewer to Member | Every domain the org verified through DNS |
| "Let members create their own orgs" turned off | Every domain the org verified through DNS |
| Require SSO turned on (phase 2) | That domain |
| An SSO connection gets a new domain or a new issuer (phase 3) | The connection's domains |

If a record is gone, the change is refused with `DOMAIN_VERIFICATION_FAILED`,
naming the domain. If DNS can't be reached, it is refused with
`DOMAIN_LOOKUP_FAILED`, and the admin can try again. Nothing else changes.
Domains verified by the operator skip the check.

Without this, a claim would be checked only once. Anyone who once had DNS
access, such as an agency or a former employee, could come back years later.
They could turn on limits no other org can lift, pull in the company's people
through auto-join, or attach the domain's only SSO connection and sign in as
anyone on it. A limit turned on while the record was there stays until the
operator runs `pug domains release`.

## Company SSO connections (phase 3)

Phases 1 and 2 cover Google Workspace everywhere, and company providers that a
self-hosted operator lists in `PUG_CONFIG_FILE`. Phase 3 lets an org admin
connect the company's own identity provider from the UI, through OIDC. It works
on cloud and self-hosted, and needs no operator.

An org can have several connections, each for its own domains. So a company that
buys another can keep both identity providers while it merges them.

OIDC covers Okta, Microsoft Entra ID, Keycloak, OneLogin and most others. SAML
stays out of scope: those providers all speak OIDC too, and OIDC is simpler to
run.

### Admin: set up a connection

In the SSO & domains tab, below the domains. It needs a verified domain first.
The admin picks which of the org's verified domains the connection signs in. A
domain can have only one connection (rule 1). An org can have several, such as
Okta for `acme.com` and Entra ID for `globex.com` after Acme buys Globex.

```
SSO connections                                   [+ Add connection]

  Button label    [ Acme SSO                         ]
  Issuer URL      [ https://acme.okta.com            ]
  Client ID       [ 0oa1b2c3d4…                      ]
  Client secret   [ ••••••••••••                     ]
  Domains         [x] acme.com   [ ] globex.com

  Add this redirect URL in your identity provider:
    https://app.pug.sh/oauth/callback              [copy]

                                                  [ Save ]
```

On save, Pug fetches the issuer's discovery document, so a wrong issuer fails
right away. A new domain or a new issuer also re-checks the TXT records (see
"Re-checking the record"). The provider must send the `email` claim. It need
not send `email_verified` for the domains the connection lists, which is what
lets Entra ID work (see "Providers that don't send `email_verified`"). A domain
that another connection already signs in, in this org or another, is shown but
can't be picked.

### People: email-first sign-in

The sign-in page keeps its buttons. What changes is the email box. When someone
types an email and presses Continue, the page first asks the server what that
email's domain uses:

```
  Sign in to Pug

  Email   [ bob@acme.com          ]

  [ Continue with Acme SSO ]

  Email me a link instead           ← hidden when Require SSO is on
```

If the domain has no connection and no Require SSO, Continue sends the email
link, as today. When Require SSO is on, the page goes straight to SSO instead of
waiting for the phase 2 `SSO_REQUIRED` error. That error stays as the server's
backstop.

### Rules for connections

1. **A connection only speaks for the domains it lists, and a domain has at most
   one connection.** The admin picks the domains from the org's verified ones. A
   sign-in through Acme's connection with `bob@acme.com` proves `acme.com`,
   because the connection lists it. The same connection signing in
   `carol@globex.com` or `jane@gmail.com` is refused, even though Acme verified
   `globex.com` too. A unique index allows one connection per domain across all
   orgs, so a second org that verified `acme.com` can't attach its own. Without
   this rule, any org admin could set up a provider that claims someone else's
   address and take over their Pug account. And the admin of any org that
   verified the domain could sign in as anyone on it, including the other orgs'
   admins.

   Pug checks two emails: the token's, and the email of the account the sign-in
   resolves to. Both must be on a domain the connection lists. The second check
   matters because sign-in looks up `sub` before email. Say Acme's connection
   lists `acme.com` and `globex.com`, and Acme later sells Globex and takes
   `globex.com` off. Without the second check, Acme's IdP admin could still sign
   in to a Globex account with that account's linked `sub` and any `@acme.com`
   email.
2. Identities from a connection are keyed `conn:<connection id>` plus the
   provider's `sub`. Provider ids in `PUG_CONFIG_FILE` can't contain `:`, so the
   two can never collide.
3. Changing the issuer keeps the connection id, and deletes the connection's
   linked identities in the same transaction. A `sub` is unique only within one
   issuer, and sign-in looks up `sub` before email. So a new issuer that reuses
   an old `sub` value, as usernames or employee numbers do, would put that
   person in someone else's account. People link again by email at their next
   sign-in, which rule 1 allows because the connection lists the domain.
4. A domain's connection can't go away while any org requires SSO for that
   domain. Deleting the connection, taking the domain off it, or removing the
   domain from the connection's org is refused until those admins turn Require
   SSO off.
5. Pug fetches the issuer's discovery document and signing keys from the server.
   On cloud it refuses private, loopback and cloud metadata addresses, so an
   admin can't point a connection at `169.254.169.254` or at services inside the
   cluster. The check is `code.dny.dev/ssrf`, set as the HTTP client's
   `net.Dialer.Control` hook. Go calls that hook after DNS resolution and right
   before each connection, so it checks the real IP of every fetch: discovery,
   the signing keys, the token endpoint the discovery document names, and any
   redirect. It also holds when DNS changes after save. The blocked ranges follow
   IANA's special-purpose address registries. `internal/httpx` has no such guard
   today.

   The client has three more settings:
   - Any port (`ssrf.WithAnyPort()`). The library allows only 80 and 443 by
     default, and company providers often use 8443.
   - No proxy. Behind a proxy, the hook would check the proxy's address, not the
     issuer's.
   - A 1 MiB cap on every response. go-oidc reads discovery documents and
     signing keys whole, so without a cap an admin could point a connection at
     a server that streams data until the timeout and run Pug out of memory.

   A self-hosted operator whose provider sits on an internal network sets
   `PUG_SSO_ALLOW_PRIVATE_ISSUERS=true`. So does one whose server reaches the
   internet only through a proxy. Connections then skip the address check and
   use the environment's proxy, but keep the size cap.
6. Client secrets are encrypted at rest with the AES-GCM helper that org email
   providers already use (`internal/core/email/secret`), under a new key,
   `PUG_SSO_SECRET_KEY`. With the key empty, phase 3 is off.
7. A connection can't use Google's issuer (`https://accounts.google.com`).
   Saving one is refused. Google needs no connection, because Pug's own Google
   client already reads `hd`. And a Google connection would be a hole: it would
   prove its domains for any Google account with a matching email, including a
   personal account without `hd`. This matches `PUG_CONFIG_FILE`, where
   `emailDomains` is refused on Google's issuer.

### Backend

| Piece | Change |
|---|---|
| Table `org_sso_connections` | `id`, `org_id`, `label`, `issuer_url`, `client_id`, `client_secret_ciphertext`, times. Unique on `(org_id, id)`, for the foreign key below. |
| Column `org_domains.sso_connection_id` | The connection that signs the domain in, or null. See the SQL below. |
| `DiscoverSignIn(email)` | New public RPC. Returns the providers that can sign in the email's domain, the same list the `SSO_REQUIRED` details carry: its connection (id, label, issuer, client id, scopes), a `PUG_CONFIG_FILE` provider that lists it, or Google. Also returns whether Require SSO is on. It reveals only facts about the domain, never whether an account exists. |
| `CompleteOIDCSignIn` | New optional `connection_id`. Exactly one of `provider_id` and `connection_id` is set. |
| `coreoauth` | Reads the connection row and its domains at every sign-in, so an edit takes effect on every server at once. Only the discovery result is cached, keyed by connection id, issuer and client id. `ProvenDomain()` is the email's domain when the connection lists it. Any other email is refused, and so is a resolved account whose email is not on a listed domain (rule 1). An email on a listed domain needs no `email_verified`. |
| Connection HTTP client | `net.Dialer{Control: ssrf.New(ssrf.WithAnyPort()).Safe}`, no proxy, and a 1 MiB cap on every response (rule 5). Passed to `newOIDCProvider` for connection providers only; go-oidc keeps it for later key fetches. Config providers keep `DefaultHTTPClient`. One new dependency: `code.dny.dev/ssrf`. |
| `OrgsService` | `ListSSOConnections`, `SetSSOConnection` (creates or updates one, with its domains), `DeleteSSOConnection`. Admin-only, on `ResourceDomain`. |
| Frontend | The connection cards with their domain picker, the email-first step, and keeping the discovered connection in session storage across the redirect, the way `src/auth/oidc.ts` keeps the provider id today. |

Which connection signs a domain in is a column on `org_domains`. The foreign key
keeps it inside the org. The partial unique index allows one connection per
domain across all orgs.

```sql
alter table org_domains
  add column sso_connection_id char(20) null,
  add constraint org_domains_sso_connection_fkey
    foreign key (org_id, sso_connection_id)
    references org_sso_connections (org_id, id)
    on delete set null (sso_connection_id),
  add constraint org_domains_sso_connection_needs_verified
    check (sso_connection_id is null or verified_at is not null);

create unique index org_domains_sso_connection_domain_key
  on org_domains (domain) where sso_connection_id is not null;
```

## Backend changes (phases 1 and 2)

### Schema

One migration, built as `021`. Renumber it at merge time, because open branches
already claim `021`.

```sql
create table org_domains (
  create_time timestamptz not null default now(),
  -- Lowercase, no trailing dot.
  domain varchar(253) not null
    constraint org_domains_domain_check check (domain = lower(domain)),
  id char(20) primary key,
  org_id char(20) not null references orgs(id) on delete cascade,
  require_sso boolean not null default false,
  -- First SSO sign-in after this claim was verified.
  sso_seen_at timestamptz,
  update_time timestamptz not null default now(),
  -- 'dns' or 'operator'.
  verification_method varchar(10),
  verification_token varchar(64) not null,
  verified_at timestamptz,
  constraint org_domains_org_domain_key unique (org_id, domain)
);

-- Several orgs can verify one domain. Sign-in looks up all of them.
create index org_domains_verified_domain_idx
  on org_domains (domain) where verified_at is not null;

create trigger update_timestamp before
update on org_domains for each row execute procedure moddatetime(update_time);

-- The two org settings.
alter table orgs
  -- NULL is auto-join off, the default.
  add column auto_join_role varchar(30)
    constraint orgs_auto_join_role_check check (auto_join_role <> 'ORG_ROLE_ADMIN'),
  add column members_can_create_orgs boolean not null default true;

alter table org_members add column joined_via_domain varchar(253);

-- The domain the session's SSO sign-in proved. Copied on every rotation.
alter table refresh_tokens add column proven_domain varchar(253);
```

Everything is new, nullable, or has today's behavior as its default. There is no
backfill: every existing org starts with auto-join off and org creation allowed,
and every existing session carries no domain. The Down drops it all.

The phase 2 columns ship now, so phases 1 and 2 need only one migration. Phase 1
already fills `sso_seen_at` and `proven_domain`, so the phase 2 switch is usable
on day one. Phase 3 adds its table and `org_domains.sso_connection_id` in a later
migration.

### Which sign-ins prove a domain

`Identity` gets one new accessor, `ProvenDomain()`. It is set in
`internal/core/auth/oauth/oidc.go`, where `verifyIDToken` knows both the token
and the provider's config:

| Provider | `ProvenDomain()` |
|---|---|
| Configured `issuerUrl` is `https://accounts.google.com` | The `hd` claim, lowercased. Empty for a personal account. |
| `emailDomains` lists the email's domain | The email's domain |
| Anything else | Empty |

The Google row reads the provider's config, not the token's `iss`. go-oidc also
accepts `iss: accounts.google.com` from Google, so a check on the token would
silently drop `hd` for those tokens.

Everything after this point uses only `ProvenDomain()`. It never needs to know
whether the proof came from Google, the config file or, in phase 3, a
connection.

`internal/config/config.go` gets the optional `AuthProvider.EmailDomains` field.
`Validate` lowercases the entries, checks they are hostnames, and rejects the
field on Google's issuer.

`verifyIDToken` also stops requiring `email_verified` where something else
vouches for the email (see "Providers that don't send `email_verified`"). It
decodes the claim as optional, to tell absent from `false`, and reads `xms_edov`
when it is absent.

### Domain code in `internal/core/orgs`

The domain code lives in the existing orgs package. A separate package would
create an import cycle: `orgs` calls the org creation check, and the domain code
needs `orgs.Role`. The admin RPCs and the CLI get
new methods on the existing `Service`. Sign-in gets in-transaction helpers with
the same shape as `ApplyInviteAcceptanceInTx`.

| Function | Called from |
|---|---|
| `ListDomains`, `AddDomain`, `VerifyDomain`, `RemoveDomain`, `SetDomainSettings`, `UpdateDomain` (phase 2) | The new org RPCs |
| `VerifyDomainByOperator`, `DomainClaims`, `ReleaseDomain`, `UnenforceDomain` (phase 2) | `pug domains` |
| `AutoJoinInTx(w, customerID, email, provenDomain)` → joined org ids | SSO sign-in and session refresh |
| `MarkSSOSeenInTx(w, provenDomain)` | SSO sign-in only: a refresh can carry proof from months ago. Marks verified claims only, so a pending claim can't learn that people sign in through SSO. |
| `OrgCreationAllowedInTx(q, customerID, email)` | `FinishSignup` and `OrgsService.Create`. `OrgsService.List` calls it through `OrgCreationAllowed`. |
| `CheckSignInInTx(q, email, provenDomain)` → `*SSORequiredError` | Every sign-in path but `DemoSignIn`, plus `RefreshSession` and `SetPassword` (phase 2) |

`VerifyDomain` looks up the TXT record, with a 5-second timeout. The name ends in
a dot, so the resolver never tries the server's search domains first. Setting
changes reuse it for the re-check. The DNS resolver is an interface, so tests can fake
it. `AddDomain` returns the existing row if the org already has that domain.

Limits: 10 domains per org, though `pug domains verify` doesn't check it. Exact
match only, so `eng.acme.com` is a separate domain. ASCII domains only, for now.

Auto-join is one SQL statement. It finds every org that verified the domain,
checks each org's setting and pending invites, and inserts. `on conflict do
nothing` skips the orgs the person is already in:

```sql
insert into org_members (org_id, customer_id, role, joined_via_domain)
select d.org_id, @customer_id, o.auto_join_role, d.domain
from org_domains d
join orgs o on o.id = d.org_id
where d.domain = @domain
  and d.verified_at is not null
  and o.auto_join_role is not null
  and not exists (
    select 1 from org_invitations i
    where i.org_id = d.org_id and lower(i.email) = lower(@email)
      and i.status = 'INVITATION_STATUS_PENDING' and i.expires_at > now()
  )
on conflict (org_id, customer_id) do nothing
returning org_id;
```

### Code changes

| Where | Change |
|---|---|
| `auth.FinishSignup` | Takes the proven domain. Runs the invite, then auto-join. Creates the default org only for a new account that joined nothing and may create orgs. Returns the joined org ids. |
| `auth.completeExternalIdentity` | Passes `ident.ProvenDomain()` to `FinishSignup` and to the new session. Calls `MarkSSOSeenInTx`. Phase 2: calls `CheckSignInInTx` for the token's email, and again for the resolved account's email. |
| `auth.issueSessionTx`, `auth.createRefreshToken` | Store the proven domain on the refresh token. |
| `auth.RefreshSession` | Copies the proven domain to the next token. After the refresh commits, runs auto-join for it through `FinishSignup`, and only logs a failure. Phase 2: calls `CheckSignInInTx` inside the refresh, and returns a refusal as `Unauthenticated`. |
| `auth.CompleteMagicLink` | Uses the new `FinishSignup` with no proven domain, so no auto-join. Phase 2: checks first. |
| `auth.SignInWithEmail`, `auth.RequestMagicLink` | Phase 2: check first. |
| `customers.SetPassword` | Phase 2: check first. |
| `orgs.CreateOrgWithDefaults` (the `Create` RPC) | Checks `OrgCreationAllowedInTx` first. The check lives in core, not the handler. |
| `orgs.ApplyInviteAcceptanceInTx` | The member insert becomes `on conflict do nothing`. Before, an existing member's unique violation aborted the caller's transaction, and accepting failed with Internal. |

Phase 2 adds an optional `invite_token` to `CompleteOIDCSignIn`. The server
accepts the invite only if its email matches the email of the account the
sign-in resolves to. This is how an invite works when email links are blocked
(see "Invites through SSO").

### RPCs

New RPCs on `dashboard.orgs.v1.OrgsService`:

| RPC | Does |
|---|---|
| `ListDomains(org_id)` | The two org settings and the domains, with status, TXT record and, from phase 2, Require SSO. It also says when another org makes a limit stricter, but only on a domain this org verified: a pending claim learns nothing about other orgs. |
| `SetDomainSettings(org_id, auto_join_role, members_can_create_orgs)` | Sets both org settings. `ORG_ROLE_UNSPECIFIED` turns auto-join off. |
| `AddDomain(org_id, domain)` | Creates a pending domain and its token |
| `VerifyDomain(org_id, domain_id)` | Checks DNS now for a pending domain. A verified one is returned as is. |
| `UpdateDomain(org_id, domain_id, require_sso)` | Phase 2. Turns Require SSO on or off for this org. Turning it on needs `verified_at` and `sso_seen_at`, checked in the update's `where`. |
| `RemoveDomain(org_id, domain_id)` | Deletes it from this org. Current members stay. |

New fields:

| Message | Field |
|---|---|
| `OrgsService.ListResponse` | `can_create_org`, so the frontend can hide "New organization" |
| `OrgMember` | `joined_via_domain` |
| `CompleteOIDCSignInResponse`, `CompleteMagicLinkResponse` | `joined_org_ids`. The frontend switches to the first one and shows the toast. |
| `CompleteOIDCSignInRequest` | `invite_token` (phase 2) |
| `OrgDomain` | `require_sso`, `sso_seen`, and `sso_required_elsewhere` (phase 2). The last is set only by `ListDomains`, and only on a verified domain. |
| `public.auth.v1.SSORequired` (new) | Phase 2. The detail on `SSO_REQUIRED`: the domain, the providers that can sign it in (as `AuthProviderConfig`), and `invite` when the refused sign-in carried an invite, as a link or an `invite_token`. |

### Authz

A new resource, `authz.ResourceDomain`. Admins get `manage`. It is not on the
viewer floor, so members and viewers cannot see or change these settings. Five
`OrgGated` entries go in `authz_registry.go` in phase 1, one (`UpdateDomain`) in
phase 2, and three more in phase 3.

`OrgsService.Create` keeps its `Self` entry. The org creation setting is a rule
about the caller's email, not a role, so it lives in core next to the create.

The role cache needs no change. It caches only positive results, and auto-join
only adds members.

### Errors

| Reason | Code | When |
|---|---|---|
| `SSO_REQUIRED` (phase 2) | FailedPrecondition, or Unauthenticated from `RefreshSession` | Require SSO blocks this sign-in. An `SSORequired` detail carries the domain and the providers that can sign it in. `SetPassword` sends only the message. |
| `INVITATION_WRONG_EMAIL` (phase 2) | PermissionDenied | An invite sent through SSO sign-in was for another email. |
| `ORG_CREATION_RESTRICTED` | PermissionDenied | "Let members create their own orgs" is off for the caller's domain. |
| `DOMAIN_INVALID` | InvalidArgument | The domain is not an ASCII hostname with at least two labels. |
| `DOMAIN_NOT_FOUND` | NotFound | The domain id is not one of this org's domains. |
| `DOMAIN_VERIFICATION_FAILED` | FailedPrecondition | No TXT record holds the org's value, when verifying a domain or when a setting re-checks it. |
| `DOMAIN_LOOKUP_FAILED` | Unavailable | DNS could not be reached (a timeout or a server failure) during that check. |
| `DOMAIN_NOT_VERIFIED` | FailedPrecondition | Auto-join turned on or raised to Member, or org creation turned off, while the org has no verified domain. Phase 2: Require SSO turned on for a pending domain. Going back to a default always works. |
| `DOMAIN_SSO_NOT_SEEN` (phase 2) | FailedPrecondition | Require SSO turned on before any SSO sign-in for the domain. |
| `DOMAIN_LIMIT_REACHED` | FailedPrecondition | More than 10 domains. |

### Frontend (`../app`)

| File | Change |
|---|---|
| `src/auth/oidc.ts` | Phase 2: `startOIDCSignIn` takes an optional `loginHint`, the domain (sent as `hd` to Google only) and an invite token for that one attempt. |
| `src/pages/oauth-callback.tsx`, `src/pages/magic-link.tsx` | Use `joined_org_ids`: switch org, show the toast. Show `SSO_REQUIRED` errors. |
| `src/App.tsx`, `src/pages/select-org.tsx` | Zero orgs is now a normal state, not an error. Today it shows "No organizations available for this account." It becomes the org picker's empty state: "You're not in an org yet. Ask an admin to invite you.", with the create button where org creation is allowed. |
| `src/pages/select-org.tsx`, `src/pages/routegen/settings/organization/index.page.tsx` | Hide "create organization" when `can_create_org` is false. |
| `src/pages/sign-in.tsx` | Phase 2: on `SSO_REQUIRED`, show the blocked screen with the listed providers. |
| `src/network/transport.ts` | Phase 2: a refused refresh that carries `SSO_REQUIRED` shows the blocked screen instead of "Session expired". |
| `src/pages/magic-link.tsx` | Phase 2: invite + `SSO_REQUIRED` → keep the invite token, go through SSO. |
| `src/pages/routegen/settings/sso/` (new), `settings-layout.tsx` | The SSO & domains tab: the two org settings, the domain list and the stricter-elsewhere notes. |
| `src/pages/routegen/members/index.page.tsx` | The `via acme.com` badge and the remove-dialog line. |
| `src/pages/routegen/settings/account/` | Phase 2: no change. The existing error toast shows the server's `SSO_REQUIRED` message on Set password. |

## Phases

**Phase 1: domains and org settings.** It starts with the Email Verified test
(see "Why Google needs no per-org setup"). Then the migration, `ProvenDomain()`
(Google `hd` and `emailDomains`), the `email_verified` fallback, DNS
verification and its re-check, `pug domains verify`/`show`/`release`, the
auto-join and org creation settings, auto-join on sign-in and on session
refresh, no default org for people who join, the invite acceptance fix, the
domain RPCs, the SSO & domains tab, the zero-org screen, the badge and the toast. It also starts filling `sso_seen_at` and `proven_domain`.
Nothing blocks a sign-in yet.

**Phase 2: Require SSO.** The sign-in and refresh checks, invites through SSO,
`pug domains unenforce`, and the frontend handling.

**Phase 3: company SSO connections.** The connection table, the domain column
and the RPCs, `DiscoverSignIn`, email-first sign-in, `connection_id` on
`CompleteOIDCSignIn`, and the connection HTTP client with its address guard and
size cap.

All phases are additive, and old clients keep working. The new errors can only
appear after an admin changes a setting from the new frontend. An old client
whose refresh is refused just signs out.

## Tests

Integration tests use the repo's `testutil` setup; 11 to 14 are unit tests.

1. With auto-join on, a new Workspace user joins and has no default org.
2. With auto-join off (the default), an SSO sign-in adds nobody, and a new
   account gets a default org.
3. Turning auto-join off keeps the people who already joined.
4. With org creation off, an `@D` account cannot create an org, and a new `@D`
   account gets no default org. An admin of an org that verified D still can
   create one. A `gmail.com` member still can too.
5. Two orgs verify one domain: auto-join adds a person to both, each with its own
   role.
6. Strictest wins: org creation off in either org blocks it, and (phase 2)
   Require SSO on in either org requires it.
7. A pending invite beats auto-join. Someone who signs in through SSO before
   using an Admin invite is not auto-joined to that org, and the invite then
   makes them Admin.
8. Accepting an invite inside the sign-in transaction, when the person is
   already a member, marks the invite accepted and keeps their role.
9. An SSO session joins a new auto-join org at its next refresh. A password or
   email-link session does not. A failing auto-join does not fail the refresh.
10. A personal Google account with an `acme.com` email does not auto-join.
    Neither does an account on another domain whose sign-in proved `acme.com`.
11. `hd` from a non-Google issuer is ignored. A Google token whose `iss` is
    `accounts.google.com` still proves its `hd`.
12. A provider with `emailDomains` proves its domain. The same provider without
    it does not.
13. Config load rejects `emailDomains` on Google's issuer.
14. A token with no `email_verified` signs in when its email's domain is in the
    provider's `emailDomains`, or when it carries `xms_edov: true`. Without
    either it is refused, and so is any token with `email_verified: false`.
15. `pug domains verify` marks a domain verified without DNS.
16. Turning on auto-join or raising it to Member, and turning off org creation,
    re-check the TXT record of every domain the org verified through DNS, and
    are refused when one is gone. So do turning on Require SSO (phase 2) and
    giving a connection a domain or issuer (phase 3). A domain verified by the
    operator skips the check.
17. Two concurrent first sign-ins of one person: one account, one membership per
    org.
18. Phase 2: every blocked path returns `SSO_REQUIRED`. A blocked email link is
    not used up. A session that did not start through SSO is refused at refresh
    with `Unauthenticated` and revoked, and a replayed token still trips reuse
    detection first; an SSO session is renewed. The demo sign-in and demo
    sessions at refresh keep working. `pug domains unenforce` turns Require SSO
    off in every org.
19. Phase 2: an invite accepted through SSO is refused when the account the
    sign-in resolves to has a different email.
20. Phase 2: an identity that resolves by `sub` to an `@D` account gets
    `SSO_REQUIRED`, even when its provider email is no longer on D.
21. Phase 3: a connection signs in only emails on the domains it lists, and
    needs no `email_verified` for them. Any other email is refused, even on
    another of the org's verified domains, and even when the provider says it is
    verified.
22. Phase 3: after a domain is taken off a connection, a token with an account's
    linked `sub` and an email on another listed domain does not reach that
    account.
23. Phase 3: a second connection for a domain is refused, from the same org or
    from another org that verified it. One org can have two connections for two
    domains.
24. Phase 3: after an issuer change, a `sub` the old issuer also used does not
    reach the old account. The person links by email instead.
25. Phase 3: a changed issuer or secret takes effect at the next sign-in on
    every server, with no restart.
26. Phase 3: on cloud, an issuer on a private, loopback or metadata address is
    refused. So is a public issuer whose discovery document points its signing
    keys or token endpoint at one, or that redirects to one. An issuer on port
    8443 works, and a discovery document over 1 MiB is refused.
27. Phase 3: a connection with Google's issuer is refused on save.

## Security notes

1. A Google account without `hd` is never proof of a domain. That is also why
   Google can't be added as a company SSO connection (phase 3, rule 7).
2. `hd` is read only from Google's issuer. The `hd` URL parameter is a UI hint;
   the server checks the claim.
3. `emailDomains` is the operator's word. List it only on a provider the company
   runs.
4. A company SSO connection is set up by an org admin, so Pug trusts it only for
   the verified domains it lists, and only one connection speaks for a domain
   (phase 3, rule 1). It checks the account a sign-in resolves to, not just the
   token's email.
5. Every org proves the domain itself, through DNS or through the operator. A
   random org can never claim a company's domain, and no org borrows another
   org's proof. An org proves it again whenever it turns on a setting that acts
   on other people, so DNS access someone has since lost can't be used later.
6. Nobody can claim `gmail.com` or `outlook.com` through DNS, because nobody can
   add a TXT record there. So there is no need for a blocklist of email
   providers.
7. When orgs disagree on a limit, the strictest wins. A second org can't weaken
   the first org's limits.
8. Auto-join is off until an admin turns it on, and it never grants Admin.
9. **Removal doesn't stick yet.** While auto-join is on, a removed member is
   added back at their next session refresh or SSO sign-in (see "Removal
   doesn't stick yet").
10. **Offboarding gap.** When Acme suspends Bob's account at its identity
    provider, Bob cannot sign in again. But his current Pug session keeps
    working, because refresh tokens slide for 90 days with use. That session can
    also auto-join orgs that turn auto-join on later. Admins should still remove
    people who leave. While auto-join is on, that doesn't hold yet (note 9).
    Open question 2 closes most of this; SCIM would close it fully. The same holds when an operator takes a domain off a provider's
    `emailDomains`: sessions that proved it keep proving it at each refresh until
    they lapse, and Pug has no tool to revoke them.
11. **An existing gap, found while writing this.** Today a personal Google
    account links to a Pug account by email, for any address. For a non-Gmail
    address without `hd`, Google says it is not the authority: the address may
    have changed owners since Google checked it. So a past owner of
    `bob@acme.com` could sign in to the current owner's Pug account. Require SSO
    closes this for its domains. The general fix is separate and small: link by
    email only when Google is the authority (a `@gmail.com` address, or `hd` is
    present). It would change linking only for Google, never for a company
    provider.
12. **Lapsed domains.** Someone who re-registers a dead company's domain and sets
    up Google Workspace on it gets tokens with the same `hd`. The fix in note 11
    still links those by email to the old accounts. And because Pug re-checks
    DNS only when a setting changes, they would also auto-join the old company's
    orgs. A scheduled re-check would close the auto-join half.
13. **Absent `email_verified`.** Pug accepts it only where something else
    vouches for the address: a domain the provider speaks for, or Entra's
    `xms_edov`. An explicit `false` is always refused.
14. **Email Verified Workspace accounts.** Google lets a Workspace exist before
    its domain is verified. Unconfirmed: whether such accounts carry `hd`.
    Phase 1 starts by testing it (see "Why Google needs no per-org setup").

## Not in scope

- SAML. OIDC covers the same providers and is simpler to run.
- SCIM, so a company's identity provider can deactivate people in Pug.
- Requiring SSO for every org member, not just emails on the domain.
- Re-checking DNS on a schedule. Admins are asked to keep the record, so this
  can come later. It is what would stop a lapsed domain's new owner from
  auto-joining (security note 12).
- A company level above orgs, with one place for all its orgs' settings.
  Several orgs verifying one domain covers a company with a few orgs.

## Open questions

1. **Should an email link prove a domain for auto-join?** Recommended: no, not
   in v1. Anyone who can read mail at any `acme.com` address could join, for
   example through a helpdesk inbox that shows emails to outside users. An SSO
   sign-in needs a real account that Acme's admin manages. A yes would help
   companies that have no SSO at all.
2. **Cap session age under Require SSO?** For example, `@D` accounts must sign
   in through SSO again every 7 days. That closes the offboarding gap to about a
   week. `proven_domain` already tells which sessions came from SSO, so it needs
   only a start time on the session.
3. **Which role is pre-selected when an admin turns auto-join on?**
   Recommended: Viewer, the least access.
4. **Existing users: silent join or a prompt?** Recommended: silent join plus a
   toast. They can leave, but while auto-join is on they rejoin (see "Removal
   doesn't stick yet").
5. **When orgs disagree on a limit, is "strictest wins" right?** The other
   choice is one shared set of domain settings that an admin of any of the orgs
   can change. That is simpler to show, but any of those admins could then
   weaken a limit, such as turning Require SSO off for everyone. Recommended:
   strictest wins.
