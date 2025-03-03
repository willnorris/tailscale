// Copyright (c) 2020 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// TODO(bradfitz): update this code to use netaddr more

// Package dnscache contains a minimal DNS cache that makes a bunch of
// assumptions that are only valid for us. Not recommended for general use.
package dnscache

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"runtime"
	"sync"
	"time"

	"tailscale.com/envknob"
	"tailscale.com/util/cloudenv"
	"tailscale.com/util/singleflight"
)

var single = &Resolver{
	Forward: &net.Resolver{PreferGo: preferGoResolver()},
}

func preferGoResolver() bool {
	// There does not appear to be a local resolver running
	// on iOS, and NetworkExtension is good at isolating DNS.
	// So do not use the Go resolver on macOS/iOS.
	if runtime.GOOS == "darwin" || runtime.GOOS == "ios" {
		return false
	}

	// The local resolver is not available on Android.
	if runtime.GOOS == "android" {
		return false
	}

	// Otherwise, the Go resolver is fine and slightly preferred
	// since it's lighter, not using cgo calls & threads.
	return true
}

// Get returns a caching Resolver singleton.
func Get() *Resolver { return single }

// Resolver is a minimal DNS caching resolver.
//
// The TTL is always fixed for now. It's not intended for general use.
// Cache entries are never cleaned up so it's intended that this is
// only used with a fixed set of hostnames.
type Resolver struct {
	// Forward is the resolver to use to populate the cache.
	// If nil, net.DefaultResolver is used.
	Forward *net.Resolver

	// LookupIPFallback optionally provides a backup DNS mechanism
	// to use if Forward returns an error or no results.
	LookupIPFallback func(ctx context.Context, host string) ([]netip.Addr, error)

	// TTL is how long to keep entries cached
	//
	// If zero, a default (currently 10 minutes) is used.
	TTL time.Duration

	// UseLastGood controls whether a cached entry older than TTL is used
	// if a refresh fails.
	UseLastGood bool

	// SingleHostStaticResult, if non-nil, is the static result of IPs that is returned
	// by Resolver.LookupIP for any hostname. When non-nil, SingleHost must also be
	// set with the expected name.
	SingleHostStaticResult []netip.Addr

	// SingleHost is the hostname that SingleHostStaticResult is for.
	// It is required when SingleHostStaticResult is present.
	SingleHost string

	sf singleflight.Group[string, ipRes]

	mu      sync.Mutex
	ipCache map[string]ipCacheEntry
}

// ipRes is the type used by the Resolver.sf singleflight group.
type ipRes struct {
	ip, ip6 net.IP
	allIPs  []net.IPAddr
}

type ipCacheEntry struct {
	ip      net.IP       // either v4 or v6
	ip6     net.IP       // nil if no v4 or no v6
	allIPs  []net.IPAddr // 1+ v4 and/or v6
	expires time.Time
}

func (r *Resolver) fwd() *net.Resolver {
	if r.Forward != nil {
		return r.Forward
	}
	return net.DefaultResolver
}

// cloudHostResolver returns a Resolver for the current cloud hosting environment.
// It currently only supports Google Cloud.
func (r *Resolver) cloudHostResolver() (v *net.Resolver, ok bool) {
	switch runtime.GOOS {
	case "android", "ios", "darwin":
		return nil, false
	case "windows":
		// TODO(bradfitz): remove this restriction once we're using Go 1.19
		// which supports net.Resolver.PreferGo on Windows.
		return nil, false
	}
	ip := cloudenv.Get().ResolverIP()
	if ip == "" {
		return nil, false
	}
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, net.JoinHostPort(ip, "53"))
		},
	}, true
}

func (r *Resolver) ttl() time.Duration {
	if r.TTL > 0 {
		return r.TTL
	}
	return 10 * time.Minute
}

var debug = envknob.Bool("TS_DEBUG_DNS_CACHE")

