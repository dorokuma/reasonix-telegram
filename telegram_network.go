// GFW fallback transport for reasonix-telegram.
// When api.telegram.org is blocked by the GFW, automatically falls back to
// alternative IPs discovered via DoH or configured manually.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Seed fallback IPs for when DoH is also blocked.
var seedFallbackIPs = []string{"149.154.167.220"}

// DoH providers for IP discovery.
var dohProviders = []struct {
	url    string
	params string
}{
	{"https://dns.google/resolve", "name=api.telegram.org&type=A"},
	{"https://cloudflare-dns.com/dns-query", "name=api.telegram.org&type=A"},
}

type dohResponse struct {
	Answer []struct {
		Type int    `json:"type"`
		Data string `json:"data"`
	} `json:"answer"`
}

// discoverFallbackIPs queries DoH providers to find api.telegram.org IPs.
func discoverFallbackIPs() []string {
	seen := map[string]bool{}
	var ips []string

	// Start with seed IPs
	for _, ip := range seedFallbackIPs {
		if !seen[ip] {
			seen[ip] = true
			ips = append(ips, ip)
		}
	}

	client := &http.Client{Timeout: 5 * time.Second}
	for _, provider := range dohProviders {
		url := provider.url + "?" + provider.params
		req, err := http.NewRequestWithContext(context.Background(), "GET", url, nil)
		if err != nil {
			continue
		}
		req.Header.Set("accept", "application/dns-json")
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("DoH %s: %v", provider.url, err)
			continue
		}
		var dr dohResponse
		if err := json.NewDecoder(resp.Body).Decode(&dr); err != nil {
			resp.Body.Close()
			continue
		}
		resp.Body.Close()

		for _, a := range dr.Answer {
			if a.Type == 1 && !seen[a.Data] { // Type A = IPv4
				// Verify it's a valid IPv4 address
				if net.ParseIP(a.Data) != nil && net.ParseIP(a.Data).To4() != nil {
					seen[a.Data] = true
					ips = append(ips, a.Data)
				}
			}
		}
	}

	log.Printf("discovered Telegram fallback IPs: %v", ips)
	return ips
}

// TelegramFallbackTransport is an http.RoundTripper that tries normal DNS first,
// then falls back to a list of known IPs for api.telegram.org.
type TelegramFallbackTransport struct {
	inner       http.RoundTripper
	fallbackIPs []string
	stickyIP    string
	stickyOK    bool
	mu          sync.Mutex
	sanitizer   *TokenSanitizer
}

// NewTelegramFallbackTransport creates a transport with fallback IP discovery.
func NewTelegramFallbackTransport(inner http.RoundTripper, manualIPs string, sanitizer *TokenSanitizer) *TelegramFallbackTransport {
	t := &TelegramFallbackTransport{
		inner:     inner,
		sanitizer: sanitizer,
	}

	// Manual IPs from env var take precedence
	if manualIPs != "" {
		for _, s := range strings.Split(manualIPs, ",") {
			s = strings.TrimSpace(s)
			if s != "" {
				t.fallbackIPs = append(t.fallbackIPs, s)
			}
		}
		log.Printf("telegram fallback: using manual IPs: %v", t.fallbackIPs)
	} else {
		// Auto-discover via DoH
		t.fallbackIPs = discoverFallbackIPs()
	}

	return t
}

// RoundTrip implements http.RoundTripper.
func (t *TelegramFallbackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Only apply Telegram-specific fallback for api.telegram.org
	host := req.URL.Host
	if host == "" {
		host = req.Host
	}
	if host != "api.telegram.org" && !strings.HasSuffix(host, ".api.telegram.org") {
		return t.inner.RoundTrip(req)
	}

	// Try sticky IP first (last known working fallback)
	t.mu.Lock()
	sticky := t.stickyIP
	stickyOK := t.stickyOK
	t.mu.Unlock()

	if stickyOK && sticky != "" {
		resp, err := t.tryIP(req, sticky)
		if err == nil {
			return resp, nil
		}
		log.Printf("telegram fallback: sticky IP %s failed, trying fallback chain", sticky)
		t.mu.Lock()
		t.stickyOK = false
		t.mu.Unlock()
	}

	// Try normal DNS
	resp, err := t.inner.RoundTrip(req)
	if err == nil {
		return resp, nil
	}
	log.Printf("telegram fallback: normal DNS failed: %v", err)

	// Try fallback IPs
	for _, ip := range t.fallbackIPs {
		resp, err := t.tryIP(req, ip)
		if err == nil {
			// Remember this IP for future requests
			t.mu.Lock()
			t.stickyIP = ip
			t.stickyOK = true
			t.mu.Unlock()
			log.Printf("telegram fallback: using IP %s", ip)
			return resp, nil
		}
	}

	return nil, fmt.Errorf("all Telegram endpoints exhausted (tried %d fallback IPs)", len(t.fallbackIPs))
}

type bodyWithCloseHook struct {
	io.ReadCloser
	hook func()
}

func (b *bodyWithCloseHook) Close() error {
	err := b.ReadCloser.Close()
	b.hook()
	return err
}

// tryIP sends the request to a specific IP address while preserving the
// original Host header (for TLS SNI and virtual hosting).
func (t *TelegramFallbackTransport) tryIP(req *http.Request, ip string) (*http.Response, error) {
	clone := req.Clone(req.Context())
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	innerTransport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err == nil && (host == "api.telegram.org" || strings.HasSuffix(host, ".api.telegram.org")) {
				addr = ip + ":" + port
			}
			return dialer.DialContext(ctx, network, addr)
		},
	}

	// Wrap in token-redacting transport for consistent sanitization.
	transport := &tokenRedactingTransport{
		inner:     innerTransport,
		sanitizer: t.sanitizer,
	}

	resp, err := transport.RoundTrip(clone)
	if err != nil {
		innerTransport.CloseIdleConnections()
		return resp, t.sanitizer.SanitizeError(err)
	}
	resp.Body = &bodyWithCloseHook{
		ReadCloser: resp.Body,
		hook:       innerTransport.CloseIdleConnections,
	}
	return resp, nil
}
