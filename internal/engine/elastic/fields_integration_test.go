//go:build integration

package elastic

import (
	"os"
	"testing"
	"time"

	"github.com/ericmustin/vern/internal/otlpin"
)

const sensitiveFixture = "../../../testdata/sensitive/canaries.jsonl"

// TestFieldsLive loads the canary fixture and discovers its fields on a real
// Elasticsearch; run with -v to print the groups.
func TestFieldsLive(t *testing.T) {
	f, err := os.Open(sensitiveFixture)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	s, err := otlpin.Decode([]otlpin.Input{{Name: sensitiveFixture, R: f}}, otlpin.DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	otlpin.Shift(s, otlpin.OffsetToNow(otlpin.Summarize(s), time.Now().Add(-time.Minute)))
	e := loadTestEngine(t)
	id := loadTestRunID(t, e)
	if _, err := e.Load(t.Context(), id, s); err != nil {
		t.Fatal(err)
	}
	fl, err := e.Fields(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range []struct {
		name string
		v    []string
	}{{"Resource", fl.Resource}, {"Scope", fl.Scope}, {"Span", fl.Span}, {"SpanEvent", fl.SpanEvent},
		{"Log", fl.Log}, {"Metric", fl.Metric}, {"MetricNames", fl.MetricNames}} {
		t.Logf("%s (%d): %v", g.name, len(g.v), g.v)
	}
	t.Logf("Dropped: %v", fl.Dropped)
	if len(fl.Resource) == 0 || len(fl.Span) == 0 || len(fl.Log) == 0 || len(fl.Metric) == 0 || len(fl.MetricNames) == 0 {
		t.Errorf("empty group in %+v", fl)
	}
}