// LookupIP returns the host's primary IP address (either IPv4 or
// IPv6, but preferring IPv4) and optionally its IPv6 address, if
// there is both IPv4 and IPv6.
//
// If err is nil, ip will be non-nil. The v6 address may be nil even
// with a nil error.
func (r *Resolver) LookupIP(ctx context.Context, host string) (ip, v6 net.IP, allIPs []net.IPAddr, err error) {
	if r.SingleHostStaticResult != nil {
		if r.SingleHost != host {
			return nil, nil, nil, fmt.Errorf("dnscache: unexpected hostname %q doesn't match expected %q", host, r.SingleHost)
		}
		for _, naIP := range r.SingleHostStaticResult {
			ipa := &net.IPAddr{
				IP:   naIP.AsSlice(),
				Zone: naIP.Zone(),
			}
			if ip == nil && naIP.Is4() {
				ip = ipa.IP
			}
			if v6 == nil && naIP.Is6() {
				v6 = ipa.IP
			}
			allIPs = append(allIPs, *ipa)
		}
		return
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			return ip4, nil, []net.IPAddr{{IP: ip4}}, nil
		}
		if debug {
			log.Printf("dnscache: %q is an IP", host)
		}
		return ip, nil, []net.IPAddr{{IP: ip}}, nil
	}

	if ip, ip6, allIPs, ok := r.lookupIPCache(host); ok {
		if debug {
			log.Printf("dnscache: %q = %v (cached)", host, ip)
		}
		return ip, ip6, allIPs, nil
	}

	ch := r.sf.DoChan(host, func() (ret ipRes, _ error) {
		ip, ip6, allIPs, err := r.lookupIP(host)
		if err != nil {
			return ret, err
		}
		return ipRes{ip, ip6, allIPs}, nil
	})
	select {
	case res := <-ch:
		if res.Err != nil {
			if r.UseLastGood {
				if ip, ip6, allIPs, ok := r.lookupIPCacheExpired(host); ok {
					if debug {
						log.Printf("dnscache: %q using %v after error", host, ip)
					}
					return ip, ip6, allIPs, nil
				}
			}
			if debug {
				log.Printf("dnscache: error resolving %q: %v", host, res.Err)
			}
			return nil, nil, nil, res.Err
		}
		r := res.Val
		return r.ip, r.ip6, r.allIPs, nil
	case <-ctx.Done():
		if debug {
			log.Printf("dnscache: context done while resolving %q: %v", host, ctx.Err())
		}
		return nil, nil, nil, ctx.Err()
	}
}

func (r *Resolver) lookupIPCache(host string) (ip, ip6 net.IP, allIPs []net.IPAddr, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ent, ok := r.ipCache[host]; ok && ent.expires.After(time.Now()) {
		return ent.ip, ent.ip6, ent.allIPs, true
	}
	return nil, nil, nil, false
}

func (r *Resolver) lookupIPCacheExpired(host string) (ip, ip6 net.IP, allIPs []net.IPAddr, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ent, ok := r.ipCache[host]; ok {
		return ent.ip, ent.ip6, ent.allIPs, true
	}
	return nil, nil, nil, false
}

func (r *Resolver) lookupTimeoutForHost(host string) time.Duration {
	if r.UseLastGood {
		if _, _, _, ok := r.lookupIPCacheExpired(host); ok {
			// If we have some previous good value for this host,
			// don't give this DNS lookup much time. If we're in a
			// situation where the user's DNS server is unreachable
			// (e.g. their corp DNS server is behind a subnet router
			// that can't come up due to Tailscale needing to
			// connect to itself), then we want to fail fast and let
			// our caller (who set UseLastGood) fall back to using
			// the last-known-good IP address.
			return 3 * time.Second
		}
	}
	return 10 * time.Second
}

