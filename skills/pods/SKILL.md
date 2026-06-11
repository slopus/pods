---
name: happy-pods
description: Deploy static sites and use the Happy Pods per-site JSON store via the pods CLI; use when asked to publish a folder of static files to a podbay server (such as podbay.dev) or to create, query, update, or delete JSON documents in a site's collections.
---

# Happy Pods

Happy Pods is a deploy-a-folder platform with a small per-site JSON document store.
The `pods` CLI talks to a `podbay` server (public instance: `https://podbay.dev`).
Each site is served at `<name>.<base>`, e.g. `https://hello.podbay.dev`. Use this skill
when you need to publish static files (HTML/CSS/JS) and get a URL, or when you need
lightweight JSON persistence scoped to a site.

Access model, in one breath: site files and the JSON store are **public** — anyone can
read a site and read/write its store, no token needed. Authentication only gates
publishing: a site is owned by the account that first deployed it, and only that owner
(or a server admin) can redeploy or delete it.

## Configure

The CLI resolves config in this order (highest wins): `--endpoint`/`--token` flags >
`PODS_ENDPOINT`/`PODS_TOKEN` env vars (`PODS_SECRET` is a legacy alias for `PODS_TOKEN`;
`--secret` for `--token`) > `~/.config/pods/config.json`.

Non-interactive option A — environment variables (good for one-off commands and CI):

```sh
export PODS_ENDPOINT=https://podbay.dev
export PODS_TOKEN=<api-token-or-static-token>
pods status
```

Non-interactive option B — save a pre-provisioned token once (writes
`~/.config/pods/config.json`, no prompts when both flags are given):

```sh
pods login --endpoint https://podbay.dev --token <api-token-or-static-token>
```

This verifies the token against the server before saving and prints
`logged in to <endpoint> as <user>`.

Interactive only — GitHub device login. Running `pods login` **without** `--token`
(it targets `https://podbay.dev` by default; pass `--endpoint URL` for a self-hosted
server) starts the GitHub device flow: it prints
`Open https://github.com/login/device and enter code XXXX-XXXX` to stderr and polls until
a human approves in a browser. Do not use this in non-interactive automation; obtain a
token in advance instead (a human runs the device login once, or a self-hosted server's
bootstrap admin secret from its startup logs / `<data>/secret` is used as a static token).

Token lifetime: GitHub logins yield a single 30-day JWT. The CLI refreshes it
automatically (via `POST /api/auth/refresh`) whenever fewer than 7 days remain and
rewrites the config file — **no action needed by you**. Static tokens (like the bootstrap
admin secret) never expire and are never rewritten. If a JWT fully expires, commands fail
with 401 and a human must log in again.

Sanity-check connectivity and identity at any time:

```sh
pods status    # endpoint, health, user (or "anonymous"), site count
```

## Develop locally (no login, no deploy)

`pods dev [dir]` runs a local server that serves the directory live and exposes the same
`/api/db` JSON store and `/pods.js` client as production, backed by in-memory SQLite. No
authentication, no network, nothing written to disk; the store resets on exit. Use it to
build or test a site before deploying.

```sh
pods dev ./site --addr :7777      # serves http://localhost:7777 with /api/db + /pods.js
```

It blocks until interrupted, so run it in the background when scripting
(`pods dev ./site --addr :7777 &`), then talk to `http://localhost:7777/api/db/...`
directly — no token and no subdomain host needed.

## Deploy a folder

```sh
pods deploy ./site --name myapp
```

- Name resolution: `--name` flag > `"name"` in the folder's `pods.json` > folder basename.
- Requires authentication. First deploy of a name claims it for your account; redeploying
  a name owned by someone else fails with 403 (`site "x" is owned by <login>`) — pick
  another name.
- The CLI tars the folder, skipping `node_modules`, `pods.json`, and all dotfiles
  (which covers `.git`).
- Output includes the file count, upload size, and the site URL (`https://<name>.<base>`).
- Site names must be DNS-label style: lowercase letters, digits, hyphens, max 63 chars.
- Flags may follow the positional: `pods deploy site --name myapp` works.

Other site commands:

```sh
pods list             # table of deployed sites: NAME, OWNER, FILES, SIZE, UPDATED
pods open myapp       # print the site URL
pods rm myapp --yes   # delete (owner/admin only) without an interactive prompt
```

