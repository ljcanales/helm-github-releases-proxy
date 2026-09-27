package chart

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

func sortVersions(versions []string) {
	sort.SliceStable(versions, func(i, j int) bool { return compareVersions(versions[i], versions[j]) > 0 })
}

func compareVersions(left, right string) int {
	l, lok := parseVersion(left)
	r, rok := parseVersion(right)
	if lok && rok {
		if l.major != r.major {
			return sign(l.major - r.major)
		}
		if l.minor != r.minor {
			return sign(l.minor - r.minor)
		}
		if l.patch != r.patch {
			return sign(l.patch - r.patch)
		}
		if l.pre == r.pre {
			return 0
		}
		if l.pre == "" {
			return 1
		}
		if r.pre == "" {
			return -1
		}
		return comparePrerelease(l.pre, r.pre)
	}
	if lok != rok {
		if lok {
			return 1
		}
		return -1
	}
	return strings.Compare(left, right)
}

type parsedVersion struct {
	major, minor, patch int64
	pre                 string
}

var versionPattern = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$`)

func parseVersion(value string) (parsedVersion, bool) {
	match := versionPattern.FindStringSubmatch(value)
	if match == nil {
		return parsedVersion{}, false
	}
	var version parsedVersion
	if _, err := fmt.Sscan(match[1], &version.major); err != nil {
		return parsedVersion{}, false
	}
	if _, err := fmt.Sscan(match[2], &version.minor); err != nil {
		return parsedVersion{}, false
	}
	if _, err := fmt.Sscan(match[3], &version.patch); err != nil {
		return parsedVersion{}, false
	}
	version.pre = match[4]
	return version, true
}

func comparePrerelease(left, right string) int {
	leftParts, rightParts := strings.Split(left, "."), strings.Split(right, ".")
	for i := 0; i < len(leftParts) && i < len(rightParts); i++ {
		ln, le := parseNumericIdentifier(leftParts[i])
		rn, re := parseNumericIdentifier(rightParts[i])
		if le && re && ln != rn {
			return sign(ln - rn)
		}
		if le != re {
			if le {
				return -1
			}
			return 1
		}
		if leftParts[i] != rightParts[i] {
			return strings.Compare(leftParts[i], rightParts[i])
		}
	}
	return sign(int64(len(leftParts) - len(rightParts)))
}

func parseNumericIdentifier(value string) (int64, bool) {
	var number int64
	if value == "" || strings.Trim(value, "0123456789") != "" {
		return 0, false
	}
	if _, err := fmt.Sscan(value, &number); err != nil {
		return 0, false
	}
	return number, true
}

func sign(value int64) int {
	if value < 0 {
		return -1
	}
	if value > 0 {
		return 1
	}
	return 0
}
