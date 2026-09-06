package pkrkit

import (
	"strings"
)

// tryParseVersion normalizes a version string into a comparable (major, minor,
// patch, prerelease) triple. Accepts an optional leading v/V and pads short
// triples. Returns ok=false when the core is not numeric.
func tryParseVersion(raw string) (major, minor, patch int, prerelease string, ok bool) {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, "v") || strings.HasPrefix(s, "V") {
		s = s[1:]
	}
	if s == "" {
		return 0, 0, 0, "", false
	}
	core := s
	prerelease = ""
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		core = s[:i]
		prerelease = s[i:]
	}
	parts := strings.Split(core, ".")
	// Accepts any width; we take up to 3.
	var nums []int
	for _, p := range parts {
		if p == "" {
			return 0, 0, 0, "", false
		}
		var n int
		for _, c := range p {
			if c < '0' || c > '9' {
				return 0, 0, 0, "", false
			}
			n = n*10 + int(c-'0')
		}
		nums = append(nums, n)
	}
	if len(nums) == 0 || len(nums) > 3 {
		return 0, 0, 0, "", false
	}
	// Left pad missing to width 3.
	for len(nums) < 3 {
		nums = append(nums, 0)
	}
	return nums[0], nums[1], nums[2], prerelease, true
}

// compareVersions returns -1/0/1 comparing two version strings semantically.
// Non-parseable entries are compared lexically so the total order is stable.
func compareVersions(a, b string) int {
	maj, min, pat, pre, okA := tryParseVersion(a)
	majB, minB, patB, preB, okB := tryParseVersion(b)
	if !okA || !okB {
		// Fall back to lexical (matches Rust's compare()).
		return strings.Compare(a, b)
	}
	if maj != majB {
		return cmp(maj, majB)
	}
	if min != minB {
		return cmp(min, minB)
	}
	if pat != patB {
		return cmp(pat, patB)
	}
	// Prerelease: a version WITHOUT prerelease is higher than one with it.
	if pre == "" && preB != "" {
		return 1
	}
	if pre != "" && preB == "" {
		return -1
	}
	return strings.Compare(pre, preB)
}

func cmp(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// HighestVersion returns the highest semantic version among versions, or "".
func HighestVersion(versions []string) string {
	var best string
	for _, v := range versions {
		if v == "" {
			continue
		}
		if best == "" || compareVersions(v, best) > 0 {
			best = v
		}
	}
	return best
}

// SortSemver sorts version strings in ascending semantic order (stable).
func SortSemver(versions []string) {
	// Simple insertion sort on the semantic key, stable for equal keys.
	for i := 1; i < len(versions); i++ {
		for j := i; j > 0 && compareVersions(versions[j], versions[j-1]) < 0; j-- {
			versions[j], versions[j-1] = versions[j-1], versions[j]
		}
	}
}
