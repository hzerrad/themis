// Package governance turns accepted proposals into governed actions.
package governance

// GovernArgs govern an accepted proposal.
type GovernArgs struct {
	ProposalID string `json:"proposalID"`
}

// Kind is persisted with each job and must never change.
func (GovernArgs) Kind() string { return "govern" }
