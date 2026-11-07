package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const maxTypeVersionLength = 64

// semVersion is a parsed SemVer 2.0 version without build metadata, which
// is not part of a type version's identity.
type semVersion struct {
	Major, Minor, Patch uint64
	Pre                 []string
}

func (v semVersion) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if len(v.Pre) > 0 {
		s += "-" + strings.Join(v.Pre, ".")
	}
	return s
}

func (v semVersion) Prerelease() bool { return len(v.Pre) > 0 }

// parseSemVer parses a SemVer 2.0 string, accepting and dropping build
// metadata.
func parseSemVer(raw string) (semVersion, error) {
	if raw == "" || len(raw) > maxTypeVersionLength {
		return semVersion{}, fmt.Errorf("version must be SemVer 2.0 and at most %d characters", maxTypeVersionLength)
	}
	core := raw
	if i := strings.IndexByte(core, '+'); i >= 0 {
		build := core[i+1:]
		core = core[:i]
		if err := checkSemVerIdentifiers(build, false); err != nil {
			return semVersion{}, fmt.Errorf("version %q has invalid build metadata", raw)
		}
	}
	var pre string
	if i := strings.IndexByte(core, '-'); i >= 0 {
		pre = core[i+1:]
		core = core[:i]
		if err := checkSemVerIdentifiers(pre, true); err != nil {
			return semVersion{}, fmt.Errorf("version %q has an invalid prerelease", raw)
		}
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return semVersion{}, fmt.Errorf("version %q must be MAJOR.MINOR.PATCH", raw)
	}
	var nums [3]uint64
	for i, part := range parts {
		if !semVerNumeric(part) {
			return semVersion{}, fmt.Errorf("version %q must be MAJOR.MINOR.PATCH without leading zeros", raw)
		}
		n, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return semVersion{}, fmt.Errorf("version %q has an out-of-range number", raw)
		}
		nums[i] = n
	}
	v := semVersion{Major: nums[0], Minor: nums[1], Patch: nums[2]}
	if pre != "" {
		v.Pre = strings.Split(pre, ".")
	}
	return v, nil
}

// parseRegistryVersion parses a version named by an API caller, which must
// already be the registry form: build metadata is refused rather than
// dropped, so a caller cannot believe it selected a build.
func parseRegistryVersion(raw string) (semVersion, error) {
	if strings.Contains(raw, "+") {
		return semVersion{}, errors.New("version must not carry build metadata")
	}
	return parseSemVer(raw)
}

func checkSemVerIdentifiers(s string, prerelease bool) error {
	if s == "" {
		return errors.New("empty")
	}
	for _, id := range strings.Split(s, ".") {
		if id == "" {
			return errors.New("empty identifier")
		}
		for _, r := range id {
			if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '-') {
				return errors.New("invalid character")
			}
		}
		if prerelease && allDigits(id) && len(id) > 1 && id[0] == '0' {
			return errors.New("leading zero")
		}
	}
	return nil
}

func semVerNumeric(s string) bool {
	return s != "" && allDigits(s) && (len(s) == 1 || s[0] != '0')
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// compareSemVer orders versions by SemVer 2.0 precedence.
func compareSemVer(a, b semVersion) int {
	for _, pair := range [][2]uint64{{a.Major, b.Major}, {a.Minor, b.Minor}, {a.Patch, b.Patch}} {
		if pair[0] != pair[1] {
			if pair[0] < pair[1] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a.Pre) == 0 && len(b.Pre) == 0:
		return 0
	case len(a.Pre) == 0:
		return 1
	case len(b.Pre) == 0:
		return -1
	}
	for i := 0; i < len(a.Pre) && i < len(b.Pre); i++ {
		if c := comparePrereleaseIdentifier(a.Pre[i], b.Pre[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(a.Pre) < len(b.Pre):
		return -1
	case len(a.Pre) > len(b.Pre):
		return 1
	}
	return 0
}

func comparePrereleaseIdentifier(a, b string) int {
	aNum, bNum := allDigits(a), allDigits(b)
	switch {
	case aNum && bNum:
		if len(a) != len(b) {
			if len(a) < len(b) {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	case aNum:
		return -1
	case bNum:
		return 1
	}
	return strings.Compare(a, b)
}
