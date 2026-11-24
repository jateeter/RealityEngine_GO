# RealityEngine_GO

The Go implementation of the Reality Engine (RE) and Perception Engine (PE).
This repository is at the bootstrap stage: the two commands serve only
`GET /api/health`. They do not yet load machines, execute steps, or implement
the RE/PE contract. Health means the bootstrap HTTP server is running, not
that an engine is ready for use.

## Requirements

Go 1.27.3 is the development toolchain pinned in `.go-version` and the minimum
in `go.mod`. With `GOTOOLCHAIN=auto`, Go can select or download a compatible
toolchain; it may use a newer one. Use `go version` to check the active
toolchain. There are no third-party dependencies.

## Build and run locally

Choose two free, distinct ports. These examples are local choices, **not**
assigned suite ports. Each process requires its own environment variable:

```sh
go build -o /tmp/reality-engine-go ./cmd/reality-engine
go build -o /tmp/perception-engine-go ./cmd/perception-engine

REALITY_ENGINE_PORT=18081 /tmp/reality-engine-go
# In a second terminal:
PERCEPTION_ENGINE_PORT=18080 /tmp/perception-engine-go
# In a third terminal:
curl http://127.0.0.1:18081/api/health
curl http://127.0.0.1:18080/api/health
```

Ports must be decimal integers from 1 through 65535. Missing, invalid, or
already-bound ports are errors. Stop either process with Ctrl-C or SIGTERM;
it stops accepting connections and drains in-flight requests before exiting.

Both services bind only to `127.0.0.1`. Without `RE_TLS_*` they serve plaintext
HTTP for local development. With `RE_TLS_CERT`, `RE_TLS_KEY` and `RE_TLS_CA`
(and optionally `RE_TLS_CRL`) they serve TLS 1.3 with required client
certificates, and a partial set refuses to start. Go cannot restrict TLS 1.3
cipher suites, so on the suite's lanes each listener sits behind
RealityEngine_CI's OpenSSL front, which serves the transport profile
([issue #6](https://github.com/jateeter/RealityEngine_GO/issues/6)).
Specified but unimplemented routes currently return 404; the required
contract-derived 501 `unsupported` responses are tracked in
[issue #7](https://github.com/jateeter/RealityEngine_GO/issues/7).

## Local checks

```sh
gofmt -l .
go vet ./...
go test -race ./...
go build ./...
```

`gofmt -l .` must print no paths. These checks cover only this checkout; they
do not establish corpus conformance or live parity. See [AGENTS.md](AGENTS.md)
for the contract map, validation plan, and hosted CI policy.
