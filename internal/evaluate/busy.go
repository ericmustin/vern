package evaluate

import (
	"bytes"
	"errors"
	"net/http"
)

// ErrBusy marks a backend that is shedding load: an HTTP 429 or a circuit
// breaker trip. A run that meets it is abandoned, never scored partially.
var ErrBusy = errors.New("backend busy")

// Busy reports whether an HTTP response means the backend is shedding load.
func Busy(status int, body []byte) bool {
	return status == http.StatusTooManyRequests || bytes.Contains(body, []byte("circuit_breaking_exception"))
}
