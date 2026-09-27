package chart_test

import (
	"reflect"
	"testing"

	"helm-github-releases-proxy/internal/chart"
)

func TestAggregatePreservesChartRulesAcrossSourceContributions(t *testing.T) {
	aggregate := make(chart.Aggregate)
	aggregate.Add(chart.Version{Name: "widget", Version: "1.0.0", Description: "first", Digest: "first-digest", URLs: []string{"charts/first/widget.tgz"}})
	aggregate.Add(chart.Version{Name: "widget", Version: "1.0.0", Description: "second", Digest: "second-digest", URLs: []string{"charts/second/widget.tgz", "charts/first/widget.tgz"}})
	aggregate.Add(chart.Version{Name: "widget", Version: "2.0.0-rc.1", URLs: []string{"charts/first/widget-rc.tgz"}})
	aggregate.Add(chart.Version{Name: "widget", Version: "2.0.0", URLs: []string{"charts/first/widget-2.tgz"}})

	versions := aggregate.Finalize()["widget"]
	if got := []string{versions[0].Version, versions[1].Version, versions[2].Version}; !reflect.DeepEqual(got, []string{"2.0.0", "2.0.0-rc.1", "1.0.0"}) {
		t.Fatalf("version order = %#v", got)
	}
	merged := versions[2]
	if merged.Description != "first" || merged.Digest != "first-digest" {
		t.Fatalf("metadata precedence = %#v", merged)
	}
	if !reflect.DeepEqual(merged.URLs, []string{"charts/first/widget.tgz", "charts/second/widget.tgz"}) {
		t.Fatalf("package URL order = %#v", merged.URLs)
	}
}
