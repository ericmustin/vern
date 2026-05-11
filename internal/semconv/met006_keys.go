package semconv

// MET006CuratedKeys returns the curated semconv attribute keys that
// MET-006 checks metric names against. The full registry has 800+ entries —
// passing all of them through ES|QL's `IN (...)` operator hits a parser limit
// that surfaces as a confusing `mismatched input ')'` error. This list is
// the subset most likely to collide with poorly-chosen metric names: common
// observability primitives (http.*, db.*, rpc.*, etc.) plus the spec's own
// resource identity attributes (service.*, host.*, k8s.*).
//
// If a downstream user finds a metric name colliding with a semconv key not
// in this list, add it here and regenerate.
func MET006CuratedKeys() []string {
	return []string{
		// Resource identity (these are the most-confused with metric names).
		"service.name", "service.namespace", "service.instance.id", "service.version",
		"host.name", "host.id", "host.type", "host.arch",
		"k8s.pod.name", "k8s.pod.uid", "k8s.namespace.name", "k8s.node.name",
		"k8s.container.name", "k8s.deployment.name",
		"container.id", "container.name", "container.image.name",
		"deployment.environment", "deployment.environment.name",
		"telemetry.sdk.name", "telemetry.sdk.language", "telemetry.sdk.version",
		"cloud.provider", "cloud.region", "cloud.account.id",
		"process.runtime.name", "process.runtime.version", "process.pid",

		// HTTP / URL — the most-overloaded namespace.
		"http.route", "http.method", "http.scheme", "http.status_code",
		"http.request.method", "http.response.status_code",
		"url.full", "url.path", "url.scheme", "url.query", "url.host",

		// Database / RPC / Messaging.
		"db.system", "db.name", "db.namespace", "db.operation", "db.statement",
		"db.query.text", "db.collection.name",
		"rpc.system", "rpc.service", "rpc.method",
		"messaging.system", "messaging.destination.name", "messaging.operation",

		// Network / client / server.
		"client.address", "server.address", "server.port",
		"network.protocol.name", "network.transport", "network.peer.address",

		// Errors / exceptions.
		"error.type", "exception.type", "exception.message",

		// Common attributes used as metric dimensions that have also been
		// (mis)used as metric names.
		"feature_flag.key", "user.id", "session.id", "trace.id", "span.id",
	}
}
