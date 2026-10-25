# Threat model

faultline deliberately degrades networked systems. Its security problem is
therefore less "can an attacker break faultline" than "can faultline be used
to break something it was not pointed at, or keep breaking it after people
think it stopped". This document states what the tool is for, what it
protects, and what it does not.

## Intended use

- Local development, CI and staging environments that the operator owns.
- Traffic reaches faultline only because the operator routed it there
  (a client configured to dial the proxy's address). faultline does not
  intercept, redirect or sniff traffic, does not modify routing or iptables,
  and needs no privileges.
- The bundled demo targets only the bundled sample service on loopback.

Out of scope: production traffic without an established chaos practice,
third-party systems, and any use against systems you do not operate.

## Assets

| Asset | Why it matters |
|---|---|
| Availability of the systems behind the proxies | Faults are outages by design; the risk is scope and duration. |
| The control API | Whoever can call it can degrade every proxied system. |
| Data passing through the proxies | faultline sees it in clear text (no TLS termination; HTTPS upstreams are re-encrypted by the reverse proxy). |
| The audit trail | Needed to explain an incident afterwards. |

## Threats and mitigations

| # | Threat | Mitigation | Residual risk |
|---|---|---|---|
| T1 | Unauthenticated network caller injects faults | Control API binds loopback by default; `serve` refuses a non-loopback listen address without `FAULTLINE_TOKEN`; bearer token compared in constant time; token read from the environment, not argv. | One shared token, no per-user identity, no TLS on the control API (put it behind a TLS terminator or keep it on loopback). Binding loopback stops remote hosts only; it does not stop a web page in the operator's browser (T9) or another local user or process. |
| T2 | Proxy used as an SSRF pivot / pointed at third-party hosts | Upstream allowlist (default loopback only); all resolved addresses must be allowed; TCP upstreams pinned to the resolved IP. | HTTP upstreams keep the host name, so a DNS change after creation is not re-checked; an operator can widen the allowlist arbitrarily. |
| T3 | A fault outlives its experiment (crashed runner, forgotten CLI fault) | Mandatory server-side TTL capped by `--max-ttl`; experiment TTL derived from step duration + grace; rollback on every exit path; kill switch on daemon shutdown. | A fault lives up to its TTL if both the runner and its rollback fail while the daemon keeps running. |
| T4 | A fault hits more than intended | Source scope, HTTP match, probability, `max_affected`, per-proxy and global caps; precondition gate; significance-tested abort objectives. | Scoping is only as good as the experiment author's choices; there is no dry-run traffic preview. |
| T5 | Control API abused for resource exhaustion | Request bodies capped at 1 MiB; caps on proxies and faults; latency/jitter/delay capped at 5 min; bandwidth floor of 64 B/s. | Many slow connections through a latency fault still hold sockets and up to 4 MiB of queued chunks per connection (64 × 32 KiB per direction). |
| T6 | Injected failures mistaken for real ones (or the reverse) | `X-Faultline-Fault` header on every faulted HTTP response; audit log; Prometheus counters; experiment reports with a full timeline. | L4 faults cannot be labelled in-band. |
| T7 | Malicious experiment file | Strict schema (unknown fields rejected), env expansion fails on undefined variables, rollback hooks restricted to http(s) URLs. | An experiment file can call arbitrary http(s) rollback hook URLs; treat experiment files as code and review them. |
| T9 | A web page in the operator's browser drives the loopback daemon (CSRF, DNS rebinding). Before the fix a cross-origin `text/plain` POST, which browsers send without a CORS preflight, created a proxy listening on `0.0.0.0` that published a loopback-only upstream to the LAN. | Requests with an `Origin` header or a `Sec-Fetch-Site` other than `none` are refused (`403`); POSTs must be `Content-Type: application/json` (`415`), which a cross-origin page cannot send without a preflight that the API never approves; without a token the `Host` header must be a loopback IP or `localhost` (`421`), which defeats DNS rebinding; proxies may listen only on loopback unless the daemon runs with `--allow-public-listen`. | Without a token, any local process or user on the machine can still drive the API (the guard only stops browsers). An unusual or buggy client that strips `Origin` from cross-origin requests would bypass the browser checks; the `Content-Type` and `Host` checks still apply. With a token set, a request from another host whose `Host` is not loopback is accepted, because the token is the protection. |
| T8 | Audit trail lost | In-memory ring of 512 events, readable incrementally (`?since=N`). | Lost on restart and not tamper-evident; export it if you need retention. |

## Explicit non-goals

- No stealth or evasion: faults are labelled and audited.
- No packet-level manipulation, ARP/DNS spoofing or traffic interception.
- No credential handling beyond the single control token.
