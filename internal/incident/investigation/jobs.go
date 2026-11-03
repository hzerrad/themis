// Package investigation runs the investigation of an incident.
package investigation

import "github.com/riverqueue/river"

// Queue holds investigate jobs apart from the default queue.
const Queue = "investigation"

// InvestigateArgs investigate a new incident.
type InvestigateArgs struct {
	IncidentID string `json:"incidentID"`
}

// Kind is persisted with each job and must never change.
func (InvestigateArgs) Kind() string { return "investigate" }

func (a InvestigateArgs) Incident() string { return a.IncidentID }

// InsertOpts puts every investigate job on Queue.
func (InvestigateArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: Queue}
}
