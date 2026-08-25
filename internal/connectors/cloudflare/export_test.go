package cloudflare

import "testing"

// newWithBase builds a connector pointed at a test server instead of the real
// Cloudflare API.
func newWithBase(t *testing.T, base string, cfg Config) *Connector {
	t.Helper()
	c := New(cfg)
	c.api.baseURL = base
	return c
}

// newDNSWithBase builds a DNS provisioner pointed at a test server instead of
// the real Cloudflare API.
func newDNSWithBase(t *testing.T, base string, cfg DNSConfig) *DNSProvisioner {
	t.Helper()
	p := NewDNS(cfg)
	p.api.baseURL = base
	return p
}
