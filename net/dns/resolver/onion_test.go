// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package resolver

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	mdns "github.com/miekg/dns"
	"tailscale.com/types/dnstype"
	"tailscale.com/types/logger"
	"tailscale.com/util/dnsname"
)

// onionTestServer serves both transports on one loopback port. Counting actual
// received requests detects accidental fallback, not just the returned answer.
func onionTestServer(t testing.TB, answer string, silent bool) (string, *atomic.Int64) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pc, err := net.ListenPacket("udp", ln.Addr().String())
	if err != nil {
		ln.Close()
		t.Fatal(err)
	}
	count := new(atomic.Int64)
	handler := mdns.HandlerFunc(func(w mdns.ResponseWriter, q *mdns.Msg) {
		count.Add(1)
		if silent {
			return
		}
		m := new(mdns.Msg)
		m.SetReply(q)
		if len(q.Question) == 1 && q.Question[0].Qtype == mdns.TypeA {
			m.Answer = []mdns.RR{&mdns.A{
				Hdr: mdns.RR_Header{Name: q.Question[0].Name, Rrtype: mdns.TypeA, Class: mdns.ClassINET, Ttl: 60},
				A:   net.ParseIP(answer).To4(),
			}}
		}
		w.WriteMsg(m)
	})
	for _, s := range []*mdns.Server{{Listener: ln, Handler: handler}, {PacketConn: pc, Handler: handler}} {
		started := make(chan struct{})
		s.NotifyStartedFunc = func() { close(started) }
		go func() {
			if err := s.ActivateAndServe(); err != nil {
				t.Errorf("DNS server: %v", err)
			}
		}()
		<-started
		t.Cleanup(func() { s.Shutdown() })
	}
	return ln.Addr().String(), count
}