func (r *Resolver) lookupIP(host string) (ip, ip6 net.IP, allIPs []net.IPAddr, err error) {
	if ip, ip6, allIPs, ok := r.lookupIPCache(host); ok {
		if debug {
			log.Printf("dnscache: %q found in cache as %v", host, ip)
		}
		return ip, ip6, allIPs, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), r.lookupTimeoutForHost(host))
	defer cancel()
	ips, err := r.fwd().LookupIPAddr(ctx, host)
	if err != nil || len(ips) == 0 {
		if resolver, ok := r.cloudHostResolver(); ok {
			ips, err = resolver.LookupIPAddr(ctx, host)
		}
	}
	if (err != nil || len(ips) == 0) && r.LookupIPFallback != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var fips []netip.Addr
		fips, err = r.LookupIPFallback(ctx, host)
		if err == nil {
			ips = nil
			for _, fip := range fips {
				ips = append(ips, net.IPAddr{
					IP:   fip.AsSlice(),
					Zone: fip.Zone(),
				})
			}
		}
	}
	if err != nil {
		return nil, nil, nil, err
	}
	if len(ips) == 0 {
		return nil, nil, nil, fmt.Errorf("no IPs for %q found", host)
	}

	have4 := false
	for _, ipa := range ips {
		if ip4 := ipa.IP.To4(); ip4 != nil {
			if !have4 {
				ip6 = ip
				ip = ip4
				have4 = true
			}
		} else {
			if have4 {
				ip6 = ipa.IP
			} else {
				ip = ipa.IP
			}
		}
	}
	r.addIPCache(host, ip, ip6, ips, r.ttl())
	return ip, ip6, ips, nil
}

func (r *Resolver) addIPCache(host string, ip, ip6 net.IP, allIPs []net.IPAddr, d time.Duration) {
	if ip.IsPrivate() {
		// Don't cache obviously wrong entries from captive portals.
		// TODO: use DoH or DoT for the forwarding resolver?
		if debug {
			log.Printf("dnscache: %q resolved to private IP %v; using but not caching", host, ip)
		}
		return
	}

	if debug {
		log.Printf("dnscache: %q resolved to IP %v; caching", host, ip)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ipCache == nil {
		r.ipCache = make(map[string]ipCacheEntry)
	}
	r.ipCache[host] = ipCacheEntry{
		ip:      ip,
		ip6:     ip6,
		allIPs:  allIPs,
		expires: time.Now().Add(d),
	}
}

type DialContextFunc func(ctx context.Context, network, address string) (net.Conn, error)

// Dialer returns a wrapped DialContext func that uses the provided dnsCache.
func Dialer(fwd DialContextFunc, dnsCache *Resolver) DialContextFunc {
	d := &dialer{
		fwd:         fwd,
		dnsCache:    dnsCache,
		pastConnect: map[netip.Addr]time.Time{},
	}
	return d.DialContext
}

// dialer is the config and accumulated state for a dial func returned by Dialer.
type dialer struct {
	fwd      DialContextFunc
	dnsCache *Resolver

	mu          sync.Mutex
	pastConnect map[netip.Addr]time.Time
}

func (d *dialer) DialContext(ctx context.Context, network, address string) (retConn net.Conn, ret error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		// Bogus. But just let the real dialer return an error rather than
		// inventing a similar one.
		return d.fwd(ctx, network, address)
	}
	dc := &dialCall{
		d:       d,
		network: network,
		address: address,
		host:    host,
		port:    port,
	}
	defer func() {
		// On failure, consider that our DNS might be wrong and ask the DNS fallback mechanism for
		// some other IPs to try.
		if ret == nil || ctx.Err() != nil || d.dnsCache.LookupIPFallback == nil || dc.dnsWasTrustworthy() {
			return
		}
		ips, err := d.dnsCache.LookupIPFallback(ctx, host)
		if err != nil {
			// Return with original error
			return
		}
		if c, err := dc.raceDial(ctx, ips); err == nil {
			retConn = c
			ret = nil
			return
		}
	}()

	ip, ip6, allIPs, err := d.dnsCache.LookupIP(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve %q: %w", host, err)
	}
	i4s := v4addrs(allIPs)
	if len(i4s) < 2 {
		if debug {
			log.Printf("dnscache: dialing %s, %s for %s", network, ip, address)
		}
		ipNA, ok := netip.AddrFromSlice(ip)
		if !ok {
			return nil, fmt.Errorf("invalid IP %q", ip)
		}
		c, err := dc.dialOne(ctx, ipNA.Unmap())
		if err == nil || ctx.Err() != nil {
			return c, err
		}
		// Fall back to trying IPv6, if any.
		ip6NA, ok := netip.AddrFromSlice(ip6)
		if !ok {
			return nil, err
		}
		return dc.dialOne(ctx, ip6NA)
	}

	// Multiple IPv4 candidates, and 0+ IPv6.
	ipsToTry := append(i4s, v6addrs(allIPs)...)
	return dc.raceDial(ctx, ipsToTry)
}

