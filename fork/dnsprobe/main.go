// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

// dnsprobe exercises the fork's DNS resolver using a read-only export from
// "tailscale dns status --json". It does not start tailscaled, log in, change
// system DNS, or create a tunnel. Packets use the already running client.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"tailscale.com/health"
	"tailscale.com/net/dns/resolver"
	"tailscale.com/net/netmon"
	"tailscale.com/net/tsdial"
	"tailscale.com/types/dnstype"
	"tailscale.com/types/logger"
	"tailscale.com/util/dnsname"
	"tailscale.com/util/eventbus"
)

type dnsStatus struct {
	Resolvers      []*dnstype.Resolver
	SplitDNSRoutes map[string][]*dnstype.Resolver
}

func run() error {
	configPath := flag.String("dns-status", "", "file exported by tailscale dns status --json")
	name := flag.String("name", "", "fully qualified name to query")
	family := flag.String("transport", "udp", "DNS transport: udp or tcp")
	httpScheme := flag.String("http", "", "also fetch / using the fork's answer: http or https")
	flag.Parse()
	if *configPath == "" || *name == "" || (*family != "udp" && *family != "tcp") {
		return errors.New("provide -dns-status, -name and a valid -transport")
	}
	if *httpScheme != "" && *httpScheme != "http" && *httpScheme != "https" {
		return errors.New("-http must be http or https")
	}
	fqdn, err := dnsname.ToFQDN(strings.ToLower(*name))
	if err != nil {
		return err
	}
	data, err := os.ReadFile(*configPath)
	if err != nil {
		return err
	}
	var status dnsStatus
	if err := json.Unmarshal(data, &status); err != nil {
		return err
	}
	routes := map[dnsname.FQDN][]*dnstype.Resolver{".": status.Resolvers}
	for suffix, upstream := range status.SplitDNSRoutes {
		domain, err := dnsname.ToFQDN(strings.ToLower(suffix))
		if err != nil {
			return err
		}
		routes[domain] = upstream
	}
	bus := eventbus.New()
	defer bus.Close()
	monitor := netmon.NewStatic()
	defer monitor.Close()
	dialer := tsdial.NewDialer(monitor)
	dialer.SetBus(bus)
	defer dialer.Close()
	r := resolver.New(logger.Discard, nil, dialer, health.NewTracker(bus), nil)
	defer r.Close()
	if err := r.SetConfig(resolver.Config{AcceptDNS: true, Routes: routes}); err != nil {
		return err
	}
	result := map[string]any{
		"name": fqdn, "transport": *family,
		"upstreams":            r.GetUpstreamResolvers(fqdn),
		"uses_existing_tunnel": true,
	}
	var queries []map[string]any
	var ipv4 []netip.Addr
	for _, typ := range []dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA, 65} {
		q := dnsmessage.Message{Header: dnsmessage.Header{ID: 42, RecursionDesired: true}, Questions: []dnsmessage.Question{
			{Name: dnsmessage.MustNewName(string(fqdn)), Type: typ, Class: dnsmessage.ClassINET},
		}}
		packet, err := q.Pack()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		start := time.Now()
		answer, err := r.Query(ctx, packet, *family, netip.MustParseAddrPort("127.0.0.1:12345"))
		elapsed := time.Since(start)
		cancel()
		if err != nil {
			return fmt.Errorf("DNS type %v: %w", typ, err)
		}
		var m dnsmessage.Message
		if err := m.Unpack(answer); err != nil {
			return err
		}
		var ips []string
		for _, rr := range m.Answers {
			if a, ok := rr.Body.(*dnsmessage.AResource); ok {
				ip := netip.AddrFrom4(a.A)
				ips = append(ips, ip.String())
				ipv4 = append(ipv4, ip)
			}
		}
		queries = append(queries, map[string]any{"type": typ.String(), "rcode": m.Header.RCode.String(),
			"answers": len(m.Answers), "ipv4": ips, "milliseconds": float64(elapsed.Microseconds()) / 1000})
	}
	result["queries"] = queries
	if *httpScheme != "" {
		if len(ipv4) == 0 {
			return errors.New("no IPv4 answer for HTTP probe")
		}
		transport := &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(address)
				if err != nil || host != fqdn.WithoutTrailingDot() {
					return nil, fmt.Errorf("unexpected HTTP target %q", address)
				}
				return (&net.Dialer{}).DialContext(ctx, "tcp4", net.JoinHostPort(ipv4[0].String(), port))
			},
		}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport, Timeout: 45 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		start := time.Now()
		resp, err := client.Get(*httpScheme + "://" + fqdn.WithoutTrailingDot() + "/")
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		n, err := io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return err
		}
		result["http"] = map[string]any{"status": resp.StatusCode, "bytes_read": n,
			"seconds": time.Since(start).Seconds(), "dial_address_from_fork_dns": ipv4[0].String()}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(result)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
