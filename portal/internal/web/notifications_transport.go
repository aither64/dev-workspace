package web

import (
	"context"
	"crypto/elliptic"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// Exact provider hosts, not suffixes. Additional providers need a reviewed
// transport change and a real subscription/test on the target browser build.
var pushProviderHosts = map[string]bool{
	"updates.push.services.mozilla.com": true,
	"fcm.googleapis.com":                true,
	// Chromium 147 uses this upstream staging GCM host for non-stable channels,
	// including unbranded builds. It is an exact host, not a Google suffix rule.
	"jmt17.google.com": true,
}

func validatePushEndpoint(endpoint string) (*url.URL, error) {
	u, err := url.Parse(endpoint)
	if err != nil || len(endpoint) > 2048 || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" ||
		(u.Port() != "" && u.Port() != "443") || !pushProviderHosts[u.Hostname()] || u.Path == "" ||
		strings.ContainsAny(endpoint, "\x00\r\n") {
		return nil, errors.New("unsupported push provider endpoint")
	}
	return u, nil
}

func validatePushSubscription(sub webpush.Subscription, expires int64, now time.Time) error {
	if _, err := validatePushEndpoint(sub.Endpoint); err != nil {
		return err
	}
	key, err := base64.RawURLEncoding.DecodeString(sub.Keys.P256dh)
	if err != nil || len(key) != 65 {
		return errors.New("invalid push public key")
	}
	x, y := elliptic.Unmarshal(elliptic.P256(), key)
	if x == nil || y == nil {
		return errors.New("invalid push public key")
	}
	auth, err := base64.RawURLEncoding.DecodeString(sub.Keys.Auth)
	if err != nil || len(auth) != 16 {
		return errors.New("invalid push authentication key")
	}
	if expires < 0 || (expires != 0 && expires <= now.UnixMilli()) {
		return errors.New("push subscription has expired")
	}
	return nil
}

func publicPushIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	// GlobalUnicast includes reserved and documentation ranges. Reject them too.
	for _, blocked := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001:db8::/32", "2001::/32", "2002::/16"} {
		if netip.MustParsePrefix(blocked).Contains(ip) {
			return false
		}
	}
	return true
}

func guardedPushClient() *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{Proxy: nil, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 8 * time.Second,
		MaxResponseHeaderBytes: 16384, MaxConnsPerHost: 2, MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || port != "443" || !pushProviderHosts[host] {
			return nil, errors.New("push destination refused")
		}
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil || len(ips) == 0 || len(ips) > 32 {
			return nil, errors.New("push destination unavailable")
		}
		for _, ip := range ips {
			if !publicPushIP(ip) {
				return nil, errors.New("push destination refused")
			}
		}
		// Dial the checked literal. TLS still verifies the original hostname; a
		// second DNS lookup and environment proxy cannot bypass this check.
		for _, ip := range ips {
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			if ctx.Err() != nil {
				break
			}
		}
		return nil, errors.New("push destination unavailable")
	}
	return &http.Client{Transport: transport, Timeout: 12 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