// dialCall is the state around a single call to dial.
type dialCall struct {
	d                            *dialer
	network, address, host, port string

	mu    sync.Mutex           // lock ordering: dialer.mu, then dialCall.mu
	fails map[netip.Addr]error // set of IPs that failed to dial thus far
}

// dnsWasTrustworthy reports whether we think the IP address(es) we
// tried (and failed) to dial were probably the correct IPs. Currently
// the heuristic is whether they ever worked previously.
func (dc *dialCall) dnsWasTrustworthy() bool {
	dc.d.mu.Lock()
	defer dc.d.mu.Unlock()
	dc.mu.Lock()
	defer dc.mu.Unlock()

	if len(dc.fails) == 0 {
		// No information.
		return false
	}

	// If any of the IPs we failed to dial worked previously in
	// this dialer, assume the DNS is fine.
	for ip := range dc.fails {
		if _, ok := dc.d.pastConnect[ip]; ok {
			return true
		}
	}
	return false
}

func (dc *dialCall) dialOne(ctx context.Context, ip netip.Addr) (net.Conn, error) {
	c, err := dc.d.fwd(ctx, dc.network, net.JoinHostPort(ip.String(), dc.port))
	dc.noteDialResult(ip, err)
	return c, err
}

// noteDialResult records that a dial to ip either succeeded or
// failed.
func (dc *dialCall) noteDialResult(ip netip.Addr, err error) {
	if err == nil {
		d := dc.d
		d.mu.Lock()
		defer d.mu.Unlock()
		d.pastConnect[ip] = time.Now()
		return
	}
	dc.mu.Lock()
	defer dc.mu.Unlock()
	if dc.fails == nil {
		dc.fails = map[netip.Addr]error{}
	}
	dc.fails[ip] = err
}

// uniqueIPs returns a possibly-mutated subslice of ips, filtering out
// dups and ones that have already failed previously.
func (dc *dialCall) uniqueIPs(ips []netip.Addr) (ret []netip.Addr) {
	dc.mu.Lock()
	defer dc.mu.Unlock()
	seen := map[netip.Addr]bool{}
	ret = ips[:0]
	for _, ip := range ips {
		if seen[ip] {
			continue
		}
		seen[ip] = true
		if dc.fails[ip] != nil {
			continue
		}
		ret = append(ret, ip)
	}
	return ret
}

// fallbackDelay is how long to wait between trying subsequent
// addresses when multiple options are available.
// 300ms is the same as Go's Happy Eyeballs fallbackDelay value.
const fallbackDelay = 300 * time.Millisecond

