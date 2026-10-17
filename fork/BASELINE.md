# Initial baseline — 2026-10-17

Development host: Apple M4 Pro, 12 logical CPUs, macOS 26.7.1, ARM64.
All final DNS comparisons used Go 1.27.1 from Tailscale's pinned compiler,
revision `24ee2fd0610e6c505ee4ec061a81215afe119d1f`.
Both sides use upstream `v1.104.1`; only the two production DNS files differ.

## DNS comparison

Six samples per case, alternating upstream/fork order. Most cases run for
500 ms per sample. TCP forwarding uses 500 queries per sample to bound
connection churn. All forwarding measurements use a local loopback resolver.
Values are medians, not statistical significance estimates.

| Operation | Upstream | Fork | Allocations/query, both |
| --- | ---: | ---: | ---: |
| Select ordinary default resolver | 5.57 ns | 14.41 ns | 0 |
| Local MagicDNS A answer | 0.308 µs | 0.312 µs | 2 |
| Onion rejection without an explicit route | 0.271 µs | 0.302 µs | 2 |
| Forward ordinary DNS over UDP | 93.9 µs | 95.9 µs | 78 |
| Forward ordinary DNS over TCP | 113.0 µs | 108.3 µs | 148 |

The explicit-route guard adds approximately 9 ns to the isolated default-route
selection benchmark, with no additional allocations. Rejecting an unconfigured
onion name adds approximately 31 ns because it now checks for an explicit route.
The small differences in forwarded DNS timings do not establish a speedup or
a regression. This patch changes DNS routing policy, not the VPN data path.

An earlier time-based TCP run varied from roughly 116 to 833 µs per query across
versions, with an apparent 19% difference between medians. That run is unsuitable
for a performance conclusion. Connection churn or host scheduling may contribute;
the cause was not proved. The bounded TCP run above provides a more useful
initial reference, but controlled repetition remains necessary for tuning.

Raw final samples:
[upstream DNS](baselines/2026-10-17-dns-upstream.txt),
[fork DNS](baselines/2026-10-17-dns-fork.txt),
[upstream TCP](baselines/2026-10-17-tcp-upstream.txt),
[fork TCP](baselines/2026-10-17-tcp-fork.txt).

Reproduce the final comparisons with:

```sh
python3 fork/dns_baseline.py --go ./tool/go --samples 6 --benchtime 500ms \
  --bench '^BenchmarkDNSRouteBaseline/(SelectDefault|MagicDNS|BlockedOnion|ForwardUDP)$' \
  --output ../dns-ab-private
python3 fork/dns_baseline.py --go ./tool/go --samples 6 --benchtime 500x \
  --bench '^BenchmarkDNSRouteBaseline/ForwardTCP$' \
  --output ../dns-tcp-ab-private
```

## Installed-client transport baseline

Measured the existing official macOS network-extension client, version
`1.104.0-t2b65a876f-g94e590e3a`, against a separate Linux peer. The observed path
was direct, using an IPv6 peer endpoint. No relay or onion gateway was used for
this throughput measurement. The official client remained running throughout.

Three 16 MiB transfers per direction and stream count; four-stream runs divide
the same 16 MiB total between connections. Total transferred: 192 MiB.

| Streams | Direction | Median | Observed range | Approx. extension CPU |
| --- | --- | ---: | ---: | ---: |
| 1 | Download | 257.8 Mbit/s | 156.9–362.9 Mbit/s | 144% |
| 1 | Upload | 29.6 Mbit/s | 29.3–29.8 Mbit/s | 36% |
| 4 | Download | 455.8 Mbit/s | 432.9–521.6 Mbit/s | 228% |
| 4 | Upload | 110.2 Mbit/s | 107.0–111.9 Mbit/s | 94% |

CPU is the median delta of process CPU time divided by transfer wall time;
100% represents one logical CPU. It excludes the GUI process, is sensitive to
short sample duration, and includes any concurrent traffic handled by the same
extension. Idle extension CPU over five seconds was approximately 0.6% of one
CPU. Extension RSS was 78.1 MiB before testing and peaked at 79.0 MiB.

TCP echo median round-trip latency was 5.36 ms before and 5.73 ms after the
transfer runs. These are short-transfer goodput figures affected by TCP startup,
WAN conditions and the peer. They are not a maximum-capacity result or a
measurement of the fork's VPN data path. The temporary peer listener exited
normally and its port was confirmed closed.

## Functional verification

- The original client reported the configured onion split resolver but answered
  the test onion A query with NXDOMAIN.
- The isolated fork resolver consumed that same configuration and returned a
  gateway virtual IPv4 address over both UDP and TCP. AAAA and HTTPS DNS queries
  returned successful empty answers, as supplied by the gateway.
- An HTTP request using the returned address received a 301 response. An HTTPS
  request to the onion hostname, retaining certificate verification, returned
  HTTP 200 and read 235,420 bytes in approximately 1.90 seconds. These individual
  page requests verify functionality; they are not Tor speed benchmarks.
- The full DNS suite passed: 440 test/subtest results passed, with six platform
  or environment skips. All 15 control-plane DNS test/subtest results passed.
  The resolver race-detector suite also passed with the pinned compiler.
- macOS ARM64 CLI, daemon and standalone probe binaries built successfully.

The live probe exercised the fork's resolver over the existing official tunnel.
No fork daemon was started or installed. The installed client's process ID,
addresses and DNS configuration were checked afterward and remained unchanged;
no resolver override files were created. Native browser resolution, OS VPN
integration, sleep/wake and full-fork transport performance await a separately
controlled migration test. Private DNS exports and raw peer details are kept
outside this repository.
