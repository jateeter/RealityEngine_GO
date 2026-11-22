# RealityEngine_GO development guidance

## Role and authority

The Go RE/PE pair is in bootstrap, not yet admitted to a suite lane. Under the
[quorum contract](https://github.com/jateeter/RealityEngine_CI/blob/main/docs/QUORUM_CONTRACT.md),
new runtimes are non-voting conformance followers; C++, LSP and Scala remain
the three voters. Shipping a follower does not make it a voter.

Read current versions of these before changing observable behavior:

- [Integrated specification](https://github.com/jateeter/RealityEngine_CI/blob/main/INTEGRATED_SPECIFICATION.md):
  documentation map and specification authority.
- [Surface specification](https://github.com/jateeter/RealityEngine_CI/blob/main/SURFACE_SPEC.md):
  RE/PE routes, canonical wire shapes, participation states and step behavior.
- [Deployment contract](https://github.com/jateeter/RealityEngine_CI/blob/main/DEPLOYMENT_CONTRACT.md):
  environment names, runtime ports and integration boundaries.
- [Arbiter contract](https://github.com/jateeter/RealityEngine_CI/blob/main/docs/ARBITER_CONTRACT.md)
  and [transport security](https://github.com/jateeter/RealityEngine_CI/blob/main/docs/TRANSPORT_SECURITY.md):
  output resolution and suite TLS/PKI requirements.
- [Engineering contract](https://github.com/jateeter/RealityEngine_CI/blob/main/docs/ENGINEERING_CONTRACT.md):
  repository verification and hosted CI rules.
- [RealityEngine_Rust](https://github.com/jateeter/RealityEngine_Rust):
  implementation precedent and `conformance/SURFACE_STATUS.md` for current
  route status. It is not the sole oracle.

Use the canonical Reality Event keys `events`, `outputEvents`, `nextEventIds`,
`inputEvent` and `eventDimension`. When the specification and voters differ,
or the voters split, report the discrepancy for an owner ruling; never invent
a reference result or treat a two-to-one vote as consensus.

## Entry points and validation

`cmd/reality-engine` and `cmd/perception-engine` start the respective local
bootstrap servers; `internal/server` implements their shared lifecycle and
health endpoint. Run the local commands in the README before a PR.

The planned tiers, modeled on the
[Rust validation workflow](https://github.com/jateeter/RealityEngine_CI/blob/main/docs/RUST_VALIDATION_WORKFLOW.md),
are local static/build and unit checks (R0), pinned corpus and surface
conformance (R1), live non-voting comparison with the three voters (R2), and
the full suite regression (R3). Only R0 exists here today. R1 is tracked in
[#9](https://github.com/jateeter/RealityEngine_GO/issues/9) and R2/R3 in
[#12](https://github.com/jateeter/RealityEngine_GO/issues/12). Report
unavailable or unrun checks explicitly, not as passing.

The engineering contract permits cheap, checkout-local peripheral checks but
reserves integrated verification for RealityEngine_CI. Rust's self-contained
hosted workflow is an owner-approved exception, not authorization for Go.
Propose any Go hosted workflow to the owner before adding it; it must not
silently grow into a separate universe or be presented as parity evidence.
