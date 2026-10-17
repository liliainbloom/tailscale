# Personal fork: milestone 1

Base: upstream `v1.104.1`, commit
`9a522a9786c97eb7910c01ccb7bd66557b04c910`.
Branch: `codex/onion-split-dns`.

This milestone implements and tests explicit `.onion` split DNS in the shared
Go core, builds the macOS CLI and daemon, and records a performance baseline.
It does not install a replacement app or change the running client's DNS,
routes, login, policy, or services. A native macOS UI, signed distribution,
system integration and a controlled migration remain later milestones.

## DNS behavior

Add an `onion` restricted nameserver in the usual Tailscale DNS settings. The
normal control-plane configuration supplies that route to the resolver. There
is no fork-specific gateway preference, hardcoded server address, hosts-file
mapping, or `/etc/resolver` exception.

- An explicit `onion` route, or a matching more-specific child route, permits
  queries to that resolver. The normal longest-suffix routing rules apply.
- Without an explicit usable route, forward lookups of `.onion` names receive
  NXDOMAIN. Paths rejected by the forwarding layer return SERVFAIL instead.
  A default or cloud fallback resolver does not enable onion DNS.
- The forwarding step independently enforces the explicit-route requirement.
  Removing the route between local lookup and forwarding fails the in-flight
  query instead of sending it to a newly configured default resolver.
- A failing onion resolver is not retried through ordinary DNS. Normal upstream
  response records and TTLs are retained; the fork invents no virtual addresses.
- Ordinary DNS, MagicDNS, packet encryption and the transport engine retain
  their upstream implementations.

The configured gateway must already resolve the names and handle the returned
addresses. Existing subnet routes and grants still govern access. This client
change creates no Tor exit, I2P transit capability, or new access grant. It also
does not change a browser's independent handling of `.onion` or secure DNS.

The rejection policy is an intentional opt-in exception to upstream's RFC 7686
handling. The exception applies to the normal client resolver path; it is not
a claim that every application, OS resolver, or exit-node DNS path filters
onion names. Keep the configured resolver trusted.

## Tests and build

Run from the repository root with the Go version in `go.mod`. For a compiler
matching upstream releases, use `./tool/go` (pinned by `go.toolchain.rev`).

```sh
go test ./net/dns/... -count=1
go test ./ipn/ipnlocal -run '^TestDNSConfigForNetmap$' -count=1
go test -race ./net/dns/resolver -count=1

# Build separate binaries; these commands do not install or start them.
mkdir -p ../fork-build
TS_USE_TOOLCHAIN=1 ./build_dist.sh -o ../fork-build/tailscale ./cmd/tailscale
TS_USE_TOOLCHAIN=1 ./build_dist.sh -o ../fork-build/tailscaled ./cmd/tailscaled
go build -o ../fork-build/dnsprobe ./fork/dnsprobe
```

The regression suite covers UDP/TCP; A, AAAA, HTTPS, SVCB, TXT and PTR queries;
case normalization; suffix boundaries; more-specific routes; local authority;
TTL preservation;
empty routes; unavailable custom resolver schemes; timeout; default/cloud
fallback isolation; and route removal between lookup and forwarding. Separate
tests cover ingestion of the control-plane route and Apple DNS compilation.
The new regression suite was run against unmodified upstream first and exposed
the expected rejection before the implementation was changed.

`Fork DNS checks` runs these checks and compilation on macOS and Linux in
GitHub Actions. It does not connect to a tailnet or need any account secrets.

## Live validation without replacing the client

The standalone probe imports a read-only snapshot of the currently delivered
DNS configuration and calls the fork's actual resolver. DNS and optional HTTP
packets travel through the already running client. No second daemon or node
identity is created. The snapshot is only a test input: a deployed fork daemon
would receive subsequent DNS changes through the normal control connection.

```sh
umask 077
tailscale dns status --json > ../dns-status.private.json
../fork-build/dnsprobe -dns-status ../dns-status.private.json \
  -name YOUR_SERVICE.onion -transport udp
../fork-build/dnsprobe -dns-status ../dns-status.private.json \
  -name YOUR_SERVICE.onion -transport tcp -http https
```

The HTTP check uses the address returned by the fork, retains the original
hostname for HTTP and TLS validation, reads at most 1 MiB, and does not follow
redirects. It does not test the system resolver or a browser. The DNS snapshot
can contain private names and authenticated resolver URLs: keep it out of Git.

## Reproduce the baseline

```sh
# Same test code and compiler for both versions; no checkout switching.
python3 fork/dns_baseline.py --go ./tool/go --base v1.104.1 \
  --samples 6 --benchtime 500ms --output ../dns-baseline-private

# Requires a trusted SSH alias, Python 3 on the peer and existing TCP grants.
python3 fork/transport_baseline.py client --ssh test-host \
  --peer-ip 100.64.0.2 --source-ip 100.64.0.1 \
  --client-pid PID_OF_CURRENT_CLIENT --output ../transport-baseline-private.json
```

The DNS harness overlays only the two changed production resolver files with
their upstream versions, while using identical tests and benchmarks. It builds
two test executables, alternates their run order, and records raw samples,
medians, compiler version, source hashes and base commit. If future production
changes extend beyond these two files, update the comparison accordingly.
It does not compare local NXDOMAIN to a real onion round trip as a speed test.

The transport harness starts a temporary server in memory over SSH. It binds
only the chosen Tailscale address, restricts the source IP, checks a random
token, caps request sizes, and expires after 180 seconds. No remote packages,
files, routes, firewall rules or Tailscale settings are installed. The client
requests shutdown and checks that the port closed. Optional repeated
`--ssh-option` flags allow existing hostname and known-host aliases to be used.

Transport results describe short TCP transfers, not maximum VPN capacity.
The fixed test exchanges 192 MiB total: three 16 MiB transfers per direction
with one stream and then four streams. CPU snapshots cover the selected
process only; other client processes and unrelated activity are not isolated.
Use a peer separate from latency-sensitive services and compare only runs
with equivalent paths, client builds, hardware and concurrent load.

See [the initial measurement report](BASELINE.md) for results and limitations.
