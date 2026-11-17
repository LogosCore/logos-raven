# logos-raven

Raven, a Logos factory that builds small PowerShell scripts: minions that
check in through the HTTP channel (`reverse-call`) and do two things, `hello`
and `echo`. It exists to run core's **codec** mechanics end to end (docs:
[Codecs](https://logoscore.github.io/logos-docs/contracts/minion-messages/codecs/)):
its minions cannot speak core's codec, so Raven translates both layers for
core over the factory link.

- **Envelope `raven.clixml`**: a message is PowerShell's own CLIXML, what
  `[System.Management.Automation.PSSerializer]` writes and reads, so the
  script needs no encoder of its own. The protocol's maps are Hashtables
  (numbered keys `Int32`, `Int64` past it), byte strings `byte[]`, arrays
  `Object[]`. Raven reads it as CBOR (the protocol's form) and writes core's
  CBOR as it, typed both ways, so nothing is guessed.
- **Suite `raven.raven`**: a lie to core. It seals nothing: Raven makes each
  build's keys when core asks, at the build's offer (32 random bytes), and
  never uses them; core sends them back with every message as the
  candidates, and the first one "opens" it. The script sends its CLIXML in
  the clear. **It protects nothing**; it is a test of the mechanics, not a
  minion to deploy.
- No staging (Raven refuses a stage's keys: its minions say hello under
  their build id), no compression, no persisted sessions, no max tasks.

## Build schema

| | |
|---|---|
| Channel | `http` version 3, kind `reverse-call`; Raven's own fields `method` (POST, PUT) and `path` (`/`) |
| Codec | crypto `raven.raven`, envelope `raven.clixml`, `codec` growth 400 %, overhead 4096 B, timeout 5s. CLIXML is some 20 times CBOR on a small message, a third more on bytes: core sends fewer items when a message would outgrow the bound |
| Field | `greeting` ("hello from Raven") |
| Target | `ps1`, a `script` for `windows/any`, `linux/any`, `darwin/any` |
| Commands | `hello` (op 1): the greeting; `echo` (op 2, param `text`): its text |

## The script

One `.ps1` (`cmd/raven-factory/raven.ps1`, a template the factory fills in)
for Windows PowerShell 5.1 and PowerShell 7. Run it with `-Trace` to see its
exchanges, `-Seconds N` to stop after N seconds.

- It speaks the HTTP channel's wire format: `offset(1) ‖ part 1 ‖ id(16) ‖
  part 2` around the CLIXML, through the build's connections in order
  (`base_url`, then `extra_base_urls`; a failure moves to the next
  connection), with the endpoint's `user_agent`, `headers`,
  `request_timeout`, `checkin_interval` and `checkin_jitter`, and
  `tls_verify` (off: any certificate, through a compiled callback).
  `host_header`, `sni`, `proxy` and `tls_pin_sha256` are not used.
- `hello` under the build id first (its machine id: Windows's MachineGuid,
  Linux's `/etc/machine-id`, else the host name), then `get_tasks` (4) every
  check-in under the session id. A task runs at once; its ack and result go in
  the next message, again until an exchange goes through (a reply, or core's
  empty one). It answers `rehello`, `sleep_update` and `poll`, and starts over
  on `err` `session_closed` or `session_replaced`.

## Run with logos-core's dev stack

Start logos-core and the HTTP channel, put `RABBITMQ_PASSWORD` (core's
`RABBITMQ_DEFAULT_PASS`) in `.env`, then `docker compose up -d --build`. The
factory reaches core's own address for its factory link and its builds'
uploads (`LOGOS_CORE_BASE_URL`, `http://logos-core-dev:8002` on the
`logos-core_logos` network), or core's front over HTTPS (the `logos-nginx`
image passes the link's WebSocket and the uploads).

Environment: `FACTORY_ID` (`raven-local`), `LOGOS_CORE_BASE_URL` (and, over
HTTPS to a self-signed core, `LOGOS_CORE_CA_FILE`, `LOGOS_CORE_CERT_SHA256` or
`LOGOS_CORE_TLS_INSECURE`: [How modules reach core](https://logoscore.github.io/logos-docs/core-infrastructure/#how-modules-reach-core)),
`RABBITMQ_HOST` / `RABBITMQ_PORT` / `RABBITMQ_USER` / `RABBITMQ_PASSWORD` /
`RABBITMQ_VHOST` (`localhost` / `5672` / `logos` / required: logos-core's
`RABBITMQ_DEFAULT_PASS` / `/`), `LOG_FORMAT` (`json` or `text`).

The image, `ghcr.io/logoscore/logos-raven`, published on each `v*` tag
(`docker-image.yml`, `linux/amd64` and `linux/arm64`), runs
`/app/raven-factory`.

### Trying it end to end

Build a minion with a connection through the HTTP channel and run the
script: Raven makes the build's keys at the offer, the session opens under
the build id, and `hello` and `echo` come back exactly (text with XML's
special characters, a literal `_x000A_`, a newline or an emoji included).
Stop Raven: the minion's messages get the channel's 404 and the session
shows core's Codec notice (`codecDownSince`), cleared once Raven is back.
Under Windows PowerShell 5.1, on Windows, the script runs the same.

### Against an unreleased protocol or SDK

The image builds from tagged modules alone (`.dockerignore` keeps a
workspace out). With a local `go.work` (`use .` and
`../logos-golang-protocol`, checkouts side by side), run a binary built on
the host, mounted over the image's:

```bash
CGO_ENABLED=0 GOOS=linux go build -o .dev-bin/raven-factory ./cmd/raven-factory
```

```yaml
# docker-compose.override.yml
services:
  raven-factory:
    volumes:
      - ./.dev-bin/raven-factory:/app/raven-factory:ro
```

Then `docker compose up -d`, without `--build`; after each change, build it
again and `docker compose restart`. `go.work`, the override and `.dev-bin/`
are gitignored and never committed (the docs' [Changing the
Protocol](https://logoscore.github.io/logos-docs/contributing/changing-the-protocol/)).

## Tests

```bash
go test ./...
golangci-lint run --max-issues-per-linter 0 --max-same-issues 0 ./...
```

`script_test.go` runs a rendered script under PowerShell against a test server
that plays the HTTP channel and core, with Raven's codec as core would call
it: `pwsh` on `PATH`, or `RAVEN_PWSH` naming the command (for example a
wrapper around `docker run --rm -i --network host -v "$TMPDIR:$TMPDIR"
mcr.microsoft.com/powershell pwsh`: the tests write the script to the
system's temporary folder). Without either it skips, unless
`RAVEN_REQUIRE_PWSH` is set, as CI sets it (ubuntu-latest has `pwsh`).

CI (`.github/workflows/ci.yml`) runs the tests under PowerShell 7 on Linux
and under Windows PowerShell 5.1 on Windows (`test-windows`), and
golangci-lint v2.13.2 (its default linters, kept at 0 issues). The Windows
job runs only on GitHub: a release pushes `main` first and is tagged once
its CI has passed.