Always pass `--yes` to `pods rm` and `pods db ... drop` when running non-interactively.

## Use the JSON store

Each deployed site has its own document store, selected by the endpoint **host**: point
`PODS_ENDPOINT` (or `pods login --endpoint`) at the site's subdomain, for example
`https://myapp.podbay.dev`. The API paths are still just `/api/db/...`; the host supplies
the site. No token is needed — the store is public by design. The store only exists for
deployed sites; an undeployed name gets 404.

```sh
export PODS_ENDPOINT=https://myapp.podbay.dev

# create (inline JSON or "-" to read stdin)
pods db posts create '{"title":"hello","status":"draft"}'
echo '{"title":"from stdin"}' | pods db posts create -

# read
pods db posts get 4f2a9c1d8e3b7a06

# replace (upsert) and merge
pods db posts set 4f2a9c1d8e3b7a06 '{"title":"replaced","status":"draft"}'
pods db posts patch 4f2a9c1d8e3b7a06 '{"status":"published"}'

# query: repeatable --where (ANDed), sort (- prefix = descending), limit/offset
pods db posts list --where status=draft --sort -created_at --limit 10

# machine-readable output: full QueryResult ({"docs":[...],"total":N}) for piping
pods db posts list --where status=draft --json

# delete a doc / drop a collection
pods db posts rm 4f2a9c1d8e3b7a06
pods db posts drop --yes
```

The server manages reserved fields `id`, `created_at`, `updated_at` — do not set them
yourself; they will be overridden.

Update streams use public SSE from the same site endpoint (no auth header needed):

```sh
curl -N "$PODS_ENDPOINT/api/events"
```

Notes for scripting:

- `pods db <coll> list` prints one JSON doc per line by default; `--json` prints the full
  result object including `total` (the match count before limit/offset).
- `--where` matches top-level fields only: strings by equality, numbers numerically,
  booleans as `true`/`false`, nulls as `null`; docs missing the field don't match.
- Collection names and custom doc IDs must match `^[A-Za-z0-9_-]{1,64}$`.
- Exit code 0 on success, 1 on error; errors go to stderr prefixed `pods: `.
- The store is world-writable: never put secrets or trust-sensitive data in it.

## Recipe: build a quick prototype

```sh
pods init demo                            # scaffold pods.json + a starter index.html
# ... edit demo/index.html (it shows how to use /pods.js for the JSON store) ...
pods deploy demo                          # ship it to https://demo.<base>
pods open demo                            # get the URL
PODS_ENDPOINT=https://demo.<base> pods db notes create '{"text":"it works"}'
```

Iterate by editing files and re-running `pods deploy demo` — each deploy atomically
replaces the site. Pages can talk to their own scoped store from the browser via the
server-hosted `/pods.js` client and same-origin `/api/db`, no token required.

## Troubleshooting

- **`401` / unauthorized**: missing or expired token on a publish/delete. Check
  `PODS_TOKEN`, or have a human re-run `pods login --endpoint ...` (GitHub device flow).
  On a self-hosted server, the bootstrap admin secret is in the startup logs or
  `<data>/secret`.
- **`403` on deploy or rm**: the site name is owned by another account. Deploy under a
  different name, or use an admin token.
- **connection refused / timeout**: wrong endpoint or server not running. Verify
  `PODS_ENDPOINT` (default server port is 7777), then check the server with
  `curl <endpoint>/healthz` — it should return `{"ok":true}`.
- **`400` on deploy**: invalid site name (must be lowercase DNS-label style) or the
  archive violates limits (max 10,000 files, 256 MiB per site).
- **`400` "site API requires a <site> subdomain host"**: db commands were run against the
  base host. Set `PODS_ENDPOINT` to the site's subdomain (`https://myapp.podbay.dev`).
- **`404` from db commands**: the site is not deployed, or the document/collection does
  not exist; `get`, `patch`, `rm`, and `drop` require an existing target (use `set` to
  upsert, `create` to make new docs).
- **flags seem ignored**: command-line flags override `PODS_ENDPOINT`/`PODS_TOKEN`,
  which override `~/.config/pods/config.json`. `pods status` shows which endpoint is in
  effect; `pods logout` removes the saved config.
