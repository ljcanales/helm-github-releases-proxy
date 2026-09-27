// Package chart owns transport-independent chart metadata, identity, and aggregation rules.
package chart

type Version struct {
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

// Identity identifies chart metadata independently of the package bytes that supply it.
type Identity struct{ Name, Version string }

func (v Version) Identity() Identity { return Identity{Name: v.Name, Version: v.Version} }

// Aggregate merges versions in contribution order. The first contribution owns
// metadata, while distinct package URLs are appended in source order.
type Aggregate map[string]map[string]*Version

func (aggregate Aggregate) Add(version Version) {
	identity := version.Identity()
	if aggregate[identity.Name] == nil {
		aggregate[identity.Name] = make(map[string]*Version)
	}
	entry := aggregate[identity.Name][identity.Version]
	if entry == nil {
		copy := cloneVersion(version)
		copy.URLs = nil
		entry = &copy
		aggregate[identity.Name][identity.Version] = entry
	}
	for _, packageURL := range version.URLs {
		if !contains(entry.URLs, packageURL) {
			entry.URLs = append(entry.URLs, packageURL)
		}
	}
}

func (aggregate Aggregate) Finalize() map[string][]Version {
	result := make(map[string][]Version, len(aggregate))
	for name, byVersion := range aggregate {
		versions := make([]string, 0, len(byVersion))
		for value := range byVersion {
			versions = append(versions, value)
		}
		sortVersions(versions)
		result[name] = make([]Version, 0, len(versions))
		for _, value := range versions {
			result[name] = append(result[name], cloneVersion(*byVersion[value]))
		}
	}
	return result
}

func cloneVersion(version Version) Version {
	version.URLs = append([]string(nil), version.URLs...)
	version.Keywords = append([]string(nil), version.Keywords...)
	version.Sources = append([]string(nil), version.Sources...)
	version.Maintainers = append([]Maintainer(nil), version.Maintainers...)
	if version.Deprecated != nil {
		deprecated := *version.Deprecated
		version.Deprecated = &deprecated
	}
	if version.Annotations != nil {
		annotations := make(map[string]string, len(version.Annotations))
		for key, value := range version.Annotations {
			annotations[key] = value
		}
		version.Annotations = annotations
	}
	return version
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
