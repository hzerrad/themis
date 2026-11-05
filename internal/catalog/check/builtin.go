package check

import _ "embed"

// GitOpsManaged is the check that says whether Argo CD or Flux manages the
// workload. Themis runs it for every incident with a fix on offer: when it
// isn't false, Themis only recommends a fix, whatever policy says.
const GitOpsManaged = "gitops_managed"

//go:embed builtin.yaml
var builtin []byte

// Builtin is Themis's own checks, which every scenario pack's profile may
// name as it names the pack's.
func Builtin() ([]Definition, error) {
	return Parse(builtin)
}
