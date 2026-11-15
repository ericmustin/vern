package evaluate

import (
	"encoding/json"
	"errors"
	"net/http"
)

// ErrBusy marks a backend that is shedding load: an HTTP 429 or a circuit
// breaker trip. A run that meets it is abandoned, never scored partially.
var ErrBusy = errors.New("backend busy")

var busyTypes = map[string]bool{
	"circuit_breaking_exception":      true,
	"es_rejected_execution_exception": true,
}

// Busy reports whether an HTTP response means the backend is shedding load:
// status 429, or a non-2xx response whose parsed error names a breaker or a
// rejected execution. Successful responses are never inspected, since they
// carry user data.
func Busy(status int, body []byte) bool {
	if status == http.StatusTooManyRequests {
		return true
	}
	if status/100 == 2 {
		return false
	}
	return hasBusyType(body, "error")
}

// BusyPartial reports whether a successful but partial response failed
// because the backend was shedding load, from its parsed failure types.
func BusyPartial(body []byte) bool { return hasBusyType(body, "failures") }

// hasBusyType parses body and looks for a busy "type" inside any value
// stored under key, at any depth.
func hasBusyType(body []byte, key string) bool {
	var v any
	if json.Unmarshal(body, &v) != nil {
		return false
	}
	return walkBusy(v, false, key)
}

func walkBusy(v any, inside bool, key string) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			if inside && k == "type" {
				if s, ok := child.(string); ok && busyTypes[s] {
					return true
				}
			}
			if walkBusy(child, inside || k == key, key) {
				return true
			}
		}
	case []any:
		for _, child := range x {
			if walkBusy(child, inside, key) {
				return true
			}
		}
	}
	return false
}
