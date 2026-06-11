package main

import "testing"

func TestResolveConfigDefaultsToHostedEndpoint(t *testing.T) {
	// Nothing configured anywhere → default to the hosted instance so that a
	// bare `pods login` just works.
	cfg := resolveConfig("", "", func(string) string { return "" }, config{})
	if cfg.Endpoint != defaultEndpoint {
		t.Fatalf("endpoint = %q, want default %q", cfg.Endpoint, defaultEndpoint)
	}
}
