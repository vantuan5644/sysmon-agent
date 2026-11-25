package main

// The test binary's default allowlist also accepts the reserved names the
// tests address the handler by: httptest.NewRequest's default Host
// example.com (RFC 2606) and names under .example such as
// sysmon.tailnet.example. Production never builds this list: main sets an
// explicit allowlist, and host_guard_test.go checks the real defaults on
// allowlists built with newHostAllowlist, not this one.
func init() {
	defaultHostAllowlist = func() *hostAllowlist {
		return newHostAllowlist(osHostnames(), "", "", []string{"example.com", "*.example"})
	}
}
