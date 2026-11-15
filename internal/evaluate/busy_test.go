package evaluate

import "testing"

func TestBusy(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
		want   bool
	}{
		{429, "", true},
		{503, `{"error":{"type":"circuit_breaking_exception"}}`, true},
		{500, `{"error":{"type":"x","root_cause":[{"type":"es_rejected_execution_exception"}]}}`, true},
		{500, `{"error":{"type":"parse_exception","reason":"circuit_breaking_exception in a name"}}`, false},
		{500, `not json circuit_breaking_exception`, false},
		{200, `{"error":{"type":"circuit_breaking_exception"}}`, false},
		{200, `{"values":[["circuit_breaking_exception"]]}`, false},
	} {
		if got := Busy(c.status, []byte(c.body)); got != c.want {
			t.Errorf("Busy(%d, %s) = %v, want %v", c.status, c.body, got, c.want)
		}
	}
}
