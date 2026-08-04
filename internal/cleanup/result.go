package cleanup

// Outcome describes whether one cleanup iteration can advance.
type Outcome string

const (
	// OutcomeComplete means that the work required by the current iteration has
	// been verified as complete.
	OutcomeComplete Outcome = "complete"
	// OutcomeWaiting means that cleanup must observe OpenStack state again in a
	// later reconciliation.
	OutcomeWaiting Outcome = "waiting"
	// OutcomeNotApplicable means that policy or observed state makes the current
	// work unnecessary.
	OutcomeNotApplicable Outcome = "not-applicable"
)

// Result is the controller-independent result of one cleanup iteration.
type Result struct {
	Outcome Outcome
}
