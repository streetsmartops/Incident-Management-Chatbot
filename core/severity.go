package core

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// SeverityPolicy maps SLO status × tenant tier to a severity and decides
// whether a signal may auto-declare. The default reproduces
// docs/architecture.md §5.2; a company policy file can replace it.
type SeverityPolicy struct {
	// Matrix[sloStatus][tier] = severity number (1..4).
	Matrix map[SloStatus]map[TenantTier]int `yaml:"matrix"`
	// MultiTenantEscalation raises severity by one level when at least this
	// many tenants are affected on the same service (0 disables).
	MultiTenantEscalation int               `yaml:"multiTenantEscalation"`
	AutoDeclare           AutoDeclarePolicy `yaml:"autoDeclare"`
	// Hash is the SHA-256 of the loaded file, logged for audit.
	Hash string `yaml:"-"`
}

// AutoDeclarePolicy controls automatic declaration.
type AutoDeclarePolicy struct {
	Enabled bool `yaml:"enabled"`
	// MaxSeverity: signals at this severity or worse auto-declare (2 = SEV1/2).
	MaxSeverity int `yaml:"maxSeverity"`
	// ConsecutiveEvaluations the status must persist before auto-declare.
	ConsecutiveEvaluations int `yaml:"consecutiveEvaluations"`
	// RequireKnownTier refuses auto-declare when the tenant tier is unknown.
	RequireKnownTier bool `yaml:"requireKnownTier"`
	// NeverFor lists SLO statuses that never auto-declare.
	NeverFor []SloStatus `yaml:"neverFor"`
}

// DefaultSeverityPolicy is the policy agreed in Step 0 (Q6=b, Q11=a).
func DefaultSeverityPolicy() SeverityPolicy {
	return SeverityPolicy{
		Matrix: map[SloStatus]map[TenantTier]int{
			SloBreached: {TierEnterprise: 1, TierBusiness: 2, TierStarter: 3, TierUnknown: 2},
			SloAtRisk:   {TierEnterprise: 2, TierBusiness: 3, TierStarter: 4, TierUnknown: 3},
			SloTimeout:  {TierEnterprise: 3, TierBusiness: 4, TierStarter: 4, TierUnknown: 4},
			SloNoData:   {TierEnterprise: 3, TierBusiness: 4, TierStarter: 4, TierUnknown: 4},
		},
		MultiTenantEscalation: 3,
		AutoDeclare: AutoDeclarePolicy{
			Enabled:                true,
			MaxSeverity:            2,
			ConsecutiveEvaluations: 2,
			RequireKnownTier:       true,
			NeverFor:               []SloStatus{SloTimeout, SloNoData},
		},
		Hash: "builtin-default",
	}
}

// LoadSeverityPolicy reads a YAML policy file and validates it.
func LoadSeverityPolicy(path string) (SeverityPolicy, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return SeverityPolicy{}, fmt.Errorf("read severity policy: %w", err)
	}
	var p SeverityPolicy
	if err := yaml.Unmarshal(b, &p); err != nil {
		return SeverityPolicy{}, fmt.Errorf("parse severity policy %s: %w", path, err)
	}
	if err := p.Validate(); err != nil {
		return SeverityPolicy{}, fmt.Errorf("invalid severity policy %s: %w", path, err)
	}
	sum := sha256.Sum256(b)
	p.Hash = hex.EncodeToString(sum[:])
	return p, nil
}

// Validate checks the policy is complete and in range.
func (p SeverityPolicy) Validate() error {
	for _, st := range []SloStatus{SloBreached, SloAtRisk, SloTimeout, SloNoData} {
		row, ok := p.Matrix[st]
		if !ok {
			return fmt.Errorf("matrix is missing status %q", st)
		}
		for _, tier := range []TenantTier{TierEnterprise, TierBusiness, TierStarter, TierUnknown} {
			v, ok := row[tier]
			if !ok {
				return fmt.Errorf("matrix[%s] is missing tier %q", st, tier)
			}
			if v < 1 || v > 4 {
				return fmt.Errorf("matrix[%s][%s]=%d must be 1..4", st, tier, v)
			}
		}
	}
	if p.MultiTenantEscalation < 0 {
		return fmt.Errorf("multiTenantEscalation must be >= 0")
	}
	if p.AutoDeclare.Enabled {
		if p.AutoDeclare.MaxSeverity < 1 || p.AutoDeclare.MaxSeverity > 4 {
			return fmt.Errorf("autoDeclare.maxSeverity must be 1..4")
		}
		if p.AutoDeclare.ConsecutiveEvaluations < 1 {
			return fmt.Errorf("autoDeclare.consecutiveEvaluations must be >= 1")
		}
	}
	return nil
}

// Classify returns the severity for an SLO status at a tier, with
// multi-tenant escalation. Returns SevUnknown for status ok.
func (p SeverityPolicy) Classify(status SloStatus, tier TenantTier, affectedTenants int) Severity {
	row, ok := p.Matrix[status]
	if !ok {
		return SevUnknown
	}
	if tier == "" {
		tier = TierUnknown
	}
	v, ok := row[tier]
	if !ok {
		v = row[TierUnknown]
	}
	if p.MultiTenantEscalation > 0 && affectedTenants >= p.MultiTenantEscalation && v > 1 {
		v--
	}
	return Severity(v)
}

// DeclareDecision is the outcome of evaluating a signal.
type DeclareDecision struct {
	Severity    Severity
	AutoDeclare bool
	// Reason explains why auto-declare was or was not chosen (shown in chat).
	Reason string
}

// Decide evaluates whether a signal should auto-declare or only be proposed.
// consecutive is how many evaluations in a row the status has held.
func (p SeverityPolicy) Decide(sig Signal, affectedTenants, consecutive int) DeclareDecision {
	sev := sig.SeverityHint
	if sig.SloStatus != "" {
		sev = p.Classify(sig.SloStatus, sig.Tier, affectedTenants)
	}
	d := DeclareDecision{Severity: sev}
	ad := p.AutoDeclare
	switch {
	case !sev.Valid():
		d.Reason = "no severity could be computed"
	case !ad.Enabled:
		d.Reason = "auto-declare disabled by policy"
	case int(sev) > ad.MaxSeverity:
		d.Reason = fmt.Sprintf("%s is below the auto-declare threshold (SEV%d or worse)", sev, ad.MaxSeverity)
	case containsStatus(ad.NeverFor, sig.SloStatus):
		d.Reason = fmt.Sprintf("status %s never auto-declares", sig.SloStatus)
	case ad.RequireKnownTier && sig.SloStatus != "" && (sig.Tier == "" || sig.Tier == TierUnknown):
		d.Reason = "tenant tier unknown; proposing instead of auto-declaring"
	case consecutive < ad.ConsecutiveEvaluations:
		d.Reason = fmt.Sprintf("status seen %d/%d consecutive evaluations", consecutive, ad.ConsecutiveEvaluations)
	default:
		d.AutoDeclare = true
		d.Reason = fmt.Sprintf("%s auto-declared by policy %s", sev, short(p.Hash))
	}
	return d
}

func containsStatus(list []SloStatus, s SloStatus) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// short abbreviates SHA-256 policy hashes; named policies are kept whole.
func short(h string) string {
	if len(h) == 64 {
		return h[:12]
	}
	return h
}
