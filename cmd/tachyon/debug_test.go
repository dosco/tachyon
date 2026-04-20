package main

import "testing"

// TestIsLoopbackAddr guards the critical refusal-to-bind rule: a debug
// endpoint on a reachable interface would leak goroutine dumps and
// CPU profiles to the network. Anything but 127.0.0.1 / ::1 / localhost
// (case-insensitive) with a port must fail.
func TestIsLoopbackAddr(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:6060", true},
		{"127.0.0.1:1", true},
		{"localhost:6060", true},
		{"LOCALHOST:6060", true},
		{"[::1]:6060", true},
		{"", false},
		{":6060", false},                  // missing host — implicit 0.0.0.0, rejected
		{"0.0.0.0:6060", false},
		{"10.0.0.1:6060", false},
		{"192.168.1.1:6060", false},
		{"example.com:6060", false},
		{"127.0.0.1", false},              // no port at all — SplitHostPort fails
		{"127.0.0.1:", true},              // SplitHostPort accepts empty port; host is still loopback
	}
	for _, tc := range tests {
		if got := isLoopbackAddr(tc.addr); got != tc.want {
			t.Errorf("isLoopbackAddr(%q) = %v; want %v", tc.addr, got, tc.want)
		}
	}
}
