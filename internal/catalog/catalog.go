// Package catalog owns transport-independent chart identity and aggregate index rules.
package catalog

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

type ChartVersion struct {
	APIVersion  string
	Name        string
	Version     string
	AppVersion  string
	Description string
	Home        string
	Icon        string
	Deprecated  *bool
	Keywords    []string
	Maintainers []Maintainer
	Sources     []string
	Annotations map[string]string
	URLs        []string
	Created     string
	Digest      string
}

type Maintainer struct {
	Name  string
	Email string
	URL   string
}

// Identity identifies a chart version, independently of any package bytes.
type Identity struct{ Name, Version string }

func (v ChartVersion) Identity() Identity { return Identity{Name: v.Name, Version: v.Version} }

// AggregateIndex merges chart versions in contribution order. The first
// contribution owns all metadata, including absent values and the digest.
// Callers contribute GitHub charts before local charts to retain precedence.
type AggregateIndex map[string]map[string]*ChartVersion

// Add retains first-contribution metadata and appends distinct package URLs.
func (index AggregateIndex) Add(version ChartVersion) {
	identity := version.Identity()
	if index[identity.Name] == nil {
		index[identity.Name] = make(map[string]*ChartVersion)
	}
	entry := index[identity.Name][identity.Version]
	if entry == nil {
		copy := version
		copy.URLs = nil
		entry = &copy
		index[identity.Name][identity.Version] = entry
	}
	for _, chartURL := range version.URLs {
		if !containsString(entry.URLs, chartURL) {
			entry.URLs = append(entry.URLs, chartURL)
		}
	}
}

// Merge accepts an already validated source contribution.
func (index AggregateIndex) Merge(source AggregateIndex) {
	for _, versions := range source {
		for _, version := range versions {
			index.Add(*version)
		}
	}
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func (source AggregateIndex) Finalize() map[string][]ChartVersion {
	result := make(map[string][]ChartVersion, len(source))
	for name, byVersion := range source {
		versions := make([]string, 0, len(byVersion))
		for version := range byVersion {
			versions = append(versions, version)
		}
		sort.SliceStable(versions, func(i, j int) bool { return compareVersions(versions[i], versions[j]) > 0 })
		result[name] = make([]ChartVersion, 0, len(versions))
		for _, version := range versions {
			result[name] = append(result[name], *byVersion[version])
		}
	}
	return result
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
