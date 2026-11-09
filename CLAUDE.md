# CLAUDE.md

`logos-raven` (`github.com/logoscore/logos-raven`): Raven, a Logos factory that builds PowerShell scripts (Windows PowerShell 5.1, PowerShell 7), minions that check in through the HTTP channel (`reverse-call`) and do `hello` and `echo`. A test of core's codec mechanics (docs: contracts/minion-messages/codecs, "Raven: the test factory"): envelope `raven.clixml` (PowerShell's CLIXML), suite `raven.raven` (seals nothing: a lie to core, it protects nothing). README.md has the schema, the script's behaviour and the dev stack.

## Commands

```bash
go build ./... && go vet ./... && go test ./...
golangci-lint run --max-issues-per-linter 0 --max-same-issues 0 ./...
docker compose up -d --build
```

The script tests need PowerShell: `pwsh` on `PATH`, or `RAVEN_PWSH` naming the command (locally, a wrapper running `mcr.microsoft.com/powershell` with `--network host` and the system's temporary folder mounted at its own path); CI runs them under PowerShell 7 on Linux and Windows PowerShell 5.1 on Windows (`test-windows`); they skip without it, and fail instead with `RAVEN_REQUIRE_PWSH=1` (CI). CI (`.github/workflows/ci.yml`) runs the tests and golangci-lint v2.13.2 with default linters, no config: keep 0 issues. `docker-image.yml` publishes `ghcr.io/logoscore/logos-raven` on `v*` tags.

## `cmd/raven-factory`

- `main.go`: factory-core `factory.Run` with the schema, the builder, management, `Codec: ravenCodec{}` and `CoreURL` (`LOGOS_CORE_BASE_URL`: core's own address, which the factory link opens on; nginx does not pass the upgrade). Embeds `icon.svg` (dark themes: the whole raven, filled) and `icon-light.svg` (light themes: the logo's navy body, its highlights left out), traced from Raven's logo without its PowerShell tile; a test validates both (`protocol.ValidateIcon`).
- `build.go`: the build schema (version 1: channel `http` v3 `reverse-call` with `method`, `path`; crypto `raven.raven`, envelope `raven.clixml`, `codec` growth 400 / overhead 4096 / timeout 5s; field `greeting`; target `ps1`; commands `hello` op 1, `echo` op 2 with `text`) and the builder (steps render, upload). `render` fills `raven.ps1`'s placeholders (`__BUILD_ID__`, `__VERSION__`, `__GREETING__`, `__OPS__`, `__CONNS__`: each connection a hashtable literal), every value a single-quoted PowerShell string (`psQuote`: quotes doubled, the typographic ones too). The script starts with UTF-8's byte order mark: Windows PowerShell 5.1 reads a script without one as the ANSI code page. `.gitattributes` keeps LF on Windows checkouts.
- `codec.go`: `ravenCodec`, factory-core's `codeclink.Codec`: inbound reads CLIXML as CBOR (the suite opens with the first key), outbound writes CBOR as CLIXML, keys are 32 random bytes for a build, a stage `Refused`.
- `clixml.go`: CLIXML ⇄ CBOR. Reading: `Obj` with `DCT` (a map), `LST`/`IE`/`STK`/`QUE` (an array), `MS`/`Props` (text-keyed members), `ToString`; `Ref` resolves an earlier `RefId`; primitives `S`, `B`, the integers, `Db`/`Sg`/`D`, `BA`, `Nil`, `C`; `_xHHHH_` escapes (UTF-16, pairs combined); 32 levels at most; a key twice refused. Writing: Hashtables (`TN` once, then `TNRef`), `Object[]`, `I32`/`I64`/`U64` by size, `S` escaped as PowerShell does (control characters and `_x`), keys sorted (numbers, then text).
- `raven.ps1`: the minion. `HttpWebRequest` (5.1 and 7), the HTTP channel's body (`offset ‖ part 1 ‖ id ‖ part 2`), `PSSerializer` both ways; hello under the build id, then `get_tasks` under the session id; pending acks and results go again until an exchange goes through (a sealed reply or core's empty one; a 404 or no answer resends); `rehello`, `sleep_update`, `poll`; `err` 5 or 6 starts over. `-Trace`, `-Seconds`. `$OpHello`, `$OpEcho` must match the schema's ops (a test checks).
- Tests: `clixml_test.go` (PowerShell-written CLIXML read, every item core and the minion exchange round-tripped, escapes, refusals), `build_test.go` (schema, codec, rendering through factorytest, refusals), `script_test.go` (the script under PowerShell against a fake channel and core: hello, tasks, a 404 resent, an empty reply taken, rehello, a closed session; HTTPS with `tls_verify` off).

## Conventions

- Raven is a test of the mechanics: never make `raven.raven` look like protection, and say so where a minion is described.
- A change to what the script sends or reads is a change to the minion: run the script tests under PowerShell.
- The suite's keys never go into the script, logs or progress.
