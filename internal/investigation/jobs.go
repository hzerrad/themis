// Package investigation runs the investigation of an incident.
package investigation

// InvestigateArgs are the arguments of the job that investigates a new
// incident. It is queued in the same transaction that creates the incident.
type InvestigateArgs struct {
	IncidentID string `json:"incidentID"`
}

// Kind is the job's type name. It is persisted with every job, so it must
// never change once released: queued jobs with the old name would never run.
func (InvestigateArgs) Kind() string { return "investigate" }
