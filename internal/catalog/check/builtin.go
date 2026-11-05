package check

import _ "embed"

// GitOpsManaged is the check that says whether Argo CD or Flux manages the
// workload. Themis runs it for every incident with a fix on offer: when it
// isn't false, Themis only recommends a fix, whatever policy says.
const GitOpsManaged = "gitops_managed"

// EvidenceHasInstructions is the check that says whether any evidence holds
// text that looks written for the investigator, such as instructions or
// forged tool output. Governance decides it again over all the incident's
// evidence before deciding a fix, since an investigator asks for more after
// the checks first run.
const EvidenceHasInstructions = "evidence_has_instructions"

//go:embed builtin.yaml
var builtin []byte

// Builtin is Themis's own checks, which every scenario pack's profile may
// name as it names the pack's.
func Builtin() ([]Definition, error) {
	return Parse(builtin)
}
