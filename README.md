<p align="center"><img src="assets/logo.png" width="360"></p>

# Happy Pods

Deploy a folder, get a URL. Plus a tiny Firebase-style SQLite document store.

Happy Pods is a self-hostable clone of Shopify's internal **Quick** platform, lovingly
inspired by their write-up: [How We Built Quick, Shopify's Internal Deployment Tool](https://shopify.engineering/quick).
Two small Go binaries with boring dependencies (SQLite and a cookie signer):

- **`pods`** — the CLI you run on your machine
- **`podbay`** — the server ("open the pod bay doors")

A public instance runs at **https://podbay.dev** — log in with GitHub and deploy.

## Why

Sometimes you just want to put a folder of HTML on the internet (or intranet) without a
build pipeline, a YAML sacrifice, or a cloud bill. Happy Pods gives you:

- **One-command deploys** — `pods deploy` tars up a folder and ships it.
- **Instant URLs** — every site gets a subdomain: `<name>.<base>`, e.g.
  `https://hello.podbay.dev`.
- **A wee JSON store** — one SQLite database per site, with create/get/set/patch/delete
  and simple queries, usable from the CLI or straight from the browser via `/pods.js`.
- **GitHub login** — `pods login` runs the GitHub device flow and hands you one
  refreshable API token. No password column anywhere.
- **Boring tech** — one static binary each, files on disk, SQLite for the data.

The Quick-style tradeoff, stated honestly: **everything is public.** Deployed sites, the
landing page, and every site's JSON store are open — reads *and* store writes — to
everyone who can reach the server. Authentication only gates publishing: each site is
owned by the account that first deployed it, and only that owner (or an admin) can
redeploy or delete it. Don't put secrets in the store; HAL is watching, and so is
everyone else.

## Quickstart

### Use the hosted instance (podbay.dev)

Install the CLI (`go install github.com/slopus/pods/cmd/pods@latest`, or clone this repo
and `make build` to get `bin/pods`), then:

```sh
pods login --endpoint https://podbay.dev   # 1. GitHub device flow: opens github.com/login/device
pods init hello                            # 2. scaffold pods.json + index.html
pods deploy hello                          # 3. ship it
pods open hello                            # 4. https://hello.podbay.dev
```

### Self-host with Docker

```sh
make docker                                                # 1. build the image
docker run -d --name podbay -p 7777:7777 \
  -e PODBAY_PUBLIC_URL=http://localhost:7777 \
  -v podbay-data:/data podbay                              # 2. run the server
docker logs podbay                                         # 3. copy the "generated secret: ..." line
pods login --endpoint http://localhost:7777 --token <paste-secret>  # 4. log in as the bootstrap admin
pods init hello && pods deploy hello                       # 5. deploy http://hello.localhost:7777
```

Prefer compose? `PODBAY_SECRET=$(openssl rand -hex 16) docker compose up -d`.
To enable `pods login` via GitHub on your own server, see
[Self-hosting notes](#self-hosting-notes).

## CLI reference

Configuration resolution, highest wins: flags `--endpoint`/`--token` (or the deprecated
alias `--secret`) → env `PODS_ENDPOINT`/`PODS_TOKEN`/`PODS_SECRET` (`PODS_TOKEN` beats
`PODS_SECRET`) → `~/.config/pods/config.json` (written by `pods login`, mode 0600).

| Command | What it does |
|---|---|
| `pods login [--endpoint URL] [--token T]` | Without `--token`: GitHub device flow — prints a code, opens `github.com/login/device` (auto-opens on macOS), polls until you approve, and saves a 30-day API token. With `--token`: verifies the token against the server and saves it. Prompts for the endpoint if the flag is omitted. |
| `pods logout` | Delete the saved config file. |
| `pods status` | Show endpoint, health check result, current user, site count, and collections if the endpoint is a site subdomain. |
| `pods init [dir]` | Scaffold a starter site (`pods.json` + a friendly `index.html`). Refuses to overwrite existing files. |
| `pods deploy [dir] [--name N]` | Tar.gz the folder and deploy. Name: flag > `pods.json` > dir basename. Prints the subdomain URL. |
| `pods list` | Table of sites: NAME, OWNER, FILES, SIZE, UPDATED. |
| `pods rm <site> [--yes]` | Delete a site (confirms unless `--yes`). |
| `pods open <site>` | Print the `<site>.<base>` URL (and open it in your browser on macOS). |
| `pods db <coll> list [--where k=v]... [--sort f] [--limit n] [--offset n] [--json]` | Query a collection; prints docs as JSON lines, or the full result with `--json`. |
| `pods db <coll> get <id>` | Pretty-print one document. |
| `pods db <coll> create <json\|->` | Create a document (`-` reads stdin); prints the created doc. |
| `pods db <coll> set <id> <json\|->` | Replace (upsert) a document. |
| `pods db <coll> patch <id> <json\|->` | Shallow-merge into an existing document. |
| `pods db <coll> rm <id>` | Delete a document. |
| `pods db <coll> drop [--yes]` | Drop a whole collection (confirms unless `--yes`). |
| `pods version` | Print the CLI version. |
| `pods help` | Usage with all commands. |

Flags may follow positionals: `pods deploy hello --name web` and `pods rm hello --yes`
both work. Exit codes: 0 on success, 1 on error. Errors go to stderr prefixed `pods: `;
URLs and `--json` output go to stdout so you can pipe them.

**Token refresh is automatic.** The API token is a single 30-day JWT. When fewer than 7
days remain, the CLI transparently refreshes it (`POST /api/auth/refresh`) before running
your command and rewrites the config file. Tokens supplied via flags or env are used
as-is and never written back.

For `pods db ...`, point `PODS_ENDPOINT` or `pods login --endpoint` at the site subdomain
you want to operate on, such as `https://hello.podbay.dev`. The path stays `/api/db`; the
host supplies the site scope. The DB API only answers on a deployed site's subdomain —
unknown sites get 404.

## Server reference

`podbay` configuration (flag > env > default):

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `--addr` | `PODBAY_ADDR` | `:7777` | listen address |
| `--data` | `PODBAY_DATA` | `./data` | data directory |
| `--secret` | `PODBAY_SECRET` | (generated) | bootstrap admin bearer token |
| `--auth` | `PODBAY_AUTH_FILE` | `<data>/auth.json` | auth config JSON file |
| `--public-url` | `PODBAY_PUBLIC_URL` | (derived from request) | base URL for site subdomains and printed URLs |
| `--cookie-domain` | `PODBAY_COOKIE_DOMAIN` | (from auth.json) | session cookie domain, e.g. `.podbay.dev` |
| `--github-client-id` | `PODBAY_GITHUB_CLIENT_ID` | (from auth.json) | GitHub OAuth app client id |
| `--github-client-secret` | `PODBAY_GITHUB_CLIENT_SECRET` | (from auth.json) | GitHub OAuth client secret (needed for browser login) |
| `--github-redirect-url` | `PODBAY_GITHUB_REDIRECT_URL` | (derived from request) | fixed GitHub OAuth callback URL |

If no bootstrap token is configured, the server generates 32 hex characters on first
boot, persists them to `<data>/secret` (mode 0600), and prints `generated secret: ...` to
stdout **once**. On first boot, podbay also creates `<data>/auth.json` with that token as
the `admin` user's bearer token.

### Auth config

Authentication is GitHub OAuth plus a small file-backed bootstrap. Browser users sign in
through `/api/auth/login/github` (session cookie); the CLI uses the GitHub device flow
and gets a JWT API token; static bearer tokens in `auth.json` remain for admin/automation.
Edit `<data>/auth.json` or pass `--auth ./auth.json`:

```json
{
  "users": [
    {
      "id": "admin",
      "name": "Admin",
      "admin": true,
      "tokens": ["random-admin-token"]
    }
  ],
  "oauth": {
    "session_secret": "long-random-session-secret",
    "cookie_domain": ".pods.example.com",
    "session_hours": 168
  },
  "github": {
    "client_id": "...",
    "client_secret": "...",
    "redirect_url": "https://pods.example.com/api/auth/callback/github",
    "allowed_users": ["alice", "bob"]
  }
}
```

- `users` are static accounts with bearer tokens; `admin: true` may manage every site.
- `oauth.session_secret` signs both session cookies **and** API-token JWTs — rotating it
  logs everyone out. Defaults to the first user token if omitted. `session_hours`
  defaults to 168 (one week).
- `github` enables GitHub login. `client_id` alone is enough for the CLI device flow;
  `client_secret` is additionally required for browser login. `allowed_users` (GitHub
  logins, case-insensitive) restricts who may sign in; empty means everyone. The
  `--github-*` flags/env vars override this block.

GitHub users live in `<data>/identity.sqlite`, keyed by a provider-agnostic
`"<provider>:<subject>"` id (e.g. `github:12345`) — GitHub is the only provider wired up
today. The same database records which user owns which site; ownership is claimed on a
site's first deploy.

### HTTP API

Errors are JSON: `{"error":"..."}` with a 4xx/5xx status.

| Method & path | Auth | Behavior |
|---|---|---|
| `GET /healthz` | no | `{"ok":true}` |
| `GET /api/me` | optional | current user profile (or `authenticated: false`), the host's site, and a `login_url` |
| `GET /api/auth/providers` | no | list configured OAuth providers and login URLs |
| `GET /api/auth/login/{provider}` | no | start browser OAuth login; accepts `return_to` |
| `GET /api/auth/callback/{provider}` | no | OAuth redirect URI; sets the signed session cookie |
| `POST /api/auth/github/device/start` | no | start the GitHub device flow; returns `device_code`, `user_code`, `verification_uri`, `interval` |
| `POST /api/auth/github/device/poll` | no | body `{"device_code":"..."}`; 202 `{"pending":true}` until approved, then the API token + user |
| `POST /api/auth/refresh` | valid API token | exchange a still-valid JWT for a fresh 30-day one; no separate refresh token exists |
| `POST /api/auth/logout` | no | clear the session cookie |
| `GET /api/events` | no | SSE stream of deploy/store updates: site-scoped on a `<site>` subdomain host, global on the base host |
| `GET /api/sites` | no | list all sites with owner, file count, size, updated time |
| `PUT /api/sites/{name}` | owner or admin | deploy; body = tar.gz. First deploy claims the site for the caller; 403 if someone else owns it |
| `DELETE /api/sites/{name}` | owner or admin | remove a site, its store, and its ownership record; 404 if absent |
| `GET /api/db` | no, site host only | list collections for the current `<site>` host |
| `GET /api/db/{coll}` | no, site host only | query documents (see below) |
| `POST /api/db/{coll}` | no, site host only | create a doc (auto `id`); 201 with the full doc |
| `GET /api/db/{coll}/{id}` | no, site host only | get a doc; 404 if absent |
| `PUT /api/db/{coll}/{id}` | no, site host only | replace (upsert; keeps `created_at` if it existed) |
| `PATCH /api/db/{coll}/{id}` | no, site host only | shallow merge into an existing doc; 404 if absent |
| `DELETE /api/db/{coll}/{id}` | no, site host only | delete a doc; 404 if absent |
| `DELETE /api/db/{coll}` | no, site host only | drop the whole collection; 404 if absent |
| `GET /sites/{site}/{path...}` | no | path fallback for static files; subdomain serving is preferred |
| `GET /` | no | landing page listing deployed sites |
| `GET /pods.js` | no | zero-dependency browser JS client |

`GET /sites/{site}` redirects to `/sites/{site}/`. `/api/db` endpoints answer only on a
deployed site's subdomain host: the base host gets 400, an unknown site gets 404 — stray
subdomains can never create database files.

Validation: site names are DNS-label style (`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`);
collection names and client-supplied doc IDs match `^[A-Za-z0-9_-]{1,64}$`. Uploads are
capped at 256 MiB and 10,000 files per site; DB request bodies at 1 MiB. Tar extraction
is zip-slip safe (no absolute paths, `..`, symlinks, hardlinks, or devices).

## JS client (`/pods.js`)

Every podbay serves a tiny browser client at `/pods.js`. On a site subdomain, same-origin
`/api/db` is automatically scoped to that site — and since the store is public, no token
is needed:

```html
<script src="/pods.js"></script>
<script type="module">
  const pods = Pods(); // endpoint defaults to same origin; { token } optional
  const me = await pods.me();
  const providers = await pods.auth.providers({ returnTo: location.href });
  const posts = pods.db.collection("posts");

  const doc = await posts.create({ title: "hi" });
  await posts.query({ where: { status: "draft" }, sort: "-created_at", limit: 10 });
  await posts.get(doc.id);
  await posts.patch(doc.id, { status: "published" });
  await posts.set(doc.id, { title: "rewritten" });
  await posts.delete(doc.id);

  const stream = pods.events((ev) => console.log(ev.type, ev)); // SSE; stream.close()
</script>
```

## The JSON store

Documents are JSON objects stored in SQLite — one database per site at
`<data>/db/<site>.sqlite`, created lazily on the first write and deleted with the site.
The server injects and maintains three reserved fields: `id` (16 hex chars),
`created_at`, and `updated_at` (RFC3339 UTC). Clients can't override them — the server
always wins. A legacy single `<data>/db.sqlite` from older versions is split into
per-site databases automatically on startup (the old file is kept as
`db.sqlite.migrated`).

### Query semantics (`GET /api/db/{coll}`)

- `where=field=value` — repeatable; conditions are ANDed. Top-level fields only.
  String fields compare as strings, numbers as float64, booleans as `true`/`false`,
  nulls as `null`; docs missing the field don't match.
- `sort=field` ascending, `sort=-field` descending. Numbers sort numerically, strings
  lexically, booleans false<true; docs missing the field sort last.
  Default sort: `created_at` ascending.
- `limit` (0 = no limit, default 0) and `offset` (default 0).
- The response's `total` counts matching docs **before** limit/offset.

```sh
pods db posts list --where status=draft --sort -created_at --limit 10
```

## Self-hosting notes

- **Secret**: set `PODBAY_SECRET` yourself, or let the server generate one
  (printed once, stored at `<data>/secret`). It becomes the default admin token in
  `<data>/auth.json` — and the default JWT/session signing secret, so treat it well.
- **GitHub login**: create a GitHub OAuth app, enable **Device Flow**, set the callback
  URL to `<public-url>/api/auth/callback/github`, then pass
  `PODBAY_GITHUB_CLIENT_ID` (and `PODBAY_GITHUB_CLIENT_SECRET` for browser login) or put
  them in `auth.json`. Without it, only static tokens from `auth.json` work.
- **Data dir**: everything lives under one directory — back it up and you're done:

  ```
  <data>/secret             # generated secret (if not configured)
  <data>/auth.json          # static users, session settings, github oauth
  <data>/sites/<name>/...   # deployed static files
  <data>/sites.json         # site deploy-time metadata
  <data>/db/<site>.sqlite   # per-site document store (created on first write)
  <data>/identity.sqlite    # OAuth users and site ownership
  ```

- **DNS / reverse proxy**: point wildcard DNS such as `*.pods.example.com` at podbay.
  Set `--public-url https://pods.example.com` so site hosts resolve and deploys print
  URLs like `hello.pods.example.com`; set `--cookie-domain .pods.example.com` so browser
  sessions work across site subdomains. Locally, `--public-url http://localhost:7777`
  makes `hello.localhost:7777` work out of the box.

## Development

Go 1.25 with exactly two direct dependencies: `modernc.org/sqlite` (pure-Go SQLite) and
`github.com/gorilla/securecookie` (signed session cookies).

| Target | What it does |
|---|---|
| `make build` | build both binaries into `bin/` (version from `git describe`) |
| `make test` | run all tests |
| `make run` | run the server locally for development |
| `make docker` | build the Docker image |
| `make clean` | remove `bin/` |

## License

MIT. Go forth and deploy folders.
