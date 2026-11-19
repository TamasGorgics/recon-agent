package state

// edgeActor is the actor-class rule for one side of an edge. The zero value
// means the class may not drive the edge.
type edgeActor struct {
	allowed bool
	reason  reasonRule
}

func allow(reason reasonRule) edgeActor {
	return edgeActor{allowed: true, reason: reason}
}

// edge is one legal case status transition. Reason requirements can differ by
// actor class; resolution is required exactly on target-resolved edges.
type edge struct {
	from               Status
	to                 Status
	human              edgeActor
	system             edgeActor
	resolutionRequired bool
	purpose            string
}

// edges is the complete case transition table from spec 4.2. It is ordered by
// source status in spec order and then by target status; tests in this package
// walk the full 6x6 matrix against it.
var edges = []edge{
	{StatusOpen, StatusInAgent, edgeActor{}, allow(reasonOptional), false,
		"worker lease acquisition"},
	{StatusOpen, StatusEscalated, edgeActor{}, allow(reasonRequiredFreeText), false,
		"attempts exhausted, missing-settlement SLA, or manual escalation"},
	{StatusOpen, StatusResolved, allow(reasonRequiredFreeText), allow(reasonOptional), true,
		"human manual resolve or system late match"},

	{StatusInAgent, StatusOpen, allow(reasonRequiredFreeText), allow(reasonOptional), false,
		"worker release or human unmatch"},
	{StatusInAgent, StatusProposalPending, edgeActor{}, allow(reasonOptional), false,
		"agent finished with a proposal"},
	{StatusInAgent, StatusWaitingInfo, edgeActor{}, allow(reasonOptional), false,
		"agent needs more information"},
	{StatusInAgent, StatusEscalated, edgeActor{}, allow(reasonRequiredFreeText), false,
		"agent failure or manual escalation"},
	{StatusInAgent, StatusResolved, allow(reasonRequiredFreeText), allow(reasonOptional), true,
		"human manual resolve or system late match"},

	{StatusProposalPending, StatusOpen, allow(reasonRequiredFreeText), allow(reasonOptional), false,
		"proposal retry or human unmatch"},
	{StatusProposalPending, StatusWaitingInfo, edgeActor{}, allow(reasonOptional), false,
		"proposal needs more information"},
	{StatusProposalPending, StatusEscalated, edgeActor{}, allow(reasonRequiredFreeText), false,
		"proposal retry cap or missing-settlement SLA"},
	{StatusProposalPending, StatusResolved, allow(reasonRequiredFreeText), allow(reasonOptional), true,
		"human manual resolve or system late match"},

	{StatusWaitingInfo, StatusOpen, allow(reasonOptional), allow(reasonOptional), false,
		"human reply, ingest resume, or human unmatch"},
	{StatusWaitingInfo, StatusEscalated, edgeActor{}, allow(reasonRequiredFreeText), false,
		"missing-settlement SLA or manual escalation"},
	{StatusWaitingInfo, StatusResolved, allow(reasonRequiredFreeText), allow(reasonOptional), true,
		"human manual resolve or system late match"},

	{StatusEscalated, StatusOpen, allow(reasonRequiredFreeText), allow(reasonRequiredPartialLateMatch), false,
		"human reopen/unmatch or system partial late match"},
	{StatusEscalated, StatusResolved, allow(reasonRequiredFreeText), allow(reasonOptional), true,
		"human manual resolve or system late match"},

	{StatusResolved, StatusOpen, allow(reasonRequiredFreeText), edgeActor{}, false,
		"human reopen or unmatch"},
}

// lookupEdge returns the legal edge from -> to, if any. Status pairs are
// unique, so the first match is the only match.
func lookupEdge(from, to Status) (edge, bool) {
	for _, candidate := range edges {
		if candidate.from == from && candidate.to == to {
			return candidate, true
		}
	}
	return edge{}, false
}

func (e edge) actorFor(class actorClass) edgeActor {
	switch class {
	case actorClassHuman:
		return e.human
	case actorClassSystem:
		return e.system
	default:
		return edgeActor{}
	}
}
