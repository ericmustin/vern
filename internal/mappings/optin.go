package mappings

import "github.com/ericmustin/vern/internal/config"

// ApplyOptInGates sets Enabled on every rule that names an OptInFlag: true
// when the active config sets that flag, false otherwise (so a rule enabled
// by mistake in the YAML stays off). Rules without an OptInFlag are left
// alone. Known flags: "filters.enable_sdk_rules" (SDK-001); any other name
// counts as off. Call it before resolving so evaluate, generate and setup
// agree on which rules run.
func ApplyOptInGates(rules []RuleMapping, cfg *config.Config) {
	for i := range rules {
		if rules[i].OptInFlag == "" {
			continue
		}
		rules[i].Enabled = optInFlagSet(rules[i].OptInFlag, cfg)
	}
}

func optInFlagSet(name string, cfg *config.Config) bool {
	switch name {
	case "filters.enable_sdk_rules":
		return cfg.Filters.EnableSDKRules
	default:
		return false
	}
}

// NeedsFieldDiscovery is why the sensitive-data rules do not run outside
// vern score and vern serve: their queries scan the fields discovered for
// one run, which a scheduled workflow or vern evaluate does not have.
const NeedsFieldDiscovery = "needs per-run field discovery: run vern score or vern serve"

// ApplyWorkflowGates applies ApplyOptInGates and then disables every rule
// marked Sensitive, with NeedsFieldDiscovery as its Status, so generate,
// setup and agent neither emit a no-op step for it nor report it enabled.
// Call it before coverage.Build and resolving.
func ApplyWorkflowGates(rules []RuleMapping, cfg *config.Config) {
	ApplyOptInGates(rules, cfg)
	for i := range rules {
		if rules[i].Sensitive {
			rules[i].Enabled = false
			rules[i].Status = NeedsFieldDiscovery
		}
	}
}
