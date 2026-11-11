# Development setup

This guide takes a fresh clone to a running Pug with demo data and an admin
account you can sign in as.

## Prerequisites

| Tool | Version | Used for |
|------|---------|----------|
| Go | 1.27.4 or newer | Building `pug` |
| Docker with Compose | Any recent | Postgres, ClickHouse, NATS and Dragonfly |
| Bun | Any recent | The dashboard (optional) |

## 1. Create `.env`

```bash
cp .env.example .env
```

The defaults already match the dev containers. Set two values:

| Variable | Value |
|----------|-------|
| `PUG_JWT_SECRET_KEY` | Any long random string, such as the output of `openssl rand -hex 32`. |
| `PUG_DEMO_ADMIN_PASSWORD` | The admin account's password: 16 characters to 72 bytes, with no leading or trailing spaces. |

`pug` reads `.env` from the current directory, and the migrations read
`schema/` from it, so run every command from the repo root. A variable exported
in your shell wins over `.env`.

## 2. Start the backing services

```bash
make infra
```

| Service | Ports | Holds |
|---------|-------|-------|
| PostgreSQL | 5433 | Accounts, orgs, projects, dashboards |
| ClickHouse | 9000, 8123, 9009 | Events and profiles |
| NATS | 4222, 6222, 8222 | Queues between the server and the workers |
| Dragonfly | 6380 | Redis-compatible cache |

`make infra-down` stops them. Their data stays in Docker volumes.

## 3. Build and migrate

```bash
make build
./bin/pug migrate
```

`pug migrate` runs the Postgres, NATS and ClickHouse migrations in that order
and stops at the first one that fails. It is safe to re-run; run it again after
you pull new migrations. It includes the NATS step that creates the streams the
workers read from.

To migrate one store, use `pug postgres migrate`, `pug nats migrate` or
`pug clickhouse migrate`. To roll back the last Postgres or ClickHouse
migration, add `--direction down --num 1`. NATS has no down.

> **`--direction down` without `--num` rolls back every migration.** It drops
> every table in the database, with no prompt.

## 4. Seed demo data and the admin

```bash
./bin/pug seed
```

The seed:

1. Resets Postgres and ClickHouse by migrating them down and back up.
2. Creates the demo org and its project. Both are named `default`.
3. Writes about six months of events (500,000 by default) and the profiles
   behind them.
4. Adds the demo dashboards.
5. Creates `admin@pug.example` as the demo org's admin, with
   `PUG_DEMO_ADMIN_PASSWORD` as its password.

A default run takes a minute or two. Pass `--count 50000` for a quicker one. If
you also run the demo worker (`PUG_DEMO_ENABLED=true`), set
`PUG_DEMO_SEED_COUNT` to the same number.

> **The seed wipes both databases.** The reset drops every table in whatever
> `DATABASE_URL` and `CLICKHOUSE_URL` point at, with no prompt. Only run it
> against the local containers.

The seed checks `PUG_DEMO_ADMIN_PASSWORD` before the reset, so a bad value
stops it before anything is dropped.

These accounts exist after a seed:

| Account | Password | Access |
|---------|----------|--------|
| `admin@pug.example` | `PUG_DEMO_ADMIN_PASSWORD` | Admin of the demo org. Created only when the variable is set. |
| `woof@pug.sh` | None | Owns the demo data. Read-only. |
| `snoop@pug.sh` | None | Read-only viewer. Open `/demo` with `PUG_DEMO_ENABLED=true` to sign in as it. |

`./bin/pug seed --no-reset` re-seeds without the reset. It keeps the schema and
every other project, and replaces only the demo project's events and profiles.

## 5. Start Pug

```bash
./bin/pug dev
```

This runs the API server on `http://localhost:3000` and every worker in one
process. The banner shows whether each backing service is reachable. Press
Ctrl+C to stop.

## 6. Sign in as the admin

Run the dashboard from its own repo, cloned next to this one:

```bash
git clone https://github.com/pug-sh/app.git ../app
cd ../app
cp .env.example .env
bun install
bun run dev
```

Open `http://localhost:5173`, choose **Sign in with password**, and enter
`admin@pug.example` with your `PUG_DEMO_ADMIN_PASSWORD`. Use the password: the
`.example` domain can't receive mail, so an email link to the admin never
arrives.

To check the admin without the dashboard:

```bash
curl -s http://localhost:3000/public.auth.v1.AuthService/SignInWithEmail \
  -H 'Content-Type: application/json' \
  -d '{"email":"admin@pug.example","password":"YOUR_ADMIN_PASSWORD"}'
```

A response with a `token` means the admin works. `invalid credentials` means
the password doesn't match the account; see the next section.

## Changing the admin password

The seed creates the admin once. It never changes the password of an admin
that already exists.

| You did this | What happens | What to do |
|--------------|--------------|------------|
| Set the variable after seeding | There is no admin yet. | Run `./bin/pug seed --no-reset` to add it. |
| Changed the variable after the admin was created | The admin keeps the old password. | Sign in with the old one, or run `./bin/pug seed` to recreate the admin. |
| Changed the password in the dashboard | Later seeds log a warning and leave the account alone. | Set the variable to the new password, which must be 16 characters or more. |

While the variable matches the admin's password, every seed also gives the
account its admin role back if someone removed or demoted it.

## Troubleshooting

| Symptom | Fix |
|---------|-----|
| `PUG_DEMO_ADMIN_PASSWORD must be at least 16 characters and at most 72 bytes` | Pick a password in that range. Nothing was reset. |
| `PUG_DEMO_ADMIN_PASSWORD has leading or trailing whitespace` | Remove the spaces or the trailing newline. |
| The seed logs `PUG_DEMO_ADMIN_PASSWORD is not set` | Set it, then run `./bin/pug seed --no-reset`. |
| Sign-in fails with a Turnstile error | Leave `PUG_TURNSTILE_SITE_KEY` and `PUG_TURNSTILE_SECRET_KEY` empty, or use the test keys listed in `.env.example`. |
| A migration can't connect | Run `make infra`, then wait until `docker compose -f infra/dev/docker-compose.yaml ps` shows every service healthy. |
| A value in `.env` seems ignored | A variable exported in your shell wins. Check `env \| grep PUG_`. |
| The dashboard can't reach the API | Keep `PUG_CORS_ORIGINS=http://localhost:5173` in this repo's `.env` and `VITE_API_BASE_URL=http://localhost:3000` in the app's. |
