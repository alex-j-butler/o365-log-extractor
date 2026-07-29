# o365-log-extractor

Parses Office 365 unified audit records and imports them into
[VictoriaLogs](https://docs.victoriametrics.com/victorialogs/).

Records can come from either source:

- **`api`** — pulls live from the [Office 365 Management Activity API](https://learn.microsoft.com/en-us/office/office-365-management-api/office-365-management-activity-api-reference),
  handling subscriptions, paging, throttling and resume-after-restart.
- **`file`** — parses an export from disk or stdin: a JSON array, JSON Lines,
  or a Purview compliance portal CSV, optionally gzipped.

Both paths normalise records the same way and ship them to the VictoriaLogs
[JSON stream API](https://docs.victoriametrics.com/victorialogs/data-ingestion/#json-stream-api)
(`/insert/jsonline`) in gzipped batches.

Go 1.26, standard library only — no third-party dependencies.

## Build

```sh
make build            # -> bin/o365-log-extractor
make test
```

Or directly: `go build -o bin/o365-log-extractor ./cmd/o365-log-extractor`

## Docker

Images are published to GitHub Container Registry for `linux/amd64` and
`linux/arm64`:

```sh
docker run --rm \
  -e O365_TENANT_ID -e O365_CLIENT_ID -e O365_CLIENT_SECRET \
  -v o365-state:/data \
  ghcr.io/alex-j-butler/o365-log-extractor:latest \
  -mode api -follow -vl-url http://victorialogs:9428
```

Everything after the image name is passed straight to the binary, so every
flag in the table below works unchanged.

- **Mount a volume at `/data` in `api` mode.** The working directory is
  `/data`, so the default `-state-file` lands there. Without a volume the
  cursor lives in the container's writable layer and is lost on recreate, and
  every restart re-reads the whole `-lookback` window.
- **Always override `-vl-url`.** The default `http://localhost:9428` resolves
  to the container itself.
- **Pass credentials as environment variables, not flags** — the same reason
  they are kept out of the process table applies to `docker inspect`.
- The image runs as UID `65532` and contains no shell. A named volume picks up
  the right ownership automatically; a bind mount (`-v ./state:/data`) needs
  `chown 65532:65532` on the host directory first.

One-shot parsing of an export works the same way, mounting the file in:

```sh
docker run --rm -v "$PWD/audit-export.csv:/data/audit-export.csv:ro" \
  ghcr.io/alex-j-butler/o365-log-extractor:latest \
  -mode file -dry-run audit-export.csv
```

Build it locally with `make docker`.

## Quick start

Parse an export and print what *would* be ingested:

```sh
o365-log-extractor -mode file -dry-run audit-export.csv
```

Import that export into a local VictoriaLogs:

```sh
o365-log-extractor -mode file -vl-url http://localhost:9428 audit-export.csv
```

Continuously pull the live audit feed:

```sh
export O365_TENANT_ID=... O365_CLIENT_ID=... O365_CLIENT_SECRET=...
o365-log-extractor -mode api -follow -vl-url http://localhost:9428
```

## Azure AD setup for `api` mode

1. Register an application in Entra ID (Azure AD) and create a client secret.
2. Add the **Office 365 Management APIs** application permission
   `ActivityFeed.Read` (add `ActivityFeed.ReadDlp` if you want `DLP.All`).
3. Grant admin consent for the tenant.
4. Ensure unified audit logging is turned on for the tenant.

Subscriptions for the requested content types are started automatically on
launch; pass `-auto-subscribe=false` to manage them yourself.

Credentials are read from `O365_TENANT_ID`, `O365_CLIENT_ID` and
`O365_CLIENT_SECRET` so they need not appear in the process table.

## How records are normalised

Each audit record becomes one flat JSON document:

| Field | Source |
| --- | --- |
| `_time` | `CreationTime`, converted to RFC3339 UTC. Unparseable or missing values fall back to ingestion time and set `_time_inferred`. |
| `_msg` | A summary line: `Set-Mailbox user=admin@example.com workload=Exchange result=True` |
| `RecordTypeName` | Symbolic name for the numeric `RecordType` (e.g. `15` → `AzureActiveDirectoryStsLogon`). |
| `UserTypeName` | Symbolic name for the numeric `UserType` (e.g. `2` → `Admin`). |
| `source` | The originating file name, or `management-activity-api`. |

All original Microsoft field names are preserved, so records stay traceable
back to the documented schema. On top of that:

- Nested objects are flattened to dotted paths — `SharePointMetaData.Site.Url`.
- `Parameters`, `ExtendedProperties` and `ModifiedProperties` are `{Name, Value}`
  arrays in the raw feed; they are expanded into real fields, so
  `Parameters.Identity` is directly queryable instead of buried in a JSON blob.
  `ModifiedProperties` also emits `<field>.old` for the previous value.
- Arrays of scalars become comma-separated strings; anything else is stored as
  JSON text.
- `null` fields are dropped.
- Purview CSV rows carry the real record as JSON in an `AuditData` column; that
  is unwrapped, with the surrounding columns kept where they don't collide.

Given this raw record:

```json
{
  "CreationTime": "2026-07-28T05:01:00", "RecordType": 1, "UserType": 2,
  "Operation": "Set-Mailbox", "Workload": "Exchange", "ResultStatus": "True",
  "UserId": "admin@example.com",
  "Parameters": [
    {"Name": "Identity", "Value": "alex@example.com"},
    {"Name": "ForwardingSmtpAddress", "Value": "smtp:attacker@evil.example"}
  ]
}
```

the ingested document is:

```json
{
  "_time": "2026-07-28T05:01:00Z",
  "_msg": "Set-Mailbox user=admin@example.com workload=Exchange result=True",
  "CreationTime": "2026-07-28T05:01:00", "RecordType": 1, "RecordTypeName": "ExchangeAdmin",
  "UserType": 2, "UserTypeName": "Admin", "Operation": "Set-Mailbox",
  "Workload": "Exchange", "ResultStatus": "True", "UserId": "admin@example.com",
  "Parameters.Identity": "alex@example.com",
  "Parameters.ForwardingSmtpAddress": "smtp:attacker@evil.example"
}
```

### Log streams

`-vl-stream-fields` defaults to `Workload,RecordTypeName` - both low
cardinality, which is what VictoriaLogs
[wants from stream fields](https://docs.victoriametrics.com/victorialogs/keyconcepts/#stream-fields).
Do not add `UserId`, `ClientIP` or `Id`: high-cardinality stream fields will
degrade the database.

### Querying

```logsql
_time:1d Workload:="AzureActiveDirectory" Operation:="UserLoggedIn" ResultStatus:="Failed"
_time:7d Parameters.ForwardingSmtpAddress:*
_time:24h RecordTypeName:="ExchangeAdmin" | stats by (UserId) count() as ops | sort by (ops desc)
```

## Incremental state

In `api` mode, progress is written to `-state-file`
(default `o365-extractor.state.json`): a per-content-type cursor plus the IDs
of content blobs already ingested. Restarts resume from the cursor rather than
re-importing, and the blob IDs guard against duplicates where the windows
overlap.

Because the API publishes blobs with a lag, each poll re-queries `-overlap`
(default 30m) behind the cursor; blobs already seen are skipped. Entries are
pruned once they pass the API's 7-day retention.

The cursor only advances after a batch has been flushed, so a crash mid-poll
re-reads that window instead of losing it. Delete the state file to force a
full re-read of the retention window.

## Flags

| Flag | Default | Purpose |
| --- | --- | --- |
| `-mode` | `api` | `api` or `file` |
| `-cloud` | `commercial` | `commercial`, `gcc`, `gcchigh` or `dod` |
| `-tenant-id` / `-client-id` / `-client-secret` | env | Entra ID app credentials |
| `-content-types` | all five | Comma-separated feeds to pull |
| `-auto-subscribe` | `true` | Start subscriptions that aren't enabled |
| `-lookback` | `24h` | How far back to read on first run (max `168h`) |
| `-overlap` | `30m` | Re-query window behind the cursor |
| `-follow` | `false` | Keep polling instead of exiting after one pass |
| `-poll-interval` | `5m` | Interval between polls when following |
| `-state-file` | `o365-extractor.state.json` | Progress file; empty disables |
| `-vl-url` | `http://localhost:9428` | VictoriaLogs base URL |
| `-vl-stream-fields` | `Workload,RecordTypeName` | Log stream fields |
| `-vl-extra-field` | — | `key=value` added to every record (repeatable) |
| `-vl-header` | — | Extra HTTP header `key=value` (repeatable) |
| `-vl-username` / `-vl-password` / `-vl-bearer-token` | env | Auth for a proxied VictoriaLogs |
| `-vl-account-id` / `-vl-project-id` | — | Tenant headers for cluster VictoriaLogs |
| `-vl-gzip` | `true` | Compress ingestion requests |
| `-batch-records` / `-batch-bytes` | `1000` / `4MiB` | Flush thresholds |
| `-max-retries` | `4` | Retries for failed HTTP requests |
| `-dry-run` | `false` | Print JSON lines to stdout instead of ingesting |
| `-log-level` / `-log-json` | `info` / `false` | Logging |

Run `o365-log-extractor -h` for the full list.

## Operational notes

- Throttled (`429`) and transient (`5xx`) API responses are retried with
  exponential backoff, honouring `Retry-After`. Malformed-request responses
  from VictoriaLogs (`4xx`) fail fast rather than retrying pointlessly.
- The Management Activity API accepts a maximum 24h query window and retains
  content for 7 days; longer requests are split into 24h chunks and the start
  time is clamped to the retention window.
- Logs go to stderr, so `-dry-run` output on stdout stays pipeable.
- `SIGINT`/`SIGTERM` stops the current poll and exits cleanly.

## Layout

```
cmd/o365-log-extractor    CLI entry point and the two run loops
internal/audit            Record parsing, flattening and normalisation
internal/o365             Management Activity API client and OAuth2
internal/victorialogs     Batching ingestion client
internal/state            Resume cursors and blob de-duplication
internal/config           Flag and environment parsing
```
