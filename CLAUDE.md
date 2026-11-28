# RealityEngine_GO Guidance

Last reviewed: 2026-11-22

See `/Users/johnt/workspace/GitHub/CLAUDE.md` for the integrated application map. When the Go engines' role, ports, validation or parity responsibilities change, update this file and the root map together.

## Role

This repo is the Go Reality Engine (RE) and Perception Engine (PE). The target for `release-v0.0.3` is **first-class dFSA engines with the full RE and PE surface**:

- canonical machine loading and transitions;
- the per-machine output fold;
- the ISRE/OSRE step: merge batch, arbitration, retention, instance clock and histories;
- clones;
- K-lines and snapshots;
- the whole PE;
- all on the suite's TLS profile.

The plan is `RealityEngine_CI/docs/GO_ENGINE_ROADMAP.md`, phases G0–G6. The work is this repo's `release-v0.0.3` milestone, with #1 as the tracking issue.

Owner decisions, 2026-11-22:

- **A non-voting conformance follower** (`RealityEngine_CI/docs/QUORUM_CONTRACT.md` §1), like RealityEngine_Rust. Go is measured against what C++, LSP and Scala agree on and never votes. Shipping is not voting. Promotion to a voter is a separate change after the release, under the contract's promotion rule.
- **Followers must agree, and a follower that does not blocks the release** (RealityEngine_CI#842). On every parity stage of a certifying run, `go-1` must record `agrees`. A `differs`, an `unsupported` or an unavailable lane blocks `release-v0.0.3`. So a route left `unsupported` is a release blocker, not a resting state.
- **K-lines are in scope with full parity** (RealityEngine_CI#603, #287). The voters land them in v0.0.3, and Go must agree with what they land.
- **The agent gateway is A2A** (RealityEngine_CI#841). PE agent dispatch is an A2A client, not an OpenClaw ACP adapter.
- **No CI exception.** RealityEngine_Rust's self-validating hosted lane is the one exception in the engineering contract, and it does not extend here.
- **Regression follows Rust's path, in RealityEngine_CI** (owner, 2026-11-23). Rust's early self-contained CI migrated into RealityEngine_CI's `regression-test.sh`, and Go goes there from the start. This repository hosts no regression workflow: its hosted check is GitGuardian.

## Codebase Map

- `cmd/reality-engine`: the RE entrypoint. It reads `REALITY_ENGINE_PORT`.
- `cmd/perception-engine`: the PE entrypoint. It reads `PERCEPTION_ENGINE_PORT`.
- `internal/server`: the shared lifecycle (loopback listener, graceful drain on SIGINT/SIGTERM), each component's implemented handlers (today `GET /api/health` in the spec's shape `{status, timestamp, version}`), and a JSON 404 for anything the spec doesn't define.
- `internal/surface`: the route inventory of `SURFACE_SPEC.md` (`spec-routes.json`, embedded, digest-checked, same schema and `routesSha256` as Rust's), the router (literal segments beat `:param`s, so the spec's overlapping routes resolve, which `ServeMux` refuses), and the specified 501 `unsupported` envelope for every route not yet implemented. `Implemented` lists the routes served, and `groupIssues` names the issue for every other route.
- `cmd/refresh-surface`: regenerates `internal/surface/spec-routes.json` and `conformance/SURFACE_STATUS.md` from a committed `../RealityEngine_CI/SURFACE_SPEC.md`. `-check` reports drift and writes nothing. It is local maintenance, never a hosted step, because it reads a sibling checkout.
- `internal/lanetls`: the runtime TLS contract (`RE_TLS_CERT`, `RE_TLS_KEY`, `RE_TLS_CA`, optional `RE_TLS_CRL`). All three or none; a partial set refuses to start. The listener is TLS 1.3 with required client certificates and the CRL; the PE's outbound client presents the instance certificate and verifies its peer.
- `internal/engine/kline.go`, `kline_engine.go`: K-lines (RealityEngine_CI#603, #17), a port of C++ `kline.cpp`. `runPhases` hands each committed step to the tracker after arbitration retention and before the completion is published; closed K-lines (`re-kline/1`) and `sustain-end` events go, as JCS lines, to `<INSTANCE_KLINE_DIR>/<uuid>/` (default the clock dir's sibling `klines/`), fsynced, with the open table in `open.json`; a minted UUID keeps them in memory. Controls `klineCapture` (default `false`; `RE_KLINE_CAPTURE=1` at boot), `klineFeedbackHops`, `klineStoreMaxBytes`; routes `/api/klines`, `/open`, `/stats`, `/:id`, `POST /api/klines/export`; budget warnings on `/api/health`. The PE's ISRE composition is still to come (#17).
- `internal/engine/snapshot.go`: runtime snapshots (RealityEngine_CI#287, #17), a port of C++ `snapshot.cpp`: retention of every retained step's active list, contributions and clock; the five `/api/runtime/snapshot*` and `/restore` routes (`re-snapshot/1`, id = digest hex, JCS without the clocks' instance and lamport); `RE_SNAPSHOT_STORE` `memory` (default), `file:<dir>` or `off` (503). The corpus fingerprint hashes `Machine.Declared`, the declared form without `id`.
- `go.mod`, `.go-version`: Go 1.27.3, the one pin for every lane. There are no third-party dependencies. Adding one is a reviewed decision, not a convenience.

Today the commands serve health, and answer every other specified route `unsupported` (Go#7). Implementing a route means adding its handler and its id to `surface.Implemented`, then regenerating the status. Extend this map as packages land.

## Build and validation

**Builds are controlled through RealityEngine_CI** (`RealityEngine_CI/docs/BUILD_CONTROL_CONTRACT.md`). Registering this repo there is RealityEngine_CI#836. A local `go build` is for working on this repo alone, never the last step before a parity claim.

Local checks, run before every PR:

```bash
gofmt -l .            # must print nothing
go vet ./...
go test -race ./...
go build ./...
```

What runs where:

- **Hosted checks here:** GitGuardian only. There is no hosted regression workflow in this repository (owner, 2026-11-23).
- **Build and regression** run from RealityEngine_CI, as Rust's do. `regression-test.sh --execute --follower rust,go` builds `bin/reality-engine` and `bin/perception-engine` with the pinned Go (`scripts/lib/go-toolchain.sh`, `GOTOOLCHAIN=local`) and checks their provenance. It then holds `go-1` against the voters in the follower lane, `scripts/follower-conformance.sh --follower go` (RealityEngine_CI#836, #837, PR #849).
- **Corpus replay and the surface probe** (Go#9, Go#7) are tools run locally and from RealityEngine_CI's lanes, never hosted jobs.
- On a TLS lane (`RE_TLS=on`), each `go-1` listener serves TLS 1.3 + mTLS on a loopback port of its own (`internal/lanetls`), behind RealityEngine_CI's `scripts/lib/tls_front.py` on the public port, which holds the profile's suites (Go#6). It is never run in plaintext there.
- **A green local or hosted run is not a parity claim.** It says this checkout builds and its tests pass. Report a check that could not run as unavailable, never as a pass.

## Ports

- Allocate with `--free-ports` (`RE_FREE_PORTS=true`). The OS assigns the ports, and the instance registry records them, so no two instances can collide.
- **Where a fixed port is unavoidable, 5850 (PE) and 5851 (RE) are reserved for Go.** That covers a docker or release port mapping, or an operator pin. These sit off the voters' `allocate-ports.sh` grid.
- Never hard-code either in a client. Resolve endpoints from the instance registry (`RE_REGISTRY_URL`).

## Rules that bite

- **The contract and the voters' agreement are the oracle. No engine is.** RealityEngine_Rust is prior art for structure, not a reference result. When the spec and the voters differ, or the voters split 2–1, report it for an owner ruling. Never choose a side to make a test pass.
- **Canonical keys only:** `events`, `outputEvents`, `nextEventIds`, `inputEvent`, `eventDimension`. The domain term is Reality Event.
- **An unimplemented specified route answers 501** with `{error, participation: "unsupported", surface}` (SURFACE_SPEC "An unimplemented route answers `unsupported`"). An unknown route answers 404. Never return a success-shaped stub.
- **Numbers leave in ECMAScript form,** as the voters write them (`1`, not `1.0`). `encoding/json` formats `float64` that way. Don't format numbers by hand, and compare wire bytes in encoding tests.
- **Parallel where the spec says parallel, deterministic where it says compared.** `POST /api/engine/process` maps across machines concurrently and joins before OSRE(n) resolves. Canonical orderings (machine lists, graph edges, arbitration records) never depend on goroutine scheduling or map iteration order. Run `go test -race` on anything concurrent.
- **Streams don't leak.** Every SSE and WebSocket handler ends its goroutines on disconnect, and tests check it.
- **Instance identity:** `INSTANCE_UUID` and `INSTANCE_CLOCK_DIR`, the Lamport clock `{instance, lamport, step}`, and an exclusive lock for the instance's life. The lock must be a POSIX record lock (`fcntl`, `F_SETLK`), not `flock`, because `instance_uuids.py live` probes it with `F_GETLK`.
- **TLS:** `crypto/tls` cannot restrict TLS 1.3 cipher suites (`Config.CipherSuites` governs TLS 1.0–1.2 only), so a Go listener alone does not meet `config/transport-profile.json`. Follow the owner-approved pattern for the suite's Go services (`RealityEngine_CI/docs/TRANSPORT_SECURITY.md`, 2026-11-12): listen on loopback in the namespace, with an OpenSSL front serving the profile with mTLS. Never disable verification. Every new link goes in `config/transport-inventory.json` in the same change (Go#6).

## Runtime Contract

The HTTP contract is `RealityEngine_CI/SURFACE_SPEC.md`. Quorum is `RealityEngine_CI/docs/QUORUM_CONTRACT.md`. Arbitration is `RealityEngine_CI/docs/ARBITER_CONTRACT.md`. Environment names and ports are `RealityEngine_CI/DEPLOYMENT_CONTRACT.md`. Transport is `RealityEngine_CI/docs/TRANSPORT_SECURITY.md`. Follow those, not this file, when they differ. `AGENTS.md` carries the same contract map for other agents.

## Standing rules

The standing rules are in
`../RealityEngine_CI/docs/ENGINEERING_CONTRACT.md`, which is
authoritative. They are not listed or summarised here, so this file never
falls behind the contract. Read it before changing anything here.
