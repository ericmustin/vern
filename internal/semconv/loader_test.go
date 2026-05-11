package semconv

import (
	"testing"
)

func TestBuild_ParsesRegistryAndPlacement(t *testing.T) {
	// Compact fixture covering the three group shapes Vern relies on:
	//   - registry group (attribute_group) that defines IDs
	//   - entity group that references attrs at resource-level
	//   - span group that references attrs at span-level
	//   - templated attribute that must be EXCLUDED from AttributeKeys
	docs := map[string][]byte{
		"model/http/registry.yaml": []byte(`
groups:
  - id: registry.http
    type: attribute_group
    attributes:
      - id: http.request.method
        type: string
      - id: http.request.header
        type: template[string[]]
      - id: http.response.status_code
        type: int
`),
		"model/service/entities.yaml": []byte(`
groups:
  - id: entity.service
    type: entity
    attributes:
      - ref: service.name
      - ref: service.namespace
`),
		"model/service/registry.yaml": []byte(`
groups:
  - id: registry.service
    type: attribute_group
    attributes:
      - id: service.name
        type: string
      - id: service.namespace
        type: string
`),
		"model/http/spans.yaml": []byte(`
groups:
  - id: span.http.client
    type: span
    attributes:
      - ref: http.request.method
      - ref: http.response.status_code
`),
	}

	cat, err := Build("v1.37.0-test", docs)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	wantKeys := map[string]bool{
		"http.request.method":       true,
		"http.response.status_code": true,
		"service.name":              true,
		"service.namespace":         true,
	}
	for _, k := range cat.AttributeKeys {
		if !wantKeys[k] {
			t.Errorf("unexpected key %q in AttributeKeys", k)
		}
		delete(wantKeys, k)
	}
	for k := range wantKeys {
		t.Errorf("missing expected key %q", k)
	}

	// Templated attribute must not appear.
	for _, k := range cat.AttributeKeys {
		if k == "http.request.header" {
			t.Errorf("templated attribute http.request.header must be excluded")
		}
	}

	// http.request.method appears only in span groups → SpanOnly.
	spanOnly := map[string]bool{}
	for _, k := range cat.LevelKeys(LevelSpan) {
		spanOnly[k] = true
	}
	if !spanOnly["http.request.method"] {
		t.Errorf("http.request.method should be SpanOnly")
	}

	// service.name appears only in entity groups → ResourceOnly.
	resourceOnly := map[string]bool{}
	for _, k := range cat.LevelKeys(LevelResource) {
		resourceOnly[k] = true
	}
	if !resourceOnly["service.name"] {
		t.Errorf("service.name should be ResourceOnly")
	}
}

func TestQuotedCSV(t *testing.T) {
	got := QuotedCSV([]string{"b.key", "a.key"})
	want := `"a.key", "b.key"`
	if got != want {
		t.Errorf("QuotedCSV = %q, want %q", got, want)
	}
	if QuotedCSV(nil) != "" {
		t.Errorf("empty input should yield empty string")
	}
}
