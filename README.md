<p align="center"><img src="assets/logo.png" width="360"></p>

# Happy Pods

Deploy a folder, get a URL. Plus a tiny Firebase-style SQLite document store.

Happy Pods is a self-hostable clone of Shopify's internal **Quick** platform, lovingly
inspired by their write-up: [How We Built Quick, Shopify's Internal Deployment Tool](https://shopify.engineering/quick).
Two small Go binaries with boring Go dependencies for OAuth and terminal prompts:

- **`pods`** — the CLI you run on your machine
- **`podbay`** — the server ("open the pod bay doors")

## Why

Sometimes you just want to put a folder of HTML on the internet (or intranet) without a
build pipeline, a YAML sacrifice, or a cloud bill. Happy Pods gives you:

- **One-command deploys** — `pods deploy` tars up a folder and ships it.
- **Instant URLs** — every site gets a tenant subdomain: `<name>.<team>.<base>`.
- **A wee JSON store** — collections of documents with create/get/set/patch/delete and
  simple queries, usable from the CLI or straight from the browser via `/pods.js`.
- **File-backed OAuth** — OIDC providers, session settings, machine tokens, team roles,
  and per-app auth policies live in one editable `auth.json`.
- **Boring tech** — one static binary each, files on disk, and SQLite for the DB.

The Quick-style tradeoff, stated honestly: **deployed sites and the landing page are open
by default to everyone who can reach the server.** Apps can opt into required auth in
`auth.json`. The special `public` team accepts anonymous publishes by default; other teams
and all database routes require an authenticated user with a team role.

## Quickstart

Build the image and the CLI (`make build` drops `pods` into `bin/`), then:

```sh
make docker                                                          # 1. build the image
docker run -d --name podbay -p 7777:7777 -v podbay-data:/data podbay # 2. run the server
docker logs podbay                                                   # 3. copy the "generated secret: ..." line
pods login --endpoint http://localhost:7777 --token <paste-secret>   # 4. point the CLI at it
pods init hello && pods deploy hello --team public                   # 5. deploy hello.public.localhost
pods open hello --team public                                        # 6. print the subdomain URL
```

Prefer compose? `PODBAY_SECRET=$(openssl rand -hex 16) docker compose up -d`.

## CLI reference

Configuration resolution, highest wins: flags `--endpoint`/`--token` (or legacy
`--secret`) → env `PODS_ENDPOINT`/`PODS_TOKEN`/`PODS_SECRET` →
`~/.config/pods/config.json` (written by `pods login`, mode 0600).

| Command | What it does |
|---|---|
| `pods login [--endpoint URL] [--token T]` | Verify and save credentials. Single-line when both flags are given; otherwise prompts (token hidden). Scriptable when stdin isn't a TTY. |
| `pods logout` | Delete the saved config file. |
| `pods status` | Show endpoint, health check result, site count, and collections if the endpoint is a site subdomain. |
| `pods init [dir]` | Scaffold a starter site (`pods.json` with `team: "public"` + a friendly `index.html`). Refuses to overwrite existing files. |
| `pods deploy [dir] [--name N] [--team T]` | Tar.gz the folder and deploy. Name: flag > `pods.json` > dir basename. Team: flag > `pods.json` > `public`. Prints the subdomain URL. |
| `pods list` | Table of sites: TEAM, NAME, FILES, SIZE, UPDATED. |
| `pods rm <site> [--team T] [--yes]` | Delete a site (confirms unless `--yes`). |
| `pods open <site> [--team T]` | Print the `<site>.<team>.<base>` URL (and open it in your browser on macOS). |
| `pods db <coll> list [--where k=v]... [--sort f] [--limit n] [--offset n] [--json]` | Query a collection; prints docs as JSON lines, or the full result with `--json`. |
| `pods db <coll> get <id>` | Pretty-print one document. |
| `pods db <coll> create <json\|->` | Create a document (`-` reads stdin); prints the created doc. |
| `pods db <coll> set <id> <json\|->` | Replace (upsert) a document. |
| `pods db <coll> patch <id> <json\|->` | Shallow-merge into an existing document. |
| `pods db <coll> rm <id>` | Delete a document. |
| `pods db <coll> drop [--yes]` | Drop a whole collection (confirms unless `--yes`). |
| `pods version` | Print the CLI version. |
| `pods help` | Usage with all commands. |

Exit codes: 0 on success, 1 on error. Errors go to stderr prefixed `pods: `; URLs and
`--json` output go to stdout so you can pipe them.

For `pods db ...`, set `PODS_ENDPOINT` or `pods login --endpoint` to the site subdomain
you want to operate on, such as `https://hello.public.pods.example.com`. The path stays
`/api/db`; the host supplies the tenant scope.

## Server reference

`podbay` configuration (flag > env > default):

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `--addr` | `PODBAY_ADDR` | `:7777` | listen address |
| `--data` | `PODBAY_DATA` | `./data` | data directory |
| `--secret` | `PODBAY_SECRET` | (generated) | bootstrap admin bearer token |
| `--auth` | `PODBAY_AUTH_FILE` | `<data>/auth.json` | auth config JSON file |
| `--public-url` | `PODBAY_PUBLIC_URL` | (derived from request) | base URL used when printing site URLs |

If no bootstrap token is configured, the server generates 32 hex characters on first boot,
persists them to `<data>/secret` (mode 0600), and prints `generated secret: ...` to stdout
**once**. On first boot, podbay also creates `<data>/auth.json` with that token as the
`admin` user's bearer token.

### Auth config

Authentication is intentionally small and file-backed. Browser users sign in through
OAuth/OIDC; bearer tokens remain available for CLI/admin automation. Edit
`<data>/auth.json` or pass `--auth ./auth.json`:

```json
{
  "users": [
    {
      "id": "alice",
      "name": "Alice",
      "email": "alice@example.com",
      "tokens": ["random-cli-token"],
      "oauth": ["google:optional-provider-subject"],
      "teams": {"ops": "publisher", "public": "publisher"}
    },
    {
      "id": "admin",
      "tokens": ["random-admin-token"],
      "admin": true,
      "teams": {"*": "admin"}
    }
  ],
  "teams": {
    "public": {"name": "Public", "public_publish": true},
    "ops": {"name": "Ops"}
  },
  "oauth": {
    "session_secret": "long-random-session-secret",
    "cookie_domain": ".pods.example.com",
    "session_hours": 168,
    "providers": [
      {
        "id": "google",
        "name": "Google",
        "issuer": "https://accounts.google.com",
        "client_id": "...",
        "client_secret": "...",
        "redirect_url": "https://pods.example.com/api/auth/callback/google",
        "allowed_domains": ["example.com"]
      }
    ]
  },
  "apps": [
    {"team": "ops", "site": "*", "auth": "required"},
    {"team": "public", "site": "profile", "auth": "optional"}
  ]
}
```

OAuth users are matched to configured users by email or by an explicit
`"<provider>:<subject>"` entry in `oauth`. Provider `allowed_domains` and
`allowed_emails` restrict who can sign in at all. Team roles are `reader`, `publisher`,
and `admin`; `admin: true` or `"*": "admin"` grants all teams. App auth modes are:

- `public` — default; static files are open.
- `optional` — static files are open, but apps can call `/api/me` to discover a user.
- `required` — static files redirect to the first configured OAuth provider unless the
  request already has a valid session or bearer token.

### HTTP API

Errors are JSON: `{"error":"..."}` with a 4xx/5xx status.

| Method & path | Auth | Behavior |
|---|---|---|
| `GET /healthz` | no | `{"ok":true}` |
| `GET /api/me` | optional | current user profile and `login_url` for bearer token or OAuth session |
| `GET /api/auth/providers` | no | list configured OAuth providers and login URLs |
| `GET /api/auth/login/{provider}` | no | start OAuth login; accepts `return_to` |
| `GET /api/auth/callback/{provider}` | no | OAuth redirect URI; creates the signed session cookie |
| `POST /api/auth/logout` | no | clear the session cookie |
| `GET /api/sites` | yes | list sites visible to the current user |
| `PUT /api/sites/{name}` | no | legacy shortcut for `public` team publish; body = tar.gz |
| `DELETE /api/sites/{name}` | publisher/admin | remove a public-team site; 404 if absent |
| `PUT /api/teams/{team}/sites/{name}` | no for `public` when enabled, publisher/admin otherwise | deploy to a team; returns URL `<name>.<team>.<base>` |
| `DELETE /api/teams/{team}/sites/{name}` | publisher/admin | remove a team site; 404 if absent |
| `GET /api/db` | reader, site host only | list collections for the current `<name>.<team>` host |
| `GET /api/db/{coll}` | reader, site host only | query documents (see below) |
| `POST /api/db/{coll}` | publisher/admin, site host only | create a doc (auto `id`); 201 with the full doc |
| `GET /api/db/{coll}/{id}` | reader, site host only | get a doc; 404 if absent |
| `PUT /api/db/{coll}/{id}` | publisher/admin, site host only | replace (upsert; keeps `created_at` if it existed) |
| `PATCH /api/db/{coll}/{id}` | publisher/admin, site host only | shallow merge into an existing doc; 404 if absent |
| `DELETE /api/db/{coll}/{id}` | publisher/admin, site host only | delete a doc; 404 if absent |
| `DELETE /api/db/{coll}` | publisher/admin, site host only | drop the whole collection; 404 if absent |
| `GET /api/events` | reader on site host, admin on base host | SSE stream: site-scoped on `<name>.<team>` hosts, global on the base host |
| `GET /sites/{team}/{site}/{path...}` | no | path fallback for static files; subdomain serving is preferred |
| `GET /` | no | landing page listing deployed sites |
| `GET /pods.js` | no | zero-dependency browser JS client |

`GET /sites/{site}` is a legacy fallback for `public/<site>`.

Validation: site names are DNS-label style (`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`);
collection names and client-supplied doc IDs match `^[A-Za-z0-9_-]{1,64}$`. Uploads are
capped at 256 MiB and 10,000 files per site; DB request bodies at 1 MiB. Tar extraction is
zip-slip safe (no absolute paths, `..`, symlinks, hardlinks, or devices).

## JS client (`/pods.js`)

Every podbay serves a tiny browser client at `/pods.js`. On a site subdomain, same-origin
`/api/db` is automatically scoped to that site tenant:

```html
<script src="/pods.js"></script>
<script type="module">
  const pods = Pods({ token: "..." }); // endpoint defaults to same origin
  const me = await pods.me();
  const providers = await pods.auth.providers({ returnTo: location.href });
  const posts = pods.db.collection("posts");

  const doc = await posts.create({ title: "hi" });
  await posts.query({ where: { status: "draft" }, sort: "-created_at", limit: 10 });
  await posts.get(doc.id);
  await posts.patch(doc.id, { status: "published" });
  await posts.set(doc.id, { title: "rewritten" });
  await posts.delete(doc.id);
</script>
```

## The JSON store

Documents are JSON objects stored in SQLite. The server injects and maintains three reserved fields:
`id` (16 hex chars), `created_at`, and `updated_at` (RFC3339 UTC). Clients can't override
`id`, `created_at`, or `updated_at` — the server always wins. Documents are partitioned
by `<team>/<site>` and persisted to `<data>/db.sqlite`. If a legacy `<data>/db.json`
exists and the SQLite DB is empty, podbay imports the JSON data on startup.

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
  `<data>/auth.json`.
- **Auth config**: edit `<data>/auth.json` for OAuth providers, session cookie settings,
  users, machine tokens, team roles, and per-app auth modes. See `auth.example.json`.
- **Data dir**: everything lives under one directory — back it up and you're done:

  ```
  <data>/secret            # generated secret (if not configured)
  <data>/auth.json         # users, teams, app auth policies
  <data>/sites/<team>/<name>/...  # deployed static files
  <data>/sites.json               # site team metadata
  <data>/db.sqlite                # tenant-scoped SQLite document store
  ```

- **DNS / reverse proxy**: point wildcard DNS such as `*.pods.example.com` at podbay.
  If TLS terminates at a proxy, set `--public-url https://pods.example.com` so deploys
  print URLs like `hello.public.pods.example.com`.

## Development

Go 1.25 with SQLite/auth/CLI dependencies (`modernc.org/sqlite`, `go-oidc`, `oauth2`,
`securecookie`, and `golang.org/x/term`).

| Target | What it does |
|---|---|
| `make build` | build both binaries into `bin/` (version from `git describe`) |
| `make test` | run all tests |
| `make run` | run the server locally for development |
| `make docker` | build the Docker image |
| `make clean` | remove `bin/` |

## License

MIT. Go forth and deploy folders.
