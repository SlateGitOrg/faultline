# Experiment file reference

An experiment is a YAML (or JSON) file. Unknown fields are errors, and
`${VAR}` / `${VAR:-default}` are expanded from the environment before
parsing (an unset variable without a default is an error). Durations are Go
duration strings: `250ms`, `3s`, `1m30s`.

Validate without running: `faultline validate experiments/*.yaml`.

```yaml
name: pricing-outage-abort                 # required
description: free text, shown in the report
control: ${FAULTLINE_CONTROL:-127.0.0.1:7070}   # required: control API address or URL

proxies:                                   # proxies the experiment needs
  - name: pricing                          # reused if it exists (kind must match),
    kind: tcp                              # created otherwise and deleted at teardown
    listen: 127.0.0.1:7102                 # default 127.0.0.1:0
    upstream: 127.0.0.1:7101               # tcp: host:port, http: base URL
  - name: edge
    kind: http
    upstream: http://127.0.0.1:7100

probes:                                    # at least one
  - name: catalog
    timeout: 1s                            # default 2s
    http:
      url: proxy://edge/products           # proxy://<name>/<path> = through that proxy
      method: GET                          # default GET
      headers: {Accept: application/json}
      body: ""
      expect_status: [200]                 # default: any 2xx
      body_contains: anvil
  - name: pricing-direct
    tcp:
      address: proxy://pricing             # or host:port
      send: "PING\n"                       # optional payload
      expect: PONG                         # optional substring of the first reply line

steady_state:                              # the hypothesis
  duration: 1500ms                         # observation before and after (default 3s)
  interval: 50ms                           # probe interval (default 200ms)
  recovery_grace: 500ms                    # unjudged settling time after rollback (default 0)
  objectives:                              # at least one
    - probe: catalog
      min_success_rate: 0.95               # any subset of these thresholds
      max_p50: 50ms
      max_p95: 200ms
      max_p99: 400ms

method:                                    # the fault schedule, at least one step
  - name: pricing-latency-spike            # default step-N
    proxy: pricing                         # declared above or already existing
    at: 0s                                 # offset from the start of the method
    duration: 60s                          # how long the faults stay installed
    faults:                                # see "Fault types"; ttl is derived, do not set it
      - type: latency
        latency: 400ms

abort:                                     # evaluated continuously during the method
  window: 1s                               # sliding window (default 2s)
  min_samples: 4                           # evidence needed before judging (default 5)
  check_every: 100ms                       # default: steady_state.interval
  confidence: 0.95                         # success-rate breaches must be significant; 0 = raw rate
  objectives:
    - probe: catalog
      min_success_rate: 0.8

rollback:                                  # extra actions after faultline removes its own faults
  - clear_faults: pricing                  # a proxy name, or "*" for every proxy
  - http: {method: POST, url: "http://127.0.0.1:9000/cache/flush"}   # 2xx expected

safety:
  fault_ttl_grace: 5s                      # fault TTL = step duration + this (default 10s)
  max_duration: 2m                         # cap on the schedule (default 10m)
```

## Fault types

Common fields: `id` (default generated), `probability` (0–1, default 1),
`max_affected` (concurrent cap, default none), `sources` (client IPs/CIDRs).

| Type | Kind | Fields | Effect |
|---|---|---|---|
| `latency` | tcp | `latency`, `jitter`, `direction` (default downstream) | Delays every chunk by latency ± U(jitter); never reorders. |
| `bandwidth` | tcp | `rate_bps` (≥ 64), `direction` | Caps throughput. |
| `reset` | tcp | `delay` (0 = on accept) | TCP RST to client and upstream. |
| `close` | tcp | `after_bytes` (0 = on accept), `direction` | Clean close after N bytes. |
| `slow_close` | tcp | `delay`, `direction` (default both) | Holds a half-close for `delay` before propagating it. |
| `blackhole` | tcp | `direction` | Silently drops data; connection stays open. |
| `delay` | http | `latency`, `jitter`, `match` | Holds the request before forwarding. |
| `status` | http | `status`, `body`, `response_headers`, `match` | Answers without contacting the upstream. |
| `abort` | http | `match` | Drops the client connection without a response. |
| `corrupt` | http | `mode: flip` + `rate` (default 0.01), or `mode: truncate` + `after_bytes`; `match` | Mangles the response body. |

HTTP `match`: `{methods: [GET, POST], path_prefix: /api, headers: {X-Canary: "1"}}`.

## Verdicts and exit codes

| Verdict | Exit | Meaning |
|---|---|---|
| `passed` | 0 | Objectives held before, during and after the faults. |
| `failed` | 2 | Hypothesis deviated during the faults, or steady state was not restored. |
| `aborted` | 3 | An abort objective was breached (or injection was refused, or the run was interrupted); faults were rolled back early. |
| `precondition_failed` | 4 | Objectives did not hold before injection; nothing was injected. |
| `error` | 1 | Setup, control-plane or rollback-hook errors. |

Reports are written to `--out` (default `faultline-reports/<name>/`) as
`report.json` and `report.md`.