func onionQuestion(t testing.TB, name string, typ uint16) []byte {
	t.Helper()
	m := new(mdns.Msg)
	m.SetQuestion(name, typ)
	b, err := m.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func onionQuery(t testing.TB, r *Resolver, name, family string, typ uint16) *mdns.Msg {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	b, err := r.Query(ctx, onionQuestion(t, name, typ), family, netip.MustParseAddrPort("127.0.0.1:12345"))
	if err != nil {
		t.Fatal(err)
	}
	m := new(mdns.Msg)
	if err := m.Unpack(b); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestOnionSplitDNS(t *testing.T) {
	addr, explicit := onionTestServer(t, "10.250.0.5", false)
	other, fallback := onionTestServer(t, "192.0.2.1", false)
	r := newResolver(t)
	t.Cleanup(r.Close)
	if err := r.SetConfig(Config{Routes: map[dnsname.FQDN][]*dnstype.Resolver{
		".":      {{Addr: other}},
		"onion.": {{Addr: addr}},
	}}); err != nil {
		t.Fatal(err)
	}
	for _, family := range []string{"udp", "tcp"} {
		for _, name := range []string{"service.onion.", "Service.ONION.", "sub.service.onion.", "onion."} {
			for _, typ := range []uint16{mdns.TypeA, mdns.TypeAAAA, mdns.TypeHTTPS, mdns.TypeSVCB, mdns.TypeTXT, mdns.TypePTR} {
				t.Run(family+"/"+name+"/"+mdns.TypeToString[typ], func(t *testing.T) {
					m := onionQuery(t, r, name, family, typ)
					if m.Rcode != mdns.RcodeSuccess {
						t.Fatalf("rcode = %v, want NOERROR", m.Rcode)
					}
					if typ == mdns.TypeA {
						if len(m.Answer) != 1 {
							t.Fatalf("answers = %v", m.Answer)
						}
						a, ok := m.Answer[0].(*mdns.A)
						if !ok || a.A.String() != "10.250.0.5" || a.Hdr.Ttl != 60 {
							t.Fatalf("answer/TTL not preserved: %v", m.Answer)
						}
					} else if len(m.Answer) != 0 {
						t.Fatalf("expected NODATA, got %v", m.Answer)
					}
				})
			}
		}
	}
	// The upstream forwarder may retry an empty UDP answer over TCP.
	if explicit.Load() < 48 || fallback.Load() != 0 {
		t.Fatalf("received requests: onion=%v, default=%v; want at least 48, 0", explicit.Load(), fallback.Load())
	}
	// These suffix lookalikes must retain ordinary DNS routing.
	for _, name := range []string{"notonion.", "service.onion.example.", "example.com."} {
		m := onionQuery(t, r, name, "udp", mdns.TypeA)
		if len(m.Answer) != 1 || m.Answer[0].(*mdns.A).A.String() != "192.0.2.1" {
			t.Fatalf("ordinary query %s misrouted: %v", name, m)
		}
	}
}

func TestOnionNoImplicitResolver(t *testing.T) {
	addr, requests := onionTestServer(t, "192.0.2.1", false)
	for _, tc := range []struct {
		name   string
		routes map[dnsname.FQDN][]*dnstype.Resolver
	}{
		{"no_routes", nil},
		{"default_only", map[dnsname.FQDN][]*dnstype.Resolver{".": {{Addr: addr}}}},
		{"unrelated_suffix", map[dnsname.FQDN][]*dnstype.Resolver{"notonion.": {{Addr: addr}}}},
		{"empty_onion", map[dnsname.FQDN][]*dnstype.Resolver{".": {{Addr: addr}}, "onion.": {}}},
		{"unavailable_scheme", map[dnsname.FQDN][]*dnstype.Resolver{".": {{Addr: addr}}, "onion.": {{Addr: "unavailable:test"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newResolver(t)
			t.Cleanup(r.Close)
			if err := r.RegisterCustomScheme("unavailable", func(string) (string, error) { return "", nil }); err != nil {
				t.Fatal(err)
			}
			if err := r.SetConfig(Config{Routes: tc.routes}); err != nil {
				t.Fatal(err)
			}
			// Simulate a cloud fallback even on a developer's non-cloud machine.
			r.forwarder.mu.Lock()
			r.forwarder.cloudHostFallback = []resolverAndDelay{{name: &dnstype.Resolver{Addr: addr}}}
			r.forwarder.mu.Unlock()
			for _, name := range []string{"service.onion.", "onion."} {
				if m := onionQuery(t, r, name, "udp", mdns.TypeA); m.Rcode != mdns.RcodeNameError {
					t.Errorf("%s rcode=%v, want NXDOMAIN", name, m.Rcode)
				}
				if got := r.GetUpstreamResolvers(dnsname.FQDN(name)); len(got) != 0 {
					t.Errorf("implicit resolvers for %s: %v", name, got)
				}
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatalf("leaked %v requests to default/cloud resolver", requests.Load())
	}
}

func TestOnionSpecificRoute(t *testing.T) {
	general, _ := onionTestServer(t, "10.250.0.5", false)
	specific, _ := onionTestServer(t, "10.250.0.6", false)
	r := newResolver(t)
	t.Cleanup(r.Close)
	if err := r.SetConfig(Config{Routes: map[dnsname.FQDN][]*dnstype.Resolver{
		"onion.":         {{Addr: general}},
		"service.onion.": {{Addr: specific}},
	}}); err != nil {
		t.Fatal(err)
	}
	m := onionQuery(t, r, "sub.service.onion.", "udp", mdns.TypeA)
	if len(m.Answer) != 1 || m.Answer[0].(*mdns.A).A.String() != "10.250.0.6" {
		t.Fatalf("most-specific route not used: %v", m)
	}
}

func TestOnionResolverFailure(t *testing.T) {
	addr, _ := onionTestServer(t, "10.250.0.5", true)
	other, fallback := onionTestServer(t, "192.0.2.1", false)
	r := newResolver(t)
	t.Cleanup(r.Close)
	if err := r.SetConfig(Config{Routes: map[dnsname.FQDN][]*dnstype.Resolver{
		".": {{Addr: other}}, "onion.": {{Addr: addr}},
	}}); err != nil {
		t.Fatal(err)
	}
	for _, family := range []string{"udp", "tcp"} {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		_, err := r.Query(ctx, onionQuestion(t, "service.onion.", mdns.TypeA), family, netip.MustParseAddrPort("127.0.0.1:12345"))
		cancel()
		if err == nil {
			t.Errorf("%s: silent resolver unexpectedly succeeded", family)
		}
	}
	if fallback.Load() != 0 {
		t.Fatalf("sent %v queries to default resolver after onion failure", fallback.Load())
	}
}

func TestOnionLocalAuthority(t *testing.T) {
	addr, requests := onionTestServer(t, "10.250.0.5", false)
	r := newResolver(t)
	t.Cleanup(r.Close)
	if err := r.SetConfig(Config{
		Routes:       map[dnsname.FQDN][]*dnstype.Resolver{"onion.": {{Addr: addr}}},
		LocalDomains: []dnsname.FQDN{"local.onion."},
		Hosts:        map[dnsname.FQDN][]netip.Addr{"host.local.onion.": {netip.MustParseAddr("10.250.0.9")}},
	}); err != nil {
		t.Fatal(err)
	}
	// Once forwarding is opted into, retain the usual local-authority rules.
	// A resolver-less child domain must not leak through its upstream parent.
	if m := onionQuery(t, r, "missing.local.onion.", "udp", mdns.TypeA); m.Rcode != mdns.RcodeNameError {
		t.Fatalf("local authoritative miss: %v", m)
	}
	m := onionQuery(t, r, "host.local.onion.", "udp", mdns.TypeA)
	if len(m.Answer) != 1 || m.Answer[0].(*mdns.A).A.String() != "10.250.0.9" {
		t.Fatalf("local record not preserved: %v", m)
	}
	if requests.Load() != 0 {
		t.Fatalf("forwarded %v authoritative queries", requests.Load())
	}
	if m := onionQuery(t, r, "remote.onion.", "udp", mdns.TypeA); m.Rcode != mdns.RcodeSuccess {
		t.Fatalf("remote query not forwarded: %v", m)
	}
}

func TestOnionRouteRemovalDuringQuery(t *testing.T) {
	addr, requests := onionTestServer(t, "192.0.2.1", false)
	r := newResolver(t)
	t.Cleanup(r.Close)
	if err := r.SetConfig(Config{Routes: map[dnsname.FQDN][]*dnstype.Resolver{"onion.": {{Addr: addr}}}}); err != nil {
		t.Fatal(err)
	}
	query := onionQuestion(t, "service.onion.", mdns.TypeA)
	if _, err := r.respond(query); !errors.Is(err, errNotOurName) {
		t.Fatalf("configured onion query was not delegated: %v", err)
	}
	// Interleave a control-plane config update between Query's local lookup
	// and its forwarding step. The second lookup must not use the new default.
	if err := r.SetConfig(Config{Routes: map[dnsname.FQDN][]*dnstype.Resolver{".": {{Addr: addr}}}}); err != nil {
		t.Fatal(err)
	}
	responses := make(chan packet, 1)
	if err := r.forwarder.forwardWithDestChan(t.Context(), packet{bs: query, family: "udp", addr: netip.MustParseAddrPort("127.0.0.1:12345")}, responses); err != nil {
		t.Fatal(err)
	}
	m := new(mdns.Msg)
	if err := m.Unpack((<-responses).bs); err != nil {
		t.Fatal(err)
	}
	if m.Rcode != mdns.RcodeServerFailure || requests.Load() != 0 {
		t.Fatalf("route removal: rcode=%v, leaked queries=%v", m.Rcode, requests.Load())
	}
	if m := onionQuery(t, r, "service.onion.", "udp", mdns.TypeA); m.Rcode != mdns.RcodeNameError {
		t.Fatalf("query after removal: rcode=%v", m.Rcode)
	}
}

// BenchmarkDNSRouteBaseline runs unchanged against upstream and the fork.
// Onion forwarding is intentionally excluded: comparing upstream's immediate
// rejection to a real DNS round trip would not be a meaningful speed comparison.
func BenchmarkDNSRouteBaseline(b *testing.B) {
	addr, _ := onionTestServer(b, "192.0.2.1", false)
	r := newResolver(b)
	b.Cleanup(r.Close)
	r.logf = logger.Discard
	r.forwarder.logf = logger.Discard
	if err := r.SetConfig(Config{
		Routes: map[dnsname.FQDN][]*dnstype.Resolver{".": {{Addr: addr}}},
		Hosts:  map[dnsname.FQDN][]netip.Addr{"node.test.": {netip.MustParseAddr("100.64.0.1")}},
	}); err != nil {
		b.Fatal(err)
	}
	b.Run("SelectDefault", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if len(r.forwarder.resolvers("example.com.")) != 1 {
				b.Fatal("default route missing")
			}
		}
	})
	for _, tc := range []struct{ name, domain, family string }{
		{"MagicDNS", "node.test.", "udp"},
		{"BlockedOnion", "service.onion.", "udp"},
		{"ForwardUDP", "example.com.", "udp"},
		{"ForwardTCP", "example.com.", "tcp"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			q := onionQuestion(b, tc.domain, mdns.TypeA)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, err := r.Query(b.Context(), q, tc.family, netip.MustParseAddrPort("127.0.0.1:12345")); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
