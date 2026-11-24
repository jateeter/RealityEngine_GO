# Load baseline

RealityEngine_GO#2 asks for "explicit measured full-corpus load and failure
behavior" and a published "baseline methodology/results, not invented
performance thresholds". This file is that baseline. **Nothing here is a
pass/fail threshold.** It records what was measured, so a later run can be
compared with it.

## Method

`go run ./cmd/load-baseline -re URL -pid PID -steps 200 -concurrency 8 -out FILE`
drives one RE through SURFACE_SPEC routes only, so it measures any runtime in
the same way. It resets the engine first and leaves it reset.

1. **Corpus:** `GET /api/machines` (count, bytes, time) and `GET /api/config`
   (`eventDimension`).
2. **Serial steps:** 200 `POST /api/perceive`, one at a time, each with a
   deterministic sparse space (about 1 cell in 16 set) of the full width.
   Machine results and the space are not returned.
3. **Concurrent:** 8 workers issue 200 steps between them. Each step is
   followed by a `GET /api/machines`. The phase records the step numbers the
   engine committed, which must be distinct and contiguous: steps are
   serialized, never lost or duplicated.
4. **Dimension growth:** `POST /api/machines` a machine whose regions sit 256
   cells past the space, read `eventDimension`, take a step, `DELETE` it.
5. **Failure behavior:** a malformed `/api/perceive` body, a 33 MiB
   `POST /api/machines` (the Go bound is 32 MiB), and `process` on an unknown
   machine, then `/api/health`.

RSS is `ps -o rss` before phase 1 and after phase 5.

## 2026-11-23, full corpus

- **Host:** Apple M1 Pro, 10 cores, 16 GiB, macOS 27.2. A native voter
  universe (cpp, lsp, scala) was running on the same host.
- **Corpus:** RealityEngine_Machines `7bb1a1c3`, branch
  `factory/ai-governance-regime-intake`: 1552 machines. The space was 20502
  cells, the 16944 seed grown by the clone band, because clones had been made
  and recycled on both engines just before.
- **Engines:** go-1 was RealityEngine_GO `73d7085` plus the PE metrics and
  clones branches (`c936e0a`). It was booted alone, `INSTANCE_ID=go-1`, and
  came up in 0.74 s. cpp-1 was RealityEngine_CPP `423ae03`, the universe's own.
  cpp-1 is included as context from the same host, not as a target.
- Raw reports: `load-baseline/2026-11-23-go-1.json` and `-cpp-1.json`.

| Measure | go-1 | cpp-1 |
|---|---|---|
| `GET /api/machines` (23.4 MB) | 182 ms | 676 ms |
| Serial step p50 / p90 / p99 / max | 12.7 / 16.0 / 36.7 / 39.2 ms | 20.4 / 22.4 / 25.0 / 25.7 ms |
| Serial throughput | 71.6 steps/s | 48.1 steps/s |
| Concurrent step p50 / p90 / p99 (8 workers, interleaved listings) | 128 / 315 / 417 ms | 1891 / 3683 / 4198 ms |
| Concurrent throughput (step + listing) | 22.1 /s | 1.65 /s |
| Concurrent step numbers | 200 distinct, contiguous 200–399 | 200 distinct, contiguous 200–399 |
| Errors (all phases) | 0 | 0 |
| Growth: width before → after | 20502 → 20766 | 20502 → 20766 |
| Step after growth | 22.3 ms | 20.8 ms |
| Malformed perceive body | 400 | 500 |
| 33 MiB machine body | 413 | connection closed (no status) |
| Unknown machine `process` | 404 | 404 |
| Health after | 200 | 200 |
| RSS before → after | 80 → 4350 MiB | 2 → 1195 MiB |

## Read with care

- **The concurrent phase is mostly `GET /api/machines`.** The 23.4 MB listing
  is interleaved with every step, so that phase measures listing contention
  more than stepping.
- **go-1's RSS grew by 4.2 GiB** across the run. The before value was sampled
  after boot, so it is not the resident corpus. The growth tracks the 400
  listings of 23.4 MB each, and Go returns heap to the OS lazily. The figure
  is reported, not chased here. A heap profile would separate live data from
  retained garbage.
- **The failure statuses differ from cpp-1** on the malformed body (400 v.
  500) and the oversize body (413 v. a dropped connection). These are recorded
  as observations, not parity results: no regression stage compares them, and
  SURFACE_SPEC states no status for either.
