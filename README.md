# BexTrackMCP

Remote MCP server that connects AI assistants (Claude on the web, desktop
and mobile, Claude Code, ChatGPT, Cursor, VS Code, ...) to a bexio account.
Users add one URL, sign in with bexio, and pick what the assistant may use.

Hosted instance:

```
https://mcp.bextrack.com/mcp
```

> **No warranty, no liability.** The hosted instance is a free, best-effort
> service provided as is: no guarantee of availability, security,
> correctness or continued operation, and no liability for any damage or
> loss, including changes it makes to your bexio data on your instruction.
> Use it at your own risk, or run your own (see below). The code is MIT
> licensed under the same terms.

API reference for bexio: https://docs.bexio.com/

## What it can do

The assistant gets three tools on top of the full public bexio API
(the OpenAPI spec is embedded, 284 operations):

| Tool          | Does                                                                 |
|---------------|----------------------------------------------------------------------|
| `bexio_docs`  | API conventions, operation groups, and each operation's parameters and request body. The assistant reads it first. |
| `bexio_read`  | Runs read operations (GET and searches) by operationId               |
| `bexio_write` | Runs everything that changes data: create, edit, delete, issue, send. Absent on read-only connections. |

Three tools instead of one per endpoint keeps the assistant's tool choice
reliable; `guide.md` teaches it bexio's quirks (search syntax, partial
edits, status ids, the `[Network Link]` placeholder for e-mails, ...).

Not available: bank payments and payroll (never callable, their scopes are
never requested), PDF and file downloads, file uploads.

## Signing in and permissions

1. The client registers itself with the server automatically (OAuth dynamic
   client registration); nobody enters a client ID or secret.
2. A consent page shows which application asks and where it will be sent
   back, and lets the user pick the areas and whether access is read only.
3. The user signs in at bexio and picks the company.
4. The client receives a token that only works through this server.

| Area on the consent page   | bexio scopes                                                         |
|----------------------------|----------------------------------------------------------------------|
| Contacts                   | `contact_show`, `contact_edit`                                       |
| Quotes                     | `kb_offer_show`, `kb_offer_edit` (+ convert to orders/invoices)      |
| Orders                     | `kb_order_show`, `kb_order_edit` (+ invoices, deliveries from orders) |
| Invoices                   | `kb_invoice_show`, `kb_invoice_edit`                                 |
| Items, stock, deliveries   | `article_*`, `stock_edit`, `kb_delivery_*`                           |
| Projects and time tracking | `project_*`, `monitoring_*`                                          |
| Notes and tasks            | `note_*`, `task_*`                                                   |
| Purchase                   | bills, expenses (read), purchase orders                              |
| Accounting                 | `accounting`                                                         |
| Files                      | `file`                                                               |
| Bank accounts              | `bank_account_show` (read only)                                      |

**Read only** drops every `_edit` scope and removes the `bexio_write` tool,
which also covers accounting, files and stock, where bexio has no separate
read scope.

**Several companies:** a bexio token belongs to one company. Add one
connector per company and give each URL its own label, e.g.
`/mcp/acme` and `/mcp/globex`. The label only makes the URLs unique.

## Data and credentials

- **Nothing is stored.** No database, no files: the container runs with a
  read-only filesystem. bexio data passes through memory for the duration
  of one request.
- **No user credentials.** Passwords are entered at bexio only. The server
  issues encrypted (AES-GCM) client IDs, codes and tokens; the bexio tokens
  inside them live with the client (e.g. in Claude), not on the server.
- **One server credential:** the bexio app's client secret, from the
  environment. It also derives the encryption key, so rotating it signs
  every user out.
- **Logs** hold method, path, MCP method, client user agent, bexio
  operation and status code. Never tokens, codes, query strings, request or
  response bodies.
- **In transit:** TLS ends at the reverse proxy (Cloudflare for the hosted
  instance), which forwards to the container on loopback.

## Security

- Consent is required for every sign-in. A per-sign-in `__Host-` cookie
  ties the consent form and bexio's callback to the user's browser, so a
  sign-in link someone else started cannot complete in your browser.
- Redirect URIs must be https, loopback http (native apps), or the
  `cursor`/`vscode` schemes; nothing redirects before consent.
- PKCE (S256) everywhere; refresh tokens expire after 90 days without use.
- The image is `FROM scratch`: the static binary and a CA bundle, running
  as an unprivileged user.

## Connect a client

**claude.ai / Claude Desktop:** Settings → Connectors → Add custom
connector, URL `https://mcp.bextrack.com/mcp` (or with a label), OAuth
client **Register automatically**. Connect, then choose the areas and the
company. Keep `bexio_write` on "Needs approval" in the connector's tool
permissions: it can issue and send documents and delete records.

**Claude Code:** `claude mcp add --transport http bexio https://mcp.bextrack.com/mcp`,
then `/mcp` → Authenticate.

Other MCP clients: add the URL as a remote (Streamable HTTP) server with
OAuth.

## Run your own

```sh
docker run -p 127.0.0.1:8080:8080 \
  -e PUBLIC_URL=https://bexio-mcp.example.com \
  -e BEXIO_CLIENT_ID=... -e BEXIO_CLIENT_SECRET=... \
  ghcr.io/<owner>/bexio-mcp:latest
```

or `docker compose up -d` with a `.env` (chmod 600) next to
`docker-compose.yml`. Put it behind TLS on a public URL.

| Variable              | Meaning                                                    |
|-----------------------|------------------------------------------------------------|
| `PUBLIC_URL`          | External base URL; the OAuth issuer and resource           |
| `BEXIO_CLIENT_ID`     | Client ID of your bexio app (developer.bexio.com)          |
| `BEXIO_CLIENT_SECRET` | Its secret                                                 |
| `PORT`                | Listen port, default `8080`                                |
| `IMAGE`, `HOST_PORT`  | compose only: image to run, loopback port to publish       |

The bexio app needs exactly one redirect URI: `<PUBLIC_URL>/oauth/callback`.

Endpoints: `/mcp` and `/mcp/<label>` (MCP), `/oauth/*` and
`/.well-known/oauth-authorization-server` (sign-in),
`/.well-known/oauth-protected-resource[/mcp...]` (also fine for health
checks).

## License

[MIT](LICENSE). Security issues: see [SECURITY.md](SECURITY.md).

## Development

```sh
go test ./...                    # unit and end-to-end tests, including the sign-in
python3 scripts/fetch-spec.py    # refresh openapi.json from docs.bexio.com
```

`guide.md` is what the assistant reads first; fix recurring mistakes there.

## Branches and releases

- `dev` is where work lands, through pull requests. `main` holds released
  code. Both are protected: changes only via pull request with passing CI.
- Commits follow [Conventional Commits](https://www.conventionalcommits.org/)
  (checked in CI). Versions follow semver and come from them: `fix:` bumps
  the patch, `feat:` the minor, `!`/`BREAKING CHANGE:` the major (minor
  while below 1.0).
- To release, open a pull request from `dev` to `main`. release-please then
  maintains a release pull request with the changelog; merging it tags
  `vX.Y.Z`, publishes the GitHub release, and pushes the image
  `ghcr.io/<owner>/bexio-mcp` as `X.Y.Z`, `X.Y` and `latest`
  (amd64 and arm64).
- The version shows in the consent page footer and in the MCP server info.
