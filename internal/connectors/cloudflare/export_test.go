package cloudflare

import "testing"

// newWithBase builds a connector pointed at a test server instead of the real
// Cloudflare API.
func newWithBase(t *testing.T, base string, cfg Config) *Connector {
	t.Helper()
	c := New(cfg)
	c.baseURL = base
	return c
}
