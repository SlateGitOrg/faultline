# Experiment `pricing-latency`: ABORTED

> Catalog tolerates
> slow pricing.

| | |
|---|---|
| Verdict | **aborted** |
| Hypothesis during faults | deviated |
| Recovered after rollback | yes |
| Abort reason | abort condition breached |
| Started | 2026-01-02T03:04:05Z |
| Duration | 5s |

## Errors

- rollback hook failed

## Timeline

| When | Event | Detail |
|---|---|---|
| t+0s | start | started |
| t+2.5s | abort | ABORT: a\|b |

## Faults injected

| Step | Proxy | Fault | Spec | Injected | Removed |
|---|---|---|---|---|---|
| slow | pricing | pricing-latency-1 | latency 400ms downstream | t+1s | t+2.6s (rollback) |
| edge | edge | failed: 403 \| blocked | status 503 | t+1s | — |

## Abort conditions at breach

- FAIL `catalog` success_rate >= 0.900, got 0.400 (4/10)

## Phases

### method (t+1s → t+2.6s)

| Probe | Samples | Success | p50 | p95 | p99 | max | Errors |
|---|---|---|---|---|---|---|---|
| catalog | 10 | 40.0% | 250ms | 251ms | 251ms | 252ms | status 503 ×5, timeout ×1 |

- FAIL `catalog` p95 <= 200ms, got 251ms

## Probe timeline

One column per 1s from the start of the run. Bar height is that slice's p95 latency relative to the slowest slice; `x` marks slices with failed probes.

```
catalog  ▁█ ▁  peak p95 250ms
          x
pricing     peak p95 0s
```
