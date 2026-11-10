package remediation

import (
	"fmt"
	"strings"
	"time"

	"github.com/streetsmartops/incident-management-chatbot/core"
)

// Approval roles.
const (
	RoleApprover     = "approver"
	RoleIC           = "ic"
	RoleServiceOwner = "service_owner"
)

// PolicyError explains why an approval was refused.
type PolicyError struct{ Msg string }

func (e *PolicyError) Error() string { return e.Msg }

func perr(format string, a ...any) error { return &PolicyError{Msg: fmt.Sprintf(format, a...)} }

func same(a, b core.UserRef) bool {
	if a.Email != "" && b.Email != "" {
		return strings.EqualFold(a.Email, b.Email)
	}
	return a.Platform == b.Platform && a.ID == b.ID && a.ID != ""
}

// Describe returns the human-readable approval rule for a risk tier.
func Describe(r core.Risk) string {
	switch r {
	case core.RiskLow:
		return "1 approver (the requester may self-approve only if they are the IC)"
	case core.RiskMedium:
		return "the IC, who must not be the requester"
	case core.RiskHigh:
		return "the IC and the service owner: two different people, neither the requester"
	}
	return "unknown risk"
}

// CanApprove decides whether approver may approve req now, and in which
// role. owner is the service owner's email ("" if unknown).
func CanApprove(req *core.ActionRequest, inc *core.Incident, approver core.UserRef, owner string, now time.Time) (string, error) {
	if req.Status != core.ActionPending {
		return "", perr("request is %s, not pending", req.Status)
	}
	if !now.Before(req.ExpiresAt) {
		return "", perr("request expired at %s; request it again", req.ExpiresAt.UTC().Format(time.RFC3339))
	}
	if approver.Email == "" {
		return "", perr("your chat identity is not mapped to an email; you cannot approve")
	}
	for _, a := range req.Approvals {
		if same(a.By, approver) {
			return "", perr("you already approved this request")
		}
	}
	ic, hasIC := inc.IC()
	isIC := hasIC && same(ic, approver)
	isOwner := owner != "" && strings.EqualFold(owner, approver.Email)
	isRequester := same(req.RequestedBy, approver)

	switch req.Risk {
	case core.RiskLow:
		if isRequester && !isIC {
			return "", perr("low risk: the requester may self-approve only as IC")
		}
		if isIC {
			return RoleIC, nil
		}
		return RoleApprover, nil
	case core.RiskMedium:
		if !isIC {
			return "", perr("medium risk: only the IC can approve")
		}
		if isRequester {
			return "", perr("medium risk: the IC cannot approve their own request")
		}
		return RoleIC, nil
	case core.RiskHigh:
		if isRequester {
			return "", perr("high risk: the requester cannot approve")
		}
		if owner == "" {
			return "", perr("high risk: no service owner is configured for this service; cannot satisfy the two-person rule")
		}
		haveIC, haveOwner := roles(req.Approvals)
		switch {
		case isIC && isOwner:
			// One person holding both roles fills only one slot.
			if !haveIC {
				return RoleIC, nil
			}
			if !haveOwner {
				return RoleServiceOwner, nil
			}
		case isIC && !haveIC:
			return RoleIC, nil
		case isOwner && !haveOwner:
			return RoleServiceOwner, nil
		}
		return "", perr("high risk: needs the IC and the service owner (%s)", owner)
	}
	return "", perr("unknown risk %q", req.Risk)
}

func roles(apps []core.Approval) (ic, owner bool) {
	for _, a := range apps {
		switch a.Role {
		case RoleIC:
			ic = true
		case RoleServiceOwner:
			owner = true
		}
	}
	return
}

// Satisfied reports whether approvals meet the risk tier's rule.
func Satisfied(risk core.Risk, requester core.UserRef, apps []core.Approval) bool {
	var valid []core.Approval
	for _, a := range apps {
		if a.By.Email != "" {
			valid = append(valid, a)
		}
	}
	switch risk {
	case core.RiskLow:
		for _, a := range valid {
			if !same(a.By, requester) || a.Role == RoleIC {
				return true
			}
		}
	case core.RiskMedium:
		for _, a := range valid {
			if a.Role == RoleIC && !same(a.By, requester) {
				return true
			}
		}
	case core.RiskHigh:
		var icBy, ownerBy *core.UserRef
		for i := range valid {
			a := valid[i]
			if same(a.By, requester) {
				continue
			}
			switch a.Role {
			case RoleIC:
				icBy = &valid[i].By
			case RoleServiceOwner:
				ownerBy = &valid[i].By
			}
		}
		return icBy != nil && ownerBy != nil && !same(*icBy, *ownerBy)
	}
	return false
}
