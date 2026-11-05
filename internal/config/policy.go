package config

// Policy modes.
const (
	// ModeGuarded runs actions only after a human approves them.
	ModeGuarded = "guarded"
	// ModeObserve never runs actions: proposals become recommendations.
	ModeObserve = "observe"
)

// Policy decisions.
const (
	DecisionAllow           = "allow"
	DecisionDeny            = "deny"
	DecisionRequireApproval = "require_approval"
)

// Policy is evaluated on every derived action. The first matching rule
// decides; with none, the action needs approval.
type Policy struct {
	Mode            string       `json:"mode,omitempty" jsonschema:"enum=guarded,enum=observe"`
	Rules           []PolicyRule `json:"rules,omitempty"`
	ApprovalTimeout Duration     `json:"approvalTimeout,omitzero"`
}

// PolicyRule decides when its CEL expression holds. The expression sees
// action.capability, action.risk, action.restore, target.namespace,
// target.managed_by, predicates (name to "true", "false" or "unknown"),
// change (what the fix says about the change, by key), proposal.source, proposal.authenticated, proposal.external and mode.
type PolicyRule struct {
	Name     string `json:"name" jsonschema:"minLength=1"`
	When     string `json:"when" jsonschema:"minLength=1"`
	Decision string `json:"decision" jsonschema:"enum=allow,enum=deny,enum=require_approval"`
}

// EffectivePolicy returns p's policy with defaults filled in.
func (p Profile) EffectivePolicy() Policy {
	pol := p.Policy
	if pol.Mode == "" {
		pol.Mode = ModeGuarded
	}
	if pol.ApprovalTimeout.Duration == 0 {
		pol.ApprovalTimeout.Duration = DefaultApprovalTimeout
	}
	return pol
}
