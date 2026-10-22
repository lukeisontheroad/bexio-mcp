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
| `bexio_docs`  | API conventions, operation groups, and each operation's parameters and request body, several topics per call. Shows only what the connection was granted. The assistant reads it first. |
| `bexio_read`  | Runs read operations (GET and searches) by operationId. `fields` keeps only the named fields of each record. |
| `bexio_write` | Runs everything that changes data: create, edit, delete, issue, send. Absent on read-only connections. |

Three tools instead of one per endpoint keeps the assistant's tool choice
reliable; `guide.md` teaches it bexio's quirks (search syntax, partial
edits, status ids, the `[Network Link]` placeholder for e-mails, ...).

Not available: bank payments, including outgoing payments of bills, and
payroll (never callable, their scopes are never requested), PDF and file
downloads, file uploads.

## Signing in and permissions

1. The client registers itself with the server automatically (OAuth dynamic
   client registration); nobody enters a client ID or secret.
2. A consent page shows which application asks and where it will be sent
   back, and lists every kind of bexio record on its own row, each Off, Read
   or Read & write (the default where bexio can change it).
3. The user signs in at bexio and picks the company. If the connector URL
   names a company (see below), any other one is refused.
4. The client receives a token that only works through this server.

| Section on the consent page | Rows                                                       | bexio scopes                                    |
|-----------------------------|------------------------------------------------------------|-------------------------------------------------|
| Contacts                    | Contacts, Additional Addresses, Contact Relations          | `contact_*`                                     |
| Sales                       | Quotes, Orders, Invoices, Deliveries                       | `kb_offer_*`, `kb_order_*`, `kb_invoice_*`, `kb_delivery_*` |
| Items and stock             | Items, Stock Areas, Stock locations                        | `article_*`, `stock_edit`                       |
| Purchase                    | Bills, Expenses, Purchase Orders                           | `kb_bill_show`, `kb_expense_show`, `kb_article_order_*`, `contact_show` |
| Projects                    | Projects, Timesheets                                       | `project_*`, `monitoring_*`                     |
| Notes, tasks and files      | Notes, Tasks, Files                                        | `note_*`, `task_*`, `file`                      |
| Accounting                  | Accounts, Account Groups, Business and Calendar Years, Manual Entries, Reports, Vat Periods, Bank Accounts | `accounting`, `bank_account_show` |
| Master data                 | Units, Taxes, Currencies, Payment Types, Countries, Languages and 11 more | none                             |

The rows are the groups `bexio_docs` shows (positions and comments follow
their quote, order or invoice), so the assistant sees the same names. The
server enforces each row itself, on every call: **Off** refuses its
operations, **Read** refuses those that change data and drops its `_edit`
scopes unless a row on Read & write shares them. That also covers accounting, files, stock and master data,
where bexio has no separate read scope or no scope at all. Rows bexio cannot
change offer only Off and Read. Turning any master data off shows a warning:
records like invoices and contacts refer to it. Converting a document (quote
to order or invoice, order to delivery) needs both rows on Read & write. With
nothing on Read & write, the `bexio_write` tool is gone.

Errors people see in the browser (sign-in errors, unknown addresses, a
connector URL opened directly, crashes) share the consent page's design.

**Several companies:** a bexio token belongs to one company. Add one
connector per company, with the company's name in the URL, e.g.
`/mcp/acme-ag` for "Acme AG" and `/mcp/acme-group-ag` for "Acme Group AG".
Case, spaces and punctuation don't matter (`/mcp/acmeag` works too), but
the whole name must match: `/mcp/acme` is neither. A sign-in to another company is refused with a page
naming the company picked and its connector URL, so a misspelt name shows
the right one. Plain `/mcp` accepts whichever company is picked.

The check relies on the client sending the connector URL at sign-in (the
`resource` parameter the MCP spec requires). A client that doesn't send it
is not checked.

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
- **Error reports (optional):** only with `SENTRY_DSN` set, and only real
  faults (crashes, network errors, bexio 5xx). Events keep method, path,
  message and stack trace; query strings, headers, cookies, bodies and user
  info are stripped before sending.
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
connector, URL `https://mcp.bextrack.com/mcp` (or `/mcp/<company-name>`, see above), OAuth
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
| `SENTRY_DSN`          | Optional: report faults to Sentry; off when unset          |
| `SENTRY_ENVIRONMENT`  | Optional: Sentry environment name                          |
| `IMAGE`, `HOST_PORT`  | compose only: image to run, loopback port to publish       |

The bexio app needs exactly one redirect URI: `<PUBLIC_URL>/oauth/callback`.

Endpoints: `/mcp` and `/mcp/<company-name>` (MCP), `/oauth/*` and
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
