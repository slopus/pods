---
name: happy-pods
description: Deploy static sites and use the Happy Pods JSON store via the pods CLI; use when asked to publish a folder of static files to a podbay server or to create, query, update, or delete JSON documents in its collections.
---

# Happy Pods

Happy Pods is a self-hosted deploy-a-folder platform with a small JSON document store.
The `pods` CLI talks to a `podbay` server. Each site belongs to a team and is served at
`<name>.<team>.<base>`. Use this skill when you need to publish static files (HTML/CSS/JS)
and get a URL, or when you need lightweight JSON persistence for a site tenant.

## Configure

The CLI resolves config in this order (highest wins): flags > env vars > config file.

Non-interactive option A — environment variables (good for one-off commands and CI):

```sh
export PODS_ENDPOINT=http://localhost:7777
export PODS_SECRET=0123456789abcdef0123456789abcdef
pods status
```

Non-interactive option B — save credentials once with login (writes
`~/.config/pods/config.json`):

```sh
pods login --endpoint http://localhost:7777 --secret 0123456789abcdef0123456789abcdef
```

With both flags given, `pods login` runs single-line with no prompts. It verifies the
credentials against the server before saving and prints `logged in to <endpoint>`.

The secret: if the server wasn't started with `--secret`/`PODBAY_SECRET`, it generated one
at first boot, printed `generated secret: ...` to stdout, and stored it at `<data>/secret`
(e.g. `docker logs podbay`, or `cat /data/secret` inside the container).

Sanity-check connectivity and credentials at any time:

```sh
pods status
```

## Deploy a folder

```sh
pods deploy ./site --name myapp --team public
```

- Name resolution: `--name` flag > `"name"` in the folder's `pods.json` > folder basename.
- Team resolution: `--team` flag > `"team"` in `pods.json` > `public`.
- The special `public` team can be published without a secret; every other team requires the bearer secret.
- The CLI tars the folder, skipping `.git`, `node_modules`, `pods.json`, and dotfiles.
- Output includes the file count, upload size, and the subdomain URL
  (`<name>.<team>.<base>`).
- Site names must be DNS-label style: lowercase letters, digits, hyphens, max 63 chars.

Other site commands:

```sh
pods list                         # table of deployed sites with TEAM and NAME
pods open myapp --team public     # print the subdomain URL
pods rm myapp --team public --yes # delete without an interactive prompt
```

Always pass `--yes` to `pods rm` and `pods db ... drop` when running non-interactively.

## Use the JSON store

Documents are JSON objects scoped to the endpoint's site host. For DB commands, configure
`PODS_ENDPOINT` or `pods login --endpoint` to the site subdomain, for example
`https://myapp.public.pods.example.com`. The API paths are still just `/api/db/...`; the
host supplies the tenant. The server manages reserved fields `id`, `created_at`,
`updated_at` — do not set them yourself; they will be overridden.

```sh
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

Update streams use authenticated SSE from the same site endpoint:

```sh
curl -N -H "Authorization: Bearer $PODS_SECRET" "$PODS_ENDPOINT/api/events"
```

Notes for scripting:

- `pods db <coll> list` prints one JSON doc per line by default; `--json` prints the full
  result object including `total` (the match count before limit/offset).
- `--where` matches top-level fields only: strings by equality, numbers numerically,
  booleans as `true`/`false`, nulls as `null`.
- Collection names and custom doc IDs must match `^[A-Za-z0-9_-]{1,64}$`.
- Exit code 0 on success, 1 on error; errors go to stderr prefixed `pods: `.

## Recipe: build a quick prototype

```sh
pods init demo                    # scaffold pods.json + a starter index.html
# ... edit demo/index.html (it shows how to use /pods.js for the JSON store) ...
pods deploy demo --team public    # ship it to demo.public.<base>
pods open demo --team public      # get the URL
```

Iterate by editing files and re-running `pods deploy demo` — each deploy atomically
replaces the site. Pages can talk to their own scoped store from the browser via the
server-hosted `/pods.js` client and same-origin `/api/db`.

## Troubleshooting

- **`401` / unauthorized**: wrong or missing secret. Check `PODS_SECRET` or re-run
  `pods login --endpoint ... --secret ...`. Recover the server's generated secret from its
  startup logs or `<data>/secret`.
- **connection refused / timeout**: wrong endpoint or server not running. Verify
  `PODS_ENDPOINT` (default server port is 7777), then check the server with
  `curl <endpoint>/healthz` — it should return `{"ok":true}`.
- **`400` on deploy**: invalid site name (must be lowercase DNS-label style) or the
  archive violates limits (max 10,000 files, 256 MiB per site).
- **`404` from db commands**: the document or collection does not exist; `get`, `patch`,
  `rm`, and `drop` require an existing target (use `set` to upsert, `create` to make
  new docs).
- **flags seem ignored**: command-line flags override `PODS_ENDPOINT`/`PODS_SECRET`,
  which override `~/.config/pods/config.json`. `pods status` shows which endpoint is in
  effect; `pods logout` removes the saved config.
