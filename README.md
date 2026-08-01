# o365-log-extractor

Parses Office 365 and Microsoft Intune audit records and imports them into
[VictoriaLogs](https://docs.victoriametrics.com/victorialogs/).

Records can come from either source:

- **`api`** — pulls live from one or both feeds, selected with `-sources`:
  - `o365` — the [Office 365 Management Activity API](https://learn.microsoft.com/en-us/office/office-365-management-api/office-365-management-activity-api-reference),
    handling subscriptions, paging, throttling and resume-after-restart.
  - `intune` — [Intune audit events](https://learn.microsoft.com/en-us/graph/api/intune-auditing-auditevent-list)
    on Microsoft Graph (`/deviceManagement/auditEvents`), with `$filter` date
    windows and `@odata.nextLink` paging.
- **`file`** — parses an export from disk or stdin: a JSON array, JSON Lines,
  or a Purview compliance portal CSV, optionally gzipped.

Every path normalises records onto a
[common core schema](#the-common-core) and ships them to the VictoriaLogs
[JSON stream API](https://docs.victoriametrics.com/victorialogs/data-ingestion/#json-stream-api)
(`/insert/jsonline`) in gzipped batches, so one query works across both feeds.

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

Continuously pull both live audit feeds:

```sh
export O365_TENANT_ID=... O365_CLIENT_ID=... O365_CLIENT_SECRET=...
o365-log-extractor -mode api -follow -vl-url http://localhost:9428
```

Pull only one of them:

```sh
o365-log-extractor -mode api -sources intune -follow
```

Backfill a year of Intune history while keeping O365 at its default window:

```sh
o365-log-extractor -mode api -intune-lookback 8760h
```

## Azure AD setup for `api` mode

1. Register an application in Entra ID (Azure AD) and create a client secret.
2. Add the application permissions for the feeds you want:
   - `o365` — **Office 365 Management APIs** → `ActivityFeed.Read`
     (add `ActivityFeed.ReadDlp` for `DLP.All`).
   - `intune` — **Microsoft Graph** → `DeviceManagementApps.Read.All`.
3. Grant admin consent for the tenant.
4. Ensure unified audit logging is turned on for the tenant. Intune auditing
   is always on and cannot be disabled, but the tenant needs an active Intune
   licence for Graph to serve the feed.

Both feeds are enabled by default. If the app registration only has the
Office 365 permission, either add the Graph permission or run with
`-sources o365`; otherwise every poll logs a `403` for the Intune feed. A
failure on one feed never stops the other from being collected.

Subscriptions for the requested O365 content types are started automatically
on launch; pass `-auto-subscribe=false` to manage them yourself.

Credentials are read from `O365_TENANT_ID`, `O365_CLIENT_ID` and
`O365_CLIENT_SECRET` so they need not appear in the process table. The same
app registration is used for both feeds, with a separate token per API.

## How records are normalised

Each audit record becomes one flat JSON document.

### The common core

Office 365 and Intune use quite different schemas — Graph has no `Workload`,
`Operation` or `UserId`, and calls its timestamp `activityDateTime`. Every
record therefore gets the same small core of fields, so one query and one set
of `-vl-stream-fields` covers both feeds:

| Field | From an O365 record | From an Intune audit event |
| --- | --- | --- |
| `_time` | `CreationTime` | `activityDateTime` |
| `_msg` | summary line | summary line |
| `Workload` | native (`Exchange`, `SharePoint`, …) | always `MicrosoftIntune` |
| `RecordTypeName` | numeric `RecordType` decoded (`15` → `AzureActiveDirectoryStsLogon`) | `Intune` + `category` (`IntuneDeviceConfiguration`) |
| `Operation` | native | `activity`, else `displayName` |
| `UserId` | native | `actor.userPrincipalName`, else the service principal |
| `ResultStatus` | native | `activityResult` |
| `ClientIP` | native | `actor.ipAddress` |
| `UserTypeName` | numeric `UserType` decoded (`2` → `Admin`) | — |
| `source` | file name, or `management-activity-api` | `intune-graph-api` |

`_time` is RFC3339 UTC; an unparseable or missing timestamp falls back to
ingestion time and sets `_time_inferred`.

Only the fields above are synthesised. Every native Microsoft field name is
preserved alongside them, so records stay traceable back to the documented
schema and nothing is lost — an Intune record carries both `UserId` and
`actor.userPrincipalName`. On top of that:

- Nested objects are flattened to dotted paths — `SharePointMetaData.Site.Url`.
- `Parameters`, `ExtendedProperties` and `ModifiedProperties` are `{Name, Value}`
  arrays in the raw feed; they are expanded into real fields, so
  `Parameters.Identity` is directly queryable instead of buried in a JSON blob.
  `ModifiedProperties` also emits `<field>.old` for the previous value.
- Intune's `resources` collection is expanded by index, and the
  `{displayName, oldValue, newValue}` entries inside it become real fields:
  `resources.0.modifiedProperties.PasswordMinimumLength` with a matching
  `.old`. This is what makes "which setting changed, from what, to what"
  answerable in a query.
- Arrays of scalars become comma-separated strings; anything else is stored as
  JSON text.
- `null` fields and Graph's `@odata.*` type annotations are dropped.
- Purview CSV rows carry the real record as JSON in an `AuditData` column; that
  is unwrapped, with the surrounding columns kept where they don't collide.

Given this raw O365 record:

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

And an Intune audit event, abridged:

```json
{
  "id": "b2c3d4e5-…", "activityDateTime": "2026-07-30T09:14:03Z",
  "activity": "Patch DeviceConfiguration", "activityResult": "Success",
  "category": "DeviceConfiguration",
  "actor": {"userPrincipalName": "admin@example.com", "ipAddress": "203.0.113.44"},
  "resources": [{
    "displayName": "Windows 10 Baseline",
    "modifiedProperties": [
      {"displayName": "PasswordMinimumLength", "oldValue": "4", "newValue": "8"}
    ]
  }]
}
```

becomes:

```json
{
  "_time": "2026-07-30T09:14:03Z",
  "_msg": "Patch DeviceConfiguration user=admin@example.com workload=MicrosoftIntune result=Success component=DeviceConfiguration ip=203.0.113.44",
  "Workload": "MicrosoftIntune", "RecordTypeName": "IntuneDeviceConfiguration",
  "Operation": "Patch DeviceConfiguration", "UserId": "admin@example.com",
  "ResultStatus": "Success", "ClientIP": "203.0.113.44",
  "id": "b2c3d4e5-…", "activity": "Patch DeviceConfiguration",
  "activityResult": "Success", "category": "DeviceConfiguration",
  "actor.userPrincipalName": "admin@example.com", "actor.ipAddress": "203.0.113.44",
  "resources.0.displayName": "Windows 10 Baseline",
  "resources.0.modifiedProperties.PasswordMinimumLength": "8",
  "resources.0.modifiedProperties.PasswordMinimumLength.old": "4"
}
```

### Log streams

`-vl-stream-fields` defaults to `Workload,RecordTypeName` - both low
cardinality, which is what VictoriaLogs
[wants from stream fields](https://docs.victoriametrics.com/victorialogs/keyconcepts/#stream-fields).
Because Intune records synthesise both fields, they slot into the same stream
layout without any configuration change.

Do not add `UserId`, `ClientIP` or `Id`: high-cardinality stream fields will
degrade the database.

### Querying

Office 365:

```logsql
_time:1d Workload:="AzureActiveDirectory" Operation:="UserLoggedIn" ResultStatus:="Failed"
_time:7d Parameters.ForwardingSmtpAddress:*
_time:24h RecordTypeName:="ExchangeAdmin" | stats by (UserId) count() as ops | sort by (ops desc)
```

Intune:

```logsql
_time:24h Workload:="MicrosoftIntune" ResultStatus:="Fail"
_time:7d RecordTypeName:="IntuneRole"
_time:7d resources.0.modifiedProperties.PasswordMinimumLength:*
```

Across both feeds, which is the point of the common core:

```logsql
_time:24h UserId:="admin@example.com" | stats by (Workload, Operation) count() as n | sort by (n desc)
```

## Incremental state

In `api` mode, progress is written to `-state-file`
(default `o365-extractor.state.json`): a cursor per O365 content type and one
for Intune, plus the IDs of records already ingested. Restarts resume from the
cursor rather than re-importing, and the IDs guard against duplicates where
the windows overlap. Both feeds share one state file.

Because both APIs publish with a lag, each poll re-queries `-overlap`
(default 30m) behind the cursor and skips what it has already seen — O365 by
content blob ID, Intune by audit event ID. Blob entries are pruned once they
pass the API's 7-day retention; Intune event IDs only need to outlive the
overlap window, so they are pruned within the hour.

The cursor only advances after a batch has been flushed, so a crash mid-poll
re-reads that window instead of losing it. A feed that errors leaves its own
cursor untouched without affecting the other. Delete the state file to force a
full re-read of the lookback window.

## Flags

| Flag | Default | Purpose |
| --- | --- | --- |
| `-mode` | `api` | `api` or `file` |
| `-sources` | `o365,intune` | Live feeds to pull in api mode |
| `-cloud` | `commercial` | `commercial`, `gcc`, `gcchigh` or `dod` |
| `-tenant-id` / `-client-id` / `-client-secret` | env | Entra ID app credentials |
| `-content-types` | all five | Comma-separated O365 content types to pull |
| `-auto-subscribe` | `true` | Start O365 subscriptions that aren't enabled |
| `-lookback` | `24h` | How far back to read on first run (max `168h`) |
| `-intune-lookback` | `-lookback` | First-run window for Intune (max `17520h`) |
| `-graph-api-version` | `v1.0` | Graph version for Intune: `v1.0` or `beta` |
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
- The two feeds have very different retention, which is why they have separate
  lookback flags:
  - The Management Activity API accepts a maximum 24h query window and retains
    content for **7 days**; longer requests are split into 24h chunks and the
    start time is clamped to the retention window with a warning.
  - Graph serves roughly **two years** of Intune audit events.
- Feeds are polled independently. A permission problem or outage on one logs an
  error and leaves the other collecting; with `-follow` the failing feed is
  retried on the next poll. Without `-follow` the process exits non-zero after
  attempting every feed.
- Logs go to stderr, so `-dry-run` output on stdout stays pipeable.
- `SIGINT`/`SIGTERM` stops the current poll and exits cleanly.

## Layout

```
cmd/o365-log-extractor    CLI entry point and the per-feed poll loops
internal/audit            Record parsing, flattening and normalisation
internal/msapi            Shared Microsoft API plumbing: clouds, OAuth2, retries
internal/o365             Management Activity API client
internal/intune           Microsoft Graph client for Intune audit events
internal/victorialogs     Batching ingestion client
internal/state            Resume cursors and record de-duplication
internal/config           Flag and environment parsing
```
