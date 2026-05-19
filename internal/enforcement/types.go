package enforcement

import "time"

type TripwirePolicy int

const (
	TripwireLogAndContinue TripwirePolicy = iota + 1
	TripwireHumanReview
	TripwireImmediateTeardown
)

func (p TripwirePolicy) String() string {
	switch p {
	case TripwireLogAndContinue:
		return "log_and_continue"
	case TripwireHumanReview:
		return "human_review"
	case TripwireImmediateTeardown:
		return "immediate_teardown"
	default:
		return "unknown"
	}
}

type PayloadDecision int

const (
	PayloadAllow PayloadDecision = iota + 1
	PayloadBlock
	PayloadHold
)

func (d PayloadDecision) String() string {
	switch d {
	case PayloadAllow:
		return "allow"
	case PayloadBlock:
		return "block"
	case PayloadHold:
		return "hold"
	default:
		return "unknown"
	}
}

type FalcoAlert struct {
	RuleName  string
	Priority  string
	Output    string
	CageID    string
	Timestamp time.Time
}

type CiliumPolicy struct {
	CageID string
	Scope  []string
	Extras []string
}
