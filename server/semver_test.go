package main

import "testing"

func TestParseSemVer(t *testing.T) {
	for _, tt := range []struct {
		raw, want string
	}{
		{"1.0.0", "1.0.0"},
		{"0.1.2", "0.1.2"},
		{"1.2.3-rc.1", "1.2.3-rc.1"},
		{"1.2.3-alpha-1.0", "1.2.3-alpha-1.0"},
		{"1.2.3+build.5", "1.2.3"},
		{"1.2.3-rc.1+sha.abc", "1.2.3-rc.1"},
	} {
		v, err := parseSemVer(tt.raw)
		if err != nil {
			t.Fatalf("parseSemVer(%q): %v", tt.raw, err)
		}
		if v.String() != tt.want {
			t.Errorf("parseSemVer(%q) = %s, want %s", tt.raw, v, tt.want)
		}
	}
	for _, raw := range []string{"", "1", "1.0", "1.0.0.0", "01.0.0", "1.00.0", "v1.0.0",
		"1.0.0-", "1.0.0-01", "1.0.0-a..b", "1.0.0+", "1.0.0-a_b", "1.0.0 ", "-1.0.0"} {
		if _, err := parseSemVer(raw); err == nil {
			t.Errorf("parseSemVer(%q) accepted an invalid version", raw)
		}
	}
	if _, err := parseRegistryVersion("1.0.0+build"); err == nil {
		t.Error("parseRegistryVersion accepted build metadata")
	}
}

func TestCompareSemVerPrecedence(t *testing.T) {
	// The SemVer 2.0 specification's precedence example, ascending.
	ordered := []string{"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta",
		"1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "1.0.1", "1.1.0", "1.10.0", "2.0.0"}
	for i := range ordered {
		for j := range ordered {
			a, _ := parseSemVer(ordered[i])
			b, _ := parseSemVer(ordered[j])
			got := compareSemVer(a, b)
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got != want {
				t.Errorf("compare(%s, %s) = %d, want %d", ordered[i], ordered[j], got, want)
			}
		}
	}
}
