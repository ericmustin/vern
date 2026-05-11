package semconv

// MET001AttrAllowlist returns the curated set of semconv attribute keys that
// MET-001 evaluates for unbounded cardinality. The spec requires evaluating
// every attribute key on every metric, but doing so dynamically from ES|QL
// is infeasible without dimension-discovery joins. This allowlist captures
// the high-risk keys that empirically explode cardinality in OTel pipelines —
// free-form URLs, raw queries, message bodies, error stacks, etc.
//
// Keys here MUST exist in AttributeKeys (the codegen guarantees this since
// they're standard semconv keys). Vern queries against `attributes.<key>`
// on metric documents.
func MET001AttrAllowlist() []string {
	return []string{
		// HTTP — high cardinality if routes aren't templated, query strings
		// aren't stripped, or user agents are passed through.
		"http.route",
		"url.path",
		"url.query",
		"url.full",
		"user_agent.original",

		// DB — raw statements blow cardinality; named statements / collection
		// names are bounded but worth watching.
		"db.query.text",
		"db.statement",
		"db.collection.name",

		// Messaging — destination names can be templated (e.g. per-tenant queues).
		"messaging.destination.name",
		"messaging.destination.template",
		"messaging.message.id",

		// RPC — method name is bounded, but service/peer can drift.
		"rpc.method",

		// Generic identifiers that frequently leak into metric attributes.
		"client.address",
		"server.address",
		"network.peer.address",
		"error.type",
		"exception.type",
	}
}
