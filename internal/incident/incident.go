package incident

type TargetRef struct {
	Group     string
	Kind      string
	Namespace string
	Name      string
	UID       string
}
