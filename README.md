# multica-slack

Standalone Go bridge connecting [Multica Cloud](https://multica.ai) with Slack:

- **Rich Link Unfurling:** Renders GitHub-style vertical-bar cards in channels and DMs for Multica issue, project, and doc links using the poster's own Multica Personal Access Token (PAT).
- **Real-Time DM Notifications:** Maintains a persistent per-user Multica `/ws` stream with `/api/inbox` reconnect catch-up, no-backfill pause/resume, and bidirectional read-state sync.
- **Interactive Slack Actions:** `Open in Multica`, `Mark Read`, and `Reply` (posts a comment directly to the Multica issue) right from Slack DMs.
- **Self-Serve App Home & `/multica`:** Connect/update a PAT, switch active notification workspaces, authorize per-user DM/uninvited-channel unfurling via Slack OAuth, and toggle notification categories.
- **Zero-CGO Encrypted Storage:** Pure-Go SQLite (`modernc.org/sqlite`) in WAL mode with AES-256-GCM field-level encryption for stored tokens.

## Environment Variables

|Variable|Required|Default|Description|
|-|-|-|-|
|`PUBLIC_BASE_URL`|Yes|-|Public HTTPS base URL (e.g. `https://multica-slack.example.com`) used for OAuth redirect URLs|
|`ENCRYPTION_KEY`|Yes|-|Base64-encoded 32-byte key for AES-256-GCM token encryption and OAuth link signing|
|`SLACK_BOT_TOKEN`|Yes|-|Slack Bot User OAuth Token (`xoxb-...`)|
|`SLACK_SIGNING_SECRET`|Yes|-|Slack app Signing Secret for verifying `/slack/*` requests|
|`SLACK_CLIENT_ID`|Yes|-|Slack App Client ID for per-user OAuth (`links:read`, `links:write`)|
|`SLACK_CLIENT_SECRET`|Yes|-|Slack App Client Secret for per-user OAuth|
|`SLACK_ALLOWED_TEAM_ID`|No|`""`|Optional Slack workspace ID (`T...`) allowlist enforced on events, commands, and OAuth|
|`PORT`|No|`3020`|HTTP listen port|
|`DATA_DIR`|No|`/data`|Directory for `multica-slack.db` SQLite database and WAL files|
|`MULTICA_API_URL`|No|`https://api.multica.ai`|Multica REST & WebSocket API base URL|
|`MULTICA_APP_URL`|No|`https://multica.ai`|Multica web app base URL matched by link unfurling and deep links|
|`MULTICA_DEFAULT_WORKSPACE_SLUG`|No|`""`|Default workspace slug pre-selected when a user connects a PAT|
|`MULTICA_FALLBACK_PAT`|No|`""`|Optional fallback PAT used for link unfurling when the poster has not linked a PAT|

Generate `ENCRYPTION_KEY`:

```bash
openssl rand -base64 32
```

## Slack App Setup

1. Create a new Slack app from [`slack-manifest.example.yaml`](slack-manifest.example.yaml) in the [Slack API dashboard](https://api.slack.com/apps).
2. Replace every `<PUBLIC_BASE_URL>` placeholder with your deployment's public HTTPS host (without `https://` prefix, since `https://` is already in the manifest template).
3. Install the app to your workspace and copy:
   - **Bot User OAuth Token** -> `SLACK_BOT_TOKEN`
   - **Signing Secret** -> `SLACK_SIGNING_SECRET`
   - **Client ID** & **Client Secret** -> `SLACK_CLIENT_ID` & `SLACK_CLIENT_SECRET`
4. Open the **Multica** app Home tab in Slack (or run `/multica connect`) to link your Multica PAT and optionally enable 1:1 DM & uninvited-channel link unfurling.

## Running with Docker Compose

Pre-built multi-arch images (`linux/amd64`, `linux/arm64`) are published to GHCR on every release tag:

```text
ghcr.io/everlightlabs/multica-slack:<version>
```

Export the required environment variables (or place them in an untracked `.env` file), then start the service:

```bash
docker compose up -d
```

Health & readiness endpoints:

- `GET /healthz` — liveness check (`200 OK`)
- `GET /readyz` — SQLite readiness check and aggregate counts (`linked_users`, `ws_connected`, `ws_auth_failed`)

After connecting your PAT in the Slack App Home tab, confirm the status line reads `WS: connected` (it updates automatically once the Multica WebSocket authenticates) and `/readyz` reports `ws_connected >= 1`.

## Deployment Notes

- **Always-on container:** `multica-slack` holds persistent outbound WebSocket connections to Multica Cloud per linked user, so run it as an always-on container rather than a scale-to-zero/serverless function.
- **Persistent `/data` volume:** Mount a persistent volume at `/data` owned by uid/gid `65532` (`nonroot` in `gcr.io/distroless/static-debian12:nonroot`). On `SIGTERM` / `SIGINT`, the process stops HTTP ingress, drains async handlers, closes WebSocket streams, and checkpoints the SQLite WAL cleanly (`PRAGMA wal_checkpoint(TRUNCATE)`).
- **TLS reverse proxy (Caddy / nginx / Traefik):** Terminate HTTPS at your edge proxy and forward to port `3020` over the internal network.
- **Render Web Service:** Deploy directly from the `Dockerfile` on a Starter (always-on) instance with a persistent disk mounted at `/data` and `PORT=3020`.
