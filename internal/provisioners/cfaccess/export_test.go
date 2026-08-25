package cfaccess

import "testing"

// newWithBase builds a provisioner pointed at a test server instead of the real
// Cloudflare API.
func newWithBase(t *testing.T, base string, cfg Config) *Provisioner {
	t.Helper()
	p := New(cfg)
	p.baseURL = base
	return p
}
