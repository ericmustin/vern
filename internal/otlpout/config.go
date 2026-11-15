// Package otlpout sends an Instrumentation Score report to any OTel backend as
// OTLP/HTTP protobuf gauge metrics.
package otlpout

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Config says where and how to export. Build it with ConfigFromEnv, then apply
// flag overrides with WithEndpoint and WithHeaders.
type Config struct {
	MetricsURL         string // final URL to POST to
	Headers            map[string]string
	ResourceAttributes map[string]string
	ServiceVersion     string
	HTTP               *http.Client // nil: 10s timeout

	// Warnings collects non-fatal config problems. They name positions, never
	// values, because header values carry credentials.
	Warnings []string
}

// Enabled reports whether an endpoint is configured.
func (c Config) Enabled() bool { return c.MetricsURL != "" }

// ConfigFromEnv reads the standard OTel exporter variables.
func ConfigFromEnv(getenv func(string) string) Config {
	c := Config{Headers: map[string]string{}, ResourceAttributes: map[string]string{}}
	if u := strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT")); u != "" {
		c.MetricsURL = u
	} else if b := strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_ENDPOINT")); b != "" {
		c.MetricsURL = BaseToMetricsURL(b)
	}
	c.addHeaders(getenv("OTEL_EXPORTER_OTLP_HEADERS"), "OTEL_EXPORTER_OTLP_HEADERS")
	c.addHeaders(getenv("OTEL_EXPORTER_OTLP_METRICS_HEADERS"), "OTEL_EXPORTER_OTLP_METRICS_HEADERS")
	for i, kv := range splitList(getenv("OTEL_RESOURCE_ATTRIBUTES")) {
		k, v, ok := parsePair(kv)
		if !ok {
			c.Warnings = append(c.Warnings, "OTEL_RESOURCE_ATTRIBUTES entry "+strconv.Itoa(i+1)+" is not key=value; skipped")
			continue
		}
		c.ResourceAttributes[k] = v
	}
	return c
}

// BaseToMetricsURL appends /v1/metrics to a base endpoint with exactly one
// slash; a value already ending in /v1/metrics is returned as is.
func BaseToMetricsURL(base string) string {
	base = strings.TrimSpace(base)
	if u, err := url.Parse(base); err == nil && u.Scheme != "" && u.Host != "" {
		p := strings.TrimRight(u.Path, "/")
		if !strings.HasSuffix(p, "/v1/metrics") {
			p += "/v1/metrics"
		}
		u.Path, u.RawPath = p, ""
		return u.String()
	}
	trimmed := strings.TrimRight(base, "/")
	if strings.HasSuffix(trimmed, "/v1/metrics") {
		return trimmed
	}
	return trimmed + "/v1/metrics"
}

// WithEndpoint overrides the endpoint with a base URL (flag semantics).
func (c Config) WithEndpoint(base string) Config {
	if strings.TrimSpace(base) != "" {
		c.MetricsURL = BaseToMetricsURL(base)
	}
	return c
}

// WithHeaders merges "k=v" entries over the existing headers; they win.
func (c Config) WithHeaders(entries []string) Config {
	m := make(map[string]string, len(c.Headers))
	for k, v := range c.Headers {
		m[k] = v
	}
	c.Headers = m
	for i, e := range entries {
		k, v, ok := parsePair(e)
		if !ok {
			c.Warnings = append(append([]string(nil), c.Warnings...), "--otlp-header #"+strconv.Itoa(i+1)+" is not key=value; skipped")
			continue
		}
		c.Headers[strings.ToLower(k)] = v
	}
	return c
}

func (c *Config) addHeaders(s, name string) {
	for i, kv := range splitList(s) {
		k, v, ok := parsePair(kv)
		if !ok {
			c.Warnings = append(c.Warnings, name+" entry "+strconv.Itoa(i+1)+" is not key=value; skipped")
			continue
		}
		c.Headers[strings.ToLower(k)] = v
	}
}

func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// parsePair splits k=v, trims the key and URL-decodes the value.
func parsePair(kv string) (k, v string, ok bool) {
	k, v, ok = strings.Cut(kv, "=")
	k = strings.TrimSpace(k)
	if !ok || k == "" {
		return "", "", false
	}
	v = strings.TrimSpace(v)
	if d, err := url.PathUnescape(v); err == nil {
		v = d
	}
	return k, v, true
}
