<p align="center"><img src="assets/logo.png" width="360"></p>

# Happy Pods

Deploy a folder, get a URL. Plus a tiny Firebase-style JSON document store.

Happy Pods is a self-hostable clone of Shopify's internal **Quick** platform, lovingly
inspired by their write-up: [How We Built Quick, Shopify's Internal Deployment Tool](https://shopify.engineering/quick).
Two small Go binaries, zero dependencies beyond the standard library:

- **`pods`** — the CLI you run on your machine
- **`podbay`** — the server ("open the pod bay doors")

## Why

Sometimes you just want to put a folder of HTML on the internet (or intranet) without a
build pipeline, a YAML sacrifice, or a cloud bill. Happy Pods gives you:

- **One-command deploys** — `pods deploy` tars up a folder and ships it.
- **Instant URLs** — every site gets a tenant subdomain: `<name>.<team>.<base>`.
- **A wee JSON store** — collections of documents with create/get/set/patch/delete and
  simple queries, usable from the CLI or straight from the browser via `/pods.js`.
- **Boring tech** — one static binary each, files on disk, a single JSON file for the DB.

The Quick-style tradeoff, stated honestly: **deployed sites and the landing page are open
to everyone who can reach the server.** The special `public` team also accepts publishes
without a bearer secret. Other teams and all database routes require the secret.

## Quickstart

Build the image and the CLI (`make build` drops `pods` into `bin/`), then:

```sh
make docker                                                          # 1. build the image
docker run -d --name podbay -p 7777:7777 -v podbay-data:/data podbay # 2. run the server
docker logs podbay                                                   # 3. copy the "generated secret: ..." line
pods login --endpoint http://localhost:7777 --secret <paste-secret>  # 4. point the CLI at it
pods init hello && pods deploy hello --team public                   # 5. deploy hello.public.localhost
pods open hello --team public                                        # 6. print the subdomain URL
```

Prefer compose? `PODBAY_SECRET=$(openssl rand -hex 16) docker compose up -d`.

## CLI reference

Configuration resolution, highest wins: flags `--endpoint`/`--secret` → env
`PODS_ENDPOINT`/`PODS_SECRET` → `~/.config/pods/config.json` (written by `pods login`, mode 0600).

| Command | What it does |
|---|---|
| `pods login [--endpoint URL] [--secret S]` | Verify and save credentials. Single-line when both flags are given; otherwise prompts (secret hidden). Scriptable when stdin isn't a TTY. |
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
| `--secret` | `PODBAY_SECRET` | (generated) | API bearer secret |
| `--public-url` | `PODBAY_PUBLIC_URL` | (derived from request) | base URL used when printing site URLs |

If no secret is configured, the server generates 32 hex characters on first boot, persists
them to `<data>/secret` (mode 0600), and prints `generated secret: ...` to stdout **once**.
The same secret is reused on restart.

### HTTP API

All `/api/*` routes require `Authorization: Bearer <secret>`. Errors are JSON:
`{"error":"..."}` with a 4xx/5xx status.

| Method & path | Auth | Behavior |
|---|---|---|
| `GET /healthz` | no | `{"ok":true}` |
| `GET /api/sites` | yes | list sites, sorted by name |
| `PUT /api/sites/{name}` | no | legacy shortcut for `public` team publish; body = tar.gz |
| `DELETE /api/sites/{name}` | yes | remove a site; 404 if absent |
| `PUT /api/teams/{team}/sites/{name}` | no for `public`, yes otherwise | deploy to a team; returns URL `<name>.<team>.<base>` |
| `DELETE /api/teams/{team}/sites/{name}` | yes | remove a team site; 404 if absent |
| `GET /api/db` | yes, site host only | list collections for the current `<name>.<team>` host |
| `GET /api/db/{coll}` | yes, site host only | query documents (see below) |
| `POST /api/db/{coll}` | yes, site host only | create a doc (auto `id`); 201 with the full doc |
| `GET /api/db/{coll}/{id}` | yes, site host only | get a doc; 404 if absent |
| `PUT /api/db/{coll}/{id}` | yes, site host only | replace (upsert; keeps `created_at` if it existed) |
| `PATCH /api/db/{coll}/{id}` | yes, site host only | shallow merge into an existing doc; 404 if absent |
| `DELETE /api/db/{coll}/{id}` | yes, site host only | delete a doc; 404 if absent |
| `DELETE /api/db/{coll}` | yes, site host only | drop the whole collection; 404 if absent |
| `GET /api/events` | yes | SSE stream: site-scoped on `<name>.<team>` hosts, global on the base host |
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
  const pods = Pods({ secret: "..." }); // endpoint defaults to same origin
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

Documents are JSON objects. The server injects and maintains three reserved fields:
`id` (16 hex chars), `created_at`, and `updated_at` (RFC3339 UTC). Clients can't override
`id`, `created_at`, or `updated_at` — the server always wins. Documents are partitioned
by `<team>/<site>` and persisted to `<data>/db.json` atomically on every mutation.

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
  (printed once, stored at `<data>/secret`). Treat it like a password — it's the only
  thing guarding `/api/*`.
- **Data dir**: everything lives under one directory — back it up and you're done:

  ```
  <data>/secret            # generated secret (if not configured)
  <data>/sites/<team>/<name>/...  # deployed static files
  <data>/sites.json               # site team metadata
  <data>/db.json                  # tenant-scoped JSON store
  ```

- **DNS / reverse proxy**: point wildcard DNS such as `*.pods.example.com` at podbay.
  If TLS terminates at a proxy, set `--public-url https://pods.example.com` so deploys
  print URLs like `hello.public.pods.example.com`.

## Development

Go 1.25, standard library plus `golang.org/x/term` only.

| Target | What it does |
|---|---|
| `make build` | build both binaries into `bin/` (version from `git describe`) |
| `make test` | run all tests |
| `make run` | run the server locally for development |
| `make docker` | build the Docker image |
| `make clean` | remove `bin/` |

## License

MIT. Go forth and deploy folders.
