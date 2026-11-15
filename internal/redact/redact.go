// Package redact keeps credentials and query strings out of URLs and
// transport errors before they reach logs, errors or reports.
package redact

import "net/url"

// URL returns raw without userinfo, query or fragment. An unparseable URL is
// not echoed at all.
func URL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(invalid URL)"
	}
	u.User = nil
	u.RawQuery, u.ForceQuery = "", false
	u.Fragment, u.RawFragment = "", ""
	return u.String()
}

// TransportError takes an error straight from http.Client.Do, a *url.Error
// whose URL net/http only password-masks, and replaces that URL with URL(it).
// The result is still a *url.Error wrapping the same cause, so errors.Is and
// errors.As keep working. Other errors are returned unchanged.
func TransportError(err error) error {
	ue, ok := err.(*url.Error)
	if !ok {
		return err
	}
	return &url.Error{Op: ue.Op, URL: URL(ue.URL), Err: ue.Err}
}
