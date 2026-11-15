package elastic

import (
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// maxBody is Elasticsearch's request body limit for the OTLP endpoints.
const maxBody = 8 << 20

// unitSlack covers the tag and length prefix a unit gets when merged into a
// batch.
const unitSlack = 8

// packer greedily merges units (each a pdata value holding one resource) into
// batches whose estimated size stays within limit. A unit larger than the
// limit becomes a batch of its own.
type packer[T any] struct {
	limit   int
	size    func(T) int
	merge   func(dst, src T)
	newT    func() T
	flush   func(T) error
	cur     T
	curSize int
	n       int
}

func (p *packer[T]) add(u T) error {
	sz := p.size(u) + unitSlack
	if p.n > 0 && p.curSize+sz > p.limit {
		if err := p.close(); err != nil {
			return err
		}
	}
	if p.n == 0 {
		p.cur = p.newT()
	}
	p.merge(p.cur, u)
	p.curSize += sz
	p.n++
	return nil
}

func (p *packer[T]) close() error {
	if p.n == 0 {
		return nil
	}
	cur := p.cur
	p.n, p.curSize = 0, 0
	return p.flush(cur)
}

// feedTraces yields one unit per resource, or, when a resource alone exceeds
// limit, one unit per span with the resource and scope copied.
func feedTraces(td ptrace.Traces, limit int, add func(ptrace.Traces) error) error {
	var m ptrace.ProtoMarshaler
	for i := 0; i < td.ResourceSpans().Len(); i++ {
		rs := td.ResourceSpans().At(i)
		u := ptrace.NewTraces()
		rs.CopyTo(u.ResourceSpans().AppendEmpty())
		if m.TracesSize(u) <= limit {
			if err := add(u); err != nil {
				return err
			}
			continue
		}
		for j := 0; j < rs.ScopeSpans().Len(); j++ {
			ss := rs.ScopeSpans().At(j)
			for k := 0; k < ss.Spans().Len(); k++ {
				u := ptrace.NewTraces()
				drs := u.ResourceSpans().AppendEmpty()
				rs.Resource().CopyTo(drs.Resource())
				drs.SetSchemaUrl(rs.SchemaUrl())
				dss := drs.ScopeSpans().AppendEmpty()
				ss.Scope().CopyTo(dss.Scope())
				dss.SetSchemaUrl(ss.SchemaUrl())
				ss.Spans().At(k).CopyTo(dss.Spans().AppendEmpty())
				if err := add(u); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// feedLogs is feedTraces for log records.
func feedLogs(ld plog.Logs, limit int, add func(plog.Logs) error) error {
	var m plog.ProtoMarshaler
	for i := 0; i < ld.ResourceLogs().Len(); i++ {
		rl := ld.ResourceLogs().At(i)
		u := plog.NewLogs()
		rl.CopyTo(u.ResourceLogs().AppendEmpty())
		if m.LogsSize(u) <= limit {
			if err := add(u); err != nil {
				return err
			}
			continue
		}
		for j := 0; j < rl.ScopeLogs().Len(); j++ {
			sl := rl.ScopeLogs().At(j)
			for k := 0; k < sl.LogRecords().Len(); k++ {
				u := plog.NewLogs()
				drl := u.ResourceLogs().AppendEmpty()
				rl.Resource().CopyTo(drl.Resource())
				drl.SetSchemaUrl(rl.SchemaUrl())
				dsl := drl.ScopeLogs().AppendEmpty()
				sl.Scope().CopyTo(dsl.Scope())
				dsl.SetSchemaUrl(sl.SchemaUrl())
				sl.LogRecords().At(k).CopyTo(dsl.LogRecords().AppendEmpty())
				if err := add(u); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// feedMetrics yields one unit per resource; an oversized resource is split
// per metric, and an oversized metric per data point.
func feedMetrics(md pmetric.Metrics, limit int, add func(pmetric.Metrics) error) error {
	var m pmetric.ProtoMarshaler
	for i := 0; i < md.ResourceMetrics().Len(); i++ {
		rm := md.ResourceMetrics().At(i)
		u := pmetric.NewMetrics()
		rm.CopyTo(u.ResourceMetrics().AppendEmpty())
		if m.MetricsSize(u) <= limit {
			if err := add(u); err != nil {
				return err
			}
			continue
		}
		for j := 0; j < rm.ScopeMetrics().Len(); j++ {
			sm := rm.ScopeMetrics().At(j)
			for k := 0; k < sm.Metrics().Len(); k++ {
				metric := sm.Metrics().At(k)
				wrap := func(mt pmetric.Metric) pmetric.Metrics {
					u := pmetric.NewMetrics()
					drm := u.ResourceMetrics().AppendEmpty()
					rm.Resource().CopyTo(drm.Resource())
					drm.SetSchemaUrl(rm.SchemaUrl())
					dsm := drm.ScopeMetrics().AppendEmpty()
					sm.Scope().CopyTo(dsm.Scope())
					dsm.SetSchemaUrl(sm.SchemaUrl())
					mt.CopyTo(dsm.Metrics().AppendEmpty())
					return u
				}
				whole := wrap(metric)
				if m.MetricsSize(whole) <= limit {
					if err := add(whole); err != nil {
						return err
					}
					continue
				}
				if err := eachPointMetric(metric, func(one pmetric.Metric) error { return add(wrap(one)) }); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// eachPointMetric yields, for every data point of m, a metric holding only
// that point. The skeleton (name, unit, temporality, ...) is built once and
// each yielded metric copies the skeleton plus one point, so the work is
// linear in the number of points.
func eachPointMetric(m pmetric.Metric, yield func(pmetric.Metric) error) error {
	skeleton := func() pmetric.Metric {
		nm := pmetric.NewMetric()
		nm.SetName(m.Name())
		nm.SetDescription(m.Description())
		nm.SetUnit(m.Unit())
		m.Metadata().CopyTo(nm.Metadata())
		return nm
	}
	switch m.Type() {
	case pmetric.MetricTypeGauge:
		dps := m.Gauge().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			nm := skeleton()
			dps.At(i).CopyTo(nm.SetEmptyGauge().DataPoints().AppendEmpty())
			if err := yield(nm); err != nil {
				return err
			}
		}
	case pmetric.MetricTypeSum:
		dps := m.Sum().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			nm := skeleton()
			sum := nm.SetEmptySum()
			sum.SetAggregationTemporality(m.Sum().AggregationTemporality())
			sum.SetIsMonotonic(m.Sum().IsMonotonic())
			dps.At(i).CopyTo(sum.DataPoints().AppendEmpty())
			if err := yield(nm); err != nil {
				return err
			}
		}
	case pmetric.MetricTypeHistogram:
		dps := m.Histogram().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			nm := skeleton()
			h := nm.SetEmptyHistogram()
			h.SetAggregationTemporality(m.Histogram().AggregationTemporality())
			dps.At(i).CopyTo(h.DataPoints().AppendEmpty())
			if err := yield(nm); err != nil {
				return err
			}
		}
	case pmetric.MetricTypeExponentialHistogram:
		dps := m.ExponentialHistogram().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			nm := skeleton()
			h := nm.SetEmptyExponentialHistogram()
			h.SetAggregationTemporality(m.ExponentialHistogram().AggregationTemporality())
			dps.At(i).CopyTo(h.DataPoints().AppendEmpty())
			if err := yield(nm); err != nil {
				return err
			}
		}
	case pmetric.MetricTypeSummary:
		dps := m.Summary().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			nm := skeleton()
			dps.At(i).CopyTo(nm.SetEmptySummary().DataPoints().AppendEmpty())
			if err := yield(nm); err != nil {
				return err
			}
		}
	default:
		return yield(m)
	}
	return nil
}
