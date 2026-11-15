package otlpout

import (
	"sort"
	"strconv"

	"github.com/ericmustin/vern/internal/evaluate"
	"github.com/ericmustin/vern/internal/score"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
)

const scopeName = "github.com/ericmustin/vern"

// Build converts a report to gauge metrics. The scored service is a data point
// attribute, not the resource service.name, because the producer is vern.
func Build(rep *evaluate.Report, c Config) pmetric.Metrics {
	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	res := rm.Resource().Attributes()
	res.PutStr("service.name", "vern")
	res.PutStr("service.version", c.ServiceVersion)
	res.PutStr("instrumentation_score.spec.version", rep.SpecVersion)
	keys := make([]string, 0, len(c.ResourceAttributes))
	for k := range c.ResourceAttributes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		res.PutStr(k, c.ResourceAttributes[k])
	}
	sm := rm.ScopeMetrics().AppendEmpty()
	sm.Scope().SetName(scopeName)
	sm.Scope().SetVersion(c.ServiceVersion)
	ts := pcommon.NewTimestampFromTime(rep.EvaluatedAt)

	if rep.Score != nil && rep.Score.Scored {
		m := gauge(sm, "instrumentation_score", "{score}")
		p := m.Gauge().DataPoints().AppendEmpty()
		p.SetTimestamp(ts)
		p.SetDoubleValue(round1(rep.Score.Overall))
		p.Attributes().PutStr("instrumentation_score.scope", "overall")
		p.Attributes().PutStr("instrumentation_score.category", rep.Score.Category)
		for _, s := range rep.Score.Services {
			p := m.Gauge().DataPoints().AppendEmpty()
			p.SetTimestamp(ts)
			p.SetDoubleValue(round1(s.Score))
			p.Attributes().PutStr("instrumentation_score.scope", "service")
			p.Attributes().PutStr("instrumentation_score.service.name", s.ServiceName)
			p.Attributes().PutStr("instrumentation_score.category", s.Category)
		}
	}

	passed := gauge(sm, "instrumentation_score.rule.passed", "1")
	counts := map[evaluate.RuleStatus]int{}
	var order []evaluate.RuleStatus
	for _, r := range rep.Rules {
		if counts[r.Status] == 0 {
			order = append(order, r.Status)
		}
		counts[r.Status]++
		if r.Status != evaluate.StatusEvaluated {
			continue
		}
		for _, o := range r.Outcomes {
			p := passed.Gauge().DataPoints().AppendEmpty()
			p.SetTimestamp(ts)
			v := int64(0)
			if o.Passed {
				v = 1
			}
			p.SetIntValue(v)
			a := p.Attributes()
			a.PutStr("instrumentation_score.rule.id", r.RuleID)
			a.PutStr("instrumentation_score.rule.impact", r.Impact)
			a.PutStr("instrumentation_score.service.name", o.ServiceName)
			a.PutBool("instrumentation_score.rule.scored", r.Scored)
		}
	}

	if passed.Gauge().DataPoints().Len() == 0 {
		sm.Metrics().RemoveIf(func(m pmetric.Metric) bool { return m.Name() == passed.Name() })
	}
	rules := gauge(sm, "instrumentation_score.rules", "{rule}")
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	for _, st := range order {
		p := rules.Gauge().DataPoints().AppendEmpty()
		p.SetTimestamp(ts)
		p.SetIntValue(int64(counts[st]))
		p.Attributes().PutStr("instrumentation_score.rule.status", string(st))
	}
	return md
}

func gauge(sm pmetric.ScopeMetrics, name, unit string) pmetric.Metric {
	m := sm.Metrics().AppendEmpty()
	m.SetName(name)
	m.SetUnit(unit)
	m.SetEmptyGauge()
	return m
}

// round1 rounds like the reports and the gate (one decimal place).
func round1(v float64) float64 {
	f, err := strconv.ParseFloat(score.Format(v), 64)
	if err != nil {
		return v
	}
	return f
}
