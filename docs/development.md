# Development

## Requirements

Go 1.22 or newer. There are no external dependencies — `go.mod` has no `require` block, so the project builds completely offline.

## Common commands

```sh
make            # vet + test + build
make test
make fuzz       # fuzz the scan_results parser for 60s
make cover
make arm64      # cross-compile; amd64 and armv7 are also available
make deb-arm64  # wrap the binaries into a .deb; needs dpkg-deb
```

On Windows (PowerShell):

```powershell
$env:GOOS="linux"; $env:GOARCH="arm64"; $env:CGO_ENABLED="0"
go build -ldflags "-s -w" -o dist/netcfgd ./cmd/netcfgd
go build -ldflags "-s -w" -o dist/netcfg-web ./cmd/netcfg-web
```

## Layering rules

These are enforced in review:

- `internal/domain` imports only the standard library and `internal/kdf`. No I/O, no `os/exec`, no `net/http`.
- `internal/app` depends only on `internal/ports` and `internal/domain`, never on a concrete adapter.
- `internal/adapters/*` implement the interfaces in `ports` and are the only place allowed to run external commands or touch the filesystem.
- `internal/httpapi` must not import an adapter; it talks to the agent through `internal/rpc`.

## Testing strategy

| Layer | Approach |
|---|---|
| `domain` | Pure table-driven tests, no I/O |
| `app` | Fakes for every port, plus `clock.Fake` to exercise rollback timers instantly |
| Config generation | Golden files for `.network` and `interfaces.d` |
| Parsers | Fuzzing for `scan_results` — untrusted input from the RF environment |
| `platform/auth` | Session lifecycle, persistence, no token leakage |
| `platform/i18n` | Language resolution, format verb consistency, and three sweeps requiring a translation for every string in Go sources, browser `.js` and `.html` templates |

The important part: **commit-confirm tests run instantly** instead of waiting 90 real seconds, because `ports.Clock` is injected:

```go
h.clock.Advance(91 * time.Second)   // the rollback timer fires now
```

Existing tests:

```
internal/domain            IPPlan normalization, diffing, dual stack, IEEE PSK vector, Secret, HotspotConfig
internal/app               6 commit-confirm scenarios plus reconciliation, and the radio guard while the AP holds it
internal/adapters/wpactrl  scan_results parsing plus fuzzing, and session recovery when the supplicant restarts
internal/adapters/sysinfo  sensor discovery against a fake /sys tree, not a hardcoded list
internal/adapters/sshctl   ufw status parsing, including the traps in its wording
internal/adapters/wifibackend  per-link backend routing and the diagnosis when nobody claims a link
internal/adapters/...      golden files for networkd and ifupdown, config injection guards
internal/platform/auth     sessions across restarts, revocation, expiry, credential lifecycle
```

The `wpactrl` session tests need real unix domain sockets, so they skip on
Windows. Cross-compile and run them where they mean something:

```sh
GOOS=linux GOARCH=amd64 go test -c -o /tmp/wt ./internal/adapters/wpactrl
```

## Running locally

On a Linux machine, without installing anything system wide:

```sh
mkdir -p /tmp/netcfg
go run ./cmd/netcfgd -socket /tmp/netcfg/agent.sock -state-dir /tmp/netcfg/state \
    -ap-fallback=false -log-level debug &

NETCFG_PASSWORD=long-enough-password go run ./cmd/netcfg-web -set-password -config /tmp/netcfg/config.json
go run ./cmd/netcfg-web -listen 127.0.0.1:8090 -agent-socket /tmp/netcfg/agent.sock \
    -config /tmp/netcfg/config.json -state-dir /tmp/netcfg
```

Without `wpa_supplicant` or an IP backend the interface still loads and explains exactly which subsystem is unavailable — that is intended behaviour, not a bug.

On Windows, `netcfgd` still runs thanks to AF_UNIX support on Windows 10+, which is enough to exercise the HTTP and RPC layers. The `SO_PEERCRED` check is skipped with a loud warning, because it only exists on Linux.

## Adding an IP backend

1. Implement `ports.IPBackend` under `internal/adapters/ipbackend/<name>/`.
2. `Detect` must return an error when the backend is absent, and an empty slice when present but managing nothing.
3. `Apply` must call `plan.Normalize()` first — that is where all safety validation happens.
4. Write golden tests covering IPv4, IPv6 and dual stack.
5. Register it in `ipbackend.NewRegistry` inside `cmd/netcfgd/main.go` at the right priority.

## Adding a Wi-Fi backend

1. Implement `ports.WiFiBackend` under `internal/adapters/wifibackend/<name>/`, and add `var _ ports.WiFiBackend = (*Adapter)(nil)` so drift breaks the build.
2. `Detect` must return the wireless links it drives, or an error saying why it stepped aside — that error is concatenated into the message the operator sees when nothing claims a link.
3. `Kind` returns a `domain.WiFiBackendKind`; the type is a free-form string, so name yourself without editing shared files.
4. Test the parsing in isolation. Shelling out is not injectable, so pure helpers carry the tests.
5. Register it in `wifibackend.New` inside `cmd/netcfgd/main.go` at the right priority.

## Releases

`.github/workflows/release.yml` runs on a `v*` tag push and builds all three architectures, then publishes them.

The same workflow runs by hand from the Actions tab:

| Input | Effect |
|---|---|
| `arch` | `all`, or one of `linux-amd64` / `linux-arm64` / `linux-armv7` |
| `publish` | `artefacts` (default), `rolling`, or `release` |
| `tag` | read only by `release`; the tag must already exist |

`rolling` replaces a prerelease called **latest**, deleting the old one and its
tag so the archives cannot end up attached to a stale commit. It needs no tag
input and is the mode to use for a build you just want to put on a device.

Because a rolling archive carries the same filename every time, each one contains
a `VERSION` file with the tag, the commit and the build time — otherwise a device
in the field could not be traced back to a commit.

`release` refuses to run without an existing tag, and fails in the first job
rather than after the builds. A single-architecture publish is allowed and
produces an archive set covering only that architecture.

## Conventions

- All identifiers, comments, log messages and user facing strings are in English.
- User facing English text is also the i18n catalog key. Add the Vietnamese translation to `internal/platform/i18n/locales/vi.json` in the same commit; `TestEverySourceStringIsTranslated` fails otherwise.
- Anything an operator can read must travel as a `domain.Message`, never as a pre-formatted string, so the web tier can still translate it.
- Format verbs inside a `Message` must be `%s`, `%q` or `%v`: arguments are stringified so they survive the JSON round trip to the browser.
- Return `domain.Error` with a classification code; do not use bare `errors.New` at layer boundaries.
- Secrets always travel as `domain.Secret`, never as `string`.
- Comments explain only what the code cannot show on its own; they never restate the next line.
- `gofmt -l .` must be clean; `make vet` enforces it.

## Open work

- VLAN, bridge and bonding.
- Multiple addresses per family.
- Multiple users and role based access control.
- `.deb` packaging with nfpm, SBOM generation, cosign signing.
- Integration tests in a Debian container with a fake `wpa_supplicant`.
