package identity

import "context"

// ClosedMessage is what Themis answers, on every surface, while no admin
// account exists.
const ClosedMessage = "No admin account. Create the first admin account to start Themis."

// GateStore says whether an admin account exists.
type GateStore interface {
	HasAdminAccount(ctx context.Context) (bool, error)
}

// Gate keeps Themis closed while no account has the admin role: it then
// serves nothing but the making of the first, and takes no alerts. An
// identity provider's admins don't open it. It is asked each time, so it
// closes again if the admin accounts are ever gone, as from a database
// restored without them.
type Gate struct {
	Store GateStore
}

// Open reports whether an admin account exists. A nil Gate is always open.
func (g *Gate) Open(ctx context.Context) (bool, error) {
	if g == nil {
		return true, nil
	}
	return g.Store.HasAdminAccount(ctx)
}
