# AGENTS.md — `multica-slack`

Standalone Go bridge connecting [Multica Cloud](https://multica.ai) with Slack: rich link unfurling across channels and DMs plus real-time per-user Slack DM notifications.

## Architecture & Package Boundaries

Keep each package focused on one responsibility:

|Package|Responsibility|
|-|-|
|`cmd/multica-slack`|Process entrypoint, wiring, Slack client adapters, daily 30-day DB pruning, graceful `SIGTERM`/`SIGINT` shutdown|
|`internal/config`|Environment variable loading and fail-fast startup validation (`ENCRYPTION_KEY` must decode to 32 raw bytes)|
|`internal/crypto`|AES-256-GCM field encryption (`v1:<iv_b64>:<tag_b64>:<ct_b64>`) and domain-separated HMAC-SHA256 OAuth launch tokens|
|`internal/store`|Pure-Go SQLite (`modernc.org/sqlite`) in WAL mode (`foreign_keys=ON`, `busy_timeout=5000`, `MaxOpenConns=1`) with WAL checkpoint on `Close()`|
|`internal/multica`|Multica Cloud REST client, 60s TTL workspace metadata cache, and per-user WebSocket client (`/ws` first-frame auth, ping/pong, exponential backoff)|
|`internal/prefs`|9 notification preference groups mapped 1:1 to Multica's inbox registry (`unknown` types default to `system_alerts`)|
|`internal/unfurl`|URL matcher (`issues`, `projects`, `docs`), GitHub-style Slack card formatter, 30s dedup, max-5 concurrent fetch, poster-PAT privacy enforcement|
|`internal/notify`|Per-user WebSocket supervisor, unread snapshot seeding (`notify_after`), 24h reconnect catch-up, DM card formatter (`MarkBlocksRead`), read-state sync|
|`internal/slackhttp`|HTTP handlers for `/slack/events`, `/slack/interactions`, `/slack/commands`, `/slack/oauth/{start,callback}`, `/healthz`, `/readyz`|

Design specification lives in [docs/superpowers/specs/2026-11-26-multica-slack-design.md](docs/superpowers/specs/2026-11-26-multica-slack-design.md).

## Non-Negotiable Invariants

1. **Pure Go (`CGO_ENABLED=0`):** Never introduce CGO dependencies. Multi-arch static binaries (`linux/amd64`, `linux/arm64`) must build cleanly in `Dockerfile` (`distroless/static-debian12:nonroot`).
2. **Encryption at rest:** Never store plaintext Multica PATs (`mul_...`) or Slack user OAuth tokens (`xoxp-...`) in SQLite. Always encrypt via `internal/crypto.Box`.
3. **Secret & content hygiene:** Never log PATs, Slack tokens, `Authorization` headers, `multica_pat_enc`, or raw comment bodies at `info` level. Never expose PII or tokens on `/healthz` or `/readyz`.
4. **Poster-PAT privacy in unfurling:** Always fetch Multica resources using the link poster's PAT (or an explicitly configured `MULTICA_FALLBACK_PAT`), never another user's PAT. Another user's `xoxp-` token may only be used as Slack `chat.unfurl` transport fallback in 1:1 DMs.
5. **Slack 3-second deadline & signing:** Every `/slack/*` POST route must verify HMAC signatures (`X-Slack-Signature` + `X-Slack-Request-Timestamp` <= 5m), cap request bodies at 1 MiB, enforce `SLACK_ALLOWED_TEAM_ID` when configured, and ack `200 OK` within 3 seconds (run Multica/Slack follow-ups asynchronously via `runAsync`).
6. **No notification floods or WS reader blocking:** Initial link, workspace switch, and resume must snapshot unread inbox IDs as `seeded` and advance `notify_after` (captured before `ListInbox`). Never run slow multi-call Slack loops (like `inbox:batch-read`) synchronously on the WebSocket reader goroutine.
7. **Error-checked closes:** Always check or explicitly discard (`defer func() { _ = ... }()`) `Close()` / `CloseNow()` returns so `golangci-lint` (`errcheck`) stays green.
8. **Public repository hygiene:** Never commit deployment-specific hosts, credentials, vault paths, or private workspace names to this repository. Keep deployment wiring in your infrastructure repository.

## Multica Cloud API Wire Quirks

Maintain compatibility with these live Multica Cloud payload shapes (covered by unit tests in `internal/multica/client_test.go` and `internal/notify/notify_test.go`):

- **`GET /api/inbox`:** `issue_due_date` may be a date-only string (`"YYYY-MM-DD"`) or `null`, not RFC3339 — keep it typed as `string`.
- **`GET /api/projects` & `Project`:** List responses return `{"projects": [...]}` (not a bare array), and project objects use `"title"` rather than `"name"`. Decode both wrapped/bare arrays and populate `Project.Name` from `"name"` or `"title"`.
- **`GET /api/projects/{id}/progress`:** Returns nested `{"progress": {"scope", "completed", "started", "cancelled"}}`. Keep `ProjectProgress.UnmarshalJSON` compatible with both nested and flat shapes.
- **`GET /api/issue-statuses`:** List responses return `{"statuses": [...]}` (not a bare array). Decode both wrapped and bare arrays.
- **Inbox body mentions:** Multica encodes mentions in comment bodies as `[label](mention://member|agent|squad|issue/<uuid>)`. Normalize these before truncating/escaping Slack mrkdwn so raw `mention://` URIs never leak into DM quotes.

## Development & Verification

Run all checks before committing or opening a PR:

```bash
go vet ./...
go test -count=1 -race ./...
CGO_ENABLED=0 go build ./cmd/multica-slack
```

If `golangci-lint` is installed locally, run `golangci-lint run` as well.

## Git & Contribution Conventions

- Work on feature branches; never commit implementation changes directly to `main`.
- Use Conventional Commits with a scope: `feat(unfurl): ...`, `fix(notify): ...`, `docs(readme): ...`.
- Write table-driven or `httptest`-backed tests for every bug fix and new behavior — test real HTTP/WS/SQLite interactions rather than stubbing away core logic.
- Do not commit scratch plans under `docs/superpowers/plans/` (git-ignored).
