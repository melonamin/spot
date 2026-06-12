package main

import "testing"

func testTrustedProxies(t *testing.T) *TrustedProxies {
	t.Helper()
	trusted, err := NewTrustedProxies("192.0.2.0/24,127.0.0.1/32,::1/128")
	if err != nil {
		t.Fatal(err)
	}
	return trusted
}
