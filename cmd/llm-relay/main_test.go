package main

import "testing"

// The ldflags stamp (-X main.version=…) must win over every fallback: that
// is the value the federation hello/hello_ack carries to both sides.
func TestBuildVersionPrefersStamp(t *testing.T) {
	old := version
	defer func() { version = old }()
	version = "v1.2.3-4-gabcdef"
	if got := buildVersion(); got != "v1.2.3-4-gabcdef" {
		t.Errorf("buildVersion() = %q, want the ldflags stamp", got)
	}
}

// An unstamped build must not report an empty or bogus version: `go test`
// compiles this package as a test binary whose build info says "(devel)",
// so the fallback chain lands on "dev".
func TestBuildVersionFallsBackToDev(t *testing.T) {
	old := version
	defer func() { version = old }()
	version = ""
	if got := buildVersion(); got != "dev" {
		t.Errorf("buildVersion() = %q, want dev", got)
	}
}

// The stamped version must be the value handed to fed.NewManager — the
// only path by which it reaches the Federation page.
func TestFedOptionsCarryBuildVersion(t *testing.T) {
	old := version
	defer func() { version = old }()
	version = "v4.5.6"
	o := fedOptions()
	if o == nil || o.Version != "v4.5.6" {
		t.Errorf("fedOptions() = %+v, want Version v4.5.6", o)
	}
}
