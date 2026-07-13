package updater

import (
	"strconv"
	"strings"
)

// semver is a parsed semantic version. Only the fields the updater needs are
// tracked: major/minor/patch precedence plus an optional pre-release string.
// Build metadata (the part after '+') is parsed off and ignored for
// precedence, per the semver spec.
type semver struct {
	major, minor, patch int
	pre                 string
}

// parseVersion parses a version string such as "v1.2.3", "1.2.3-rc.1" or
// "v1.2.3+build.5" into a semver. A leading "v"/"V" is optional. It returns
// ok=false for anything that is not a MAJOR.MINOR.PATCH triple of integers -
// e.g. "dev", "" or a bare git sha - so callers can decline to compare
// non-release builds rather than guessing.
func parseVersion(v string) (semver, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return semver{}, false
	}
	if v[0] == 'v' || v[0] == 'V' {
		v = v[1:]
	}

	// Strip build metadata ("+...") - it never affects precedence.
	if i := strings.IndexByte(v, '+'); i >= 0 {
		v = v[:i]
	}

	// Split off the pre-release ("-...").
	var pre string
	if i := strings.IndexByte(v, '-'); i >= 0 {
		pre = v[i+1:]
		v = v[:i]
	}

	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return semver{}, false
	}

	nums := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return semver{}, false
		}
		nums[i] = n
	}

	return semver{major: nums[0], minor: nums[1], patch: nums[2], pre: pre}, true
}

// isReleaseVersion reports whether v is a clean release version (a valid
// MAJOR.MINOR.PATCH with no pre-release identifier). The updater only ever
// self-replaces when the *currently running* binary is a clean release, so a
// local dev build (e.g. "v1.2.3-4-gabcdef" from `git describe`, or "dev")
// is never clobbered by rolling it back to the latest published release.
func isReleaseVersion(v string) bool {
	sv, ok := parseVersion(v)
	return ok && sv.pre == ""
}

// compareVersions returns -1 if a < b, 0 if a == b, and +1 if a > b, using
// semantic-version precedence. Both inputs must be parseable; ok=false is
// returned otherwise so callers don't act on a bogus comparison.
func compareVersions(a, b string) (int, bool) {
	sa, oka := parseVersion(a)
	sb, okb := parseVersion(b)
	if !oka || !okb {
		return 0, false
	}
	return compareSemver(sa, sb), true
}

func compareSemver(a, b semver) int {
	if c := cmpInt(a.major, b.major); c != 0 {
		return c
	}
	if c := cmpInt(a.minor, b.minor); c != 0 {
		return c
	}
	if c := cmpInt(a.patch, b.patch); c != 0 {
		return c
	}
	return comparePre(a.pre, b.pre)
}

// comparePre implements semver pre-release precedence: a version with a
// pre-release has lower precedence than the associated normal version, and
// pre-release identifiers are compared field-by-field (numeric fields
// numerically, others lexically; numeric fields rank below non-numeric).
func comparePre(a, b string) int {
	switch {
	case a == "" && b == "":
		return 0
	case a == "": // no pre-release outranks any pre-release
		return 1
	case b == "":
		return -1
	}

	af := strings.Split(a, ".")
	bf := strings.Split(b, ".")
	for i := 0; i < len(af) && i < len(bf); i++ {
		if c := comparePreField(af[i], bf[i]); c != 0 {
			return c
		}
	}
	// All shared fields equal: the one with more fields has higher precedence.
	return cmpInt(len(af), len(bf))
}

func comparePreField(a, b string) int {
	an, aerr := strconv.Atoi(a)
	bn, berr := strconv.Atoi(b)
	aNum, bNum := aerr == nil, berr == nil

	switch {
	case aNum && bNum:
		return cmpInt(an, bn)
	case aNum: // numeric identifiers always rank lower than alphanumeric
		return -1
	case bNum:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