// raceDial tries to dial port on each ip in ips, starting a new race
// dial every fallbackDelay apart, returning whichever completes first.
func (dc *dialCall) raceDial(ctx context.Context, ips []netip.Addr) (net.Conn, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type res struct {
		c   net.Conn
		err error
	}
	resc := make(chan res)           // must be unbuffered
	failBoost := make(chan struct{}) // best effort send on dial failure

	// Remove IPs that we tried & failed to dial previously
	// (such as when we're being called after a dnsfallback lookup and get
	// the same results)
	ips = dc.uniqueIPs(ips)
	if len(ips) == 0 {
		return nil, errors.New("no IPs")
	}

	// Partition candidate list and then merge such that an IPv6 address is
	// in the first spot if present, and then addresses are interleaved.
	// This ensures that we're trying an IPv6 address first, then
	// alternating between v4 and v6 in case one of the two networks is
	// broken.
	var iv4, iv6 []netip.Addr
	for _, ip := range ips {
		if ip.Is6() {
			iv6 = append(iv6, ip)
		} else {
			iv4 = append(iv4, ip)
		}
	}
	ips = interleaveSlices(iv6, iv4)

	go func() {
		for i, ip := range ips {
			if i != 0 {
				timer := time.NewTimer(fallbackDelay)
				select {
				case <-timer.C:
				case <-failBoost:
					timer.Stop()
				case <-ctx.Done():
					timer.Stop()
					return
				}
			}
			go func(ip netip.Addr) {
				c, err := dc.dialOne(ctx, ip)
				if err != nil {
					// Best effort wake-up a pending dial.
					// e.g. IPv4 dials failing quickly on an IPv6-only system.
					// In that case we don't want to wait 300ms per IPv4 before
					// we get to the IPv6 addresses.
					select {
					case failBoost <- struct{}{}:
					default:
					}
				}
				select {
				case resc <- res{c, err}:
				case <-ctx.Done():
					if c != nil {
						c.Close()
					}
				}
			}(ip)
		}
	}()

	var firstErr error
	var fails int
	for {
		select {
		case r := <-resc:
			if r.c != nil {
				return r.c, nil
			}
			fails++
			if firstErr == nil {
				firstErr = r.err
			}
			if fails == len(ips) {
				return nil, firstErr
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// interleaveSlices combines two slices of the form [a, b, c] and [x, y, z]
// into a slice with elements interleaved; i.e. [a, x, b, y, c, z].
func interleaveSlices[T any](a, b []T) []T {
	var (
		i   int
		ret = make([]T, 0, len(a)+len(b))
	)
	for i = 0; i < len(a) && i < len(b); i++ {
		ret = append(ret, a[i], b[i])
	}
	ret = append(ret, a[i:]...)
	ret = append(ret, b[i:]...)
	return ret
}

func v4addrs(aa []net.IPAddr) (ret []netip.Addr) {
	for _, a := range aa {
		ip, ok := netip.AddrFromSlice(a.IP)
		ip = ip.Unmap()
		if ok && ip.Is4() {
			ret = append(ret, ip)
		}
	}
	return ret
}

func v6addrs(aa []net.IPAddr) (ret []netip.Addr) {
	for _, a := range aa {
		if ip, ok := netip.AddrFromSlice(a.IP); ok && ip.Is6() {
			ret = append(ret, ip)
		}
	}
	return ret
}

var errTLSHandshakeTimeout = errors.New("timeout doing TLS handshake")

// TLSDialer is like Dialer but returns a func suitable for using with net/http.Transport.DialTLSContext.
// It returns a *tls.Conn type on success.
// On TLS cert validation failure, it can invoke a backup DNS resolution strategy.
func TLSDialer(fwd DialContextFunc, dnsCache *Resolver, tlsConfigBase *tls.Config) DialContextFunc {
	tcpDialer := Dialer(fwd, dnsCache)
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		tcpConn, err := tcpDialer(ctx, network, address)
		if err != nil {
			return nil, err
		}

		cfg := cloneTLSConfig(tlsConfigBase)
		if cfg.ServerName == "" {
			cfg.ServerName = host
		}
		tlsConn := tls.Client(tcpConn, cfg)

		handshakeCtx, handshakeTimeoutCancel := context.WithTimeout(ctx, 5*time.Second)
		defer handshakeTimeoutCancel()
		if err := tlsConn.HandshakeContext(handshakeCtx); err != nil {
			tcpConn.Close()
			// TODO: if err != errTLSHandshakeTimeout,
			// assume it might be some captive portal or
			// otherwise incorrect DNS and try the backup
			// DNS mechanism.
			return nil, err
		}
		return tlsConn, nil
	}
}

func cloneTLSConfig(cfg *tls.Config) *tls.Config {
	if cfg == nil {
		return &tls.Config{}
	}
	return cfg.Clone()
}
