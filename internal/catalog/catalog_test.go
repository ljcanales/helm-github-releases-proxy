package catalog_test

import (
	"helm-github-releases-proxy/internal/catalog"
	"reflect"
	"testing"
)

func TestAggregateIndexMergesChartVersionsPreservingFirstMetadata(t *testing.T) {
	entries := make(catalog.AggregateIndex)
	entries.Add(catalog.ChartVersion{Name: "demo", Version: "1.0.0", Digest: "github", Description: "first", URLs: []string{"github", "github"}})
	entries.Add(catalog.ChartVersion{Name: "demo", Version: "1.0.0", Digest: "local", Description: "second", URLs: []string{"local", "github"}})
	got := entries.Finalize()["demo"]
	if len(got) != 1 || got[0].Digest != "github" || got[0].Description != "first" || !reflect.DeepEqual(got[0].URLs, []string{"github", "local"}) {
		t.Fatalf("merged chart version: %#v", got)
	}
}

func TestAggregateIndexKeepsAbsentFirstMetadataWhenMergingSources(t *testing.T) {
	aggregate := make(catalog.AggregateIndex)
	aggregate.Add(catalog.ChartVersion{Name: "demo", Version: "1.0.0", URLs: []string{"github"}})
	source := make(catalog.AggregateIndex)
	source.Add(catalog.ChartVersion{Name: "demo", Version: "1.0.0", Digest: "local", Created: "later", URLs: []string{"github", "local"}})
	source.Add(catalog.ChartVersion{Name: "other", Version: "2.0.0", Digest: "other", URLs: []string{"other"}})
	aggregate.Merge(source)
	got := aggregate.Finalize()
	if got["demo"][0].Digest != "" || got["demo"][0].Created != "" || !reflect.DeepEqual(got["demo"][0].URLs, []string{"github", "local"}) || got["other"][0].Digest != "other" {
		t.Fatalf("merged sources: %#v", got)
	}
}

func TestAggregateIndexOrdersVersionsUsingExistingPolicy(t *testing.T) {
	cases := []struct{ name, newer, older string }{
		{"major", "2.0.0", "1.99.99"},
		{"minor", "1.10.0", "1.9.99"},
		{"patch", "1.0.10", "1.0.9"},
		{"stable before prerelease", "1.0.0", "1.0.0-rc.1"},
		{"numeric prerelease", "1.0.0-rc.10", "1.0.0-rc.2"},
		{"numeric before text", "1.0.0-alpha", "1.0.0-9"},
		{"prerelease length", "1.0.0-alpha.1", "1.0.0-alpha"},
		{"text prerelease", "1.0.0-beta", "1.0.0-alpha"},
		{"valid before invalid", "0.0.0", "zzz"},
		{"invalid lexical fallback", "zzz", "aaa"},
		{"leading v", "v2.0.0", "1.0.0"},
		{"overflow falls back to invalid", "1.0.0", "999999999999999999999.0.0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			aggregate := make(catalog.AggregateIndex)
			aggregate.Add(catalog.ChartVersion{Name: "demo", Version: tc.older})
			aggregate.Add(catalog.ChartVersion{Name: "demo", Version: tc.newer})
			got := aggregate.Finalize()["demo"]
			if len(got) != 2 || got[0].Version != tc.newer || got[1].Version != tc.older {
				t.Fatalf("ordered versions: %#v", got)
			}
		})
	}
}

func TestAggregateIndexKeepsDistinctVersionSpellingsAndPackageBytes(t *testing.T) {
	aggregate := make(catalog.AggregateIndex)
	for _, version := range []string{"1.0.0", "v1.0.0", "1.0.0+build.1", "1.0.0+build.2"} {
		aggregate.Add(catalog.ChartVersion{Name: "demo", Version: version, Digest: version})
	}
	got := aggregate.Finalize()["demo"]
	if len(got) != 4 {
		t.Fatalf("distinct versions: %#v", got)
	}
	for _, version := range got {
		if version.Digest != version.Version {
			t.Fatalf("metadata changed: %#v", version)
		}
	}
	// Equal comparator precedence does not imply identity equality or prescribe
	// ordering between equal-precedence spellings.
}
