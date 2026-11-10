// Package remediation implements the version-controlled action catalog, the
// approval policy and the request → dry-run → approve → execute → verify
// flow. There is no free-form command path: only catalog IDs can run.
package remediation

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/streetsmartops/incident-management-chatbot/core"
)

// ParamSpec is a constrained JSON-Schema-like parameter definition.
type ParamSpec struct {
	Type        string   `yaml:"type"` // string | integer
	Required    bool     `yaml:"required"`
	Pattern     string   `yaml:"pattern"`
	Enum        []string `yaml:"enum"`
	Min         *int     `yaml:"min"`
	Max         *int     `yaml:"max"`
	Description string   `yaml:"description"`
	re          *regexp.Regexp
}

// Scopes restricts where an action may run.
type Scopes struct {
	Clusters   []string `yaml:"clusters"`
	Namespaces []string `yaml:"namespaces"`
}

// VerifySpec describes the post-execution check.
type VerifySpec struct {
	Type    string `yaml:"type"`
	Timeout string `yaml:"timeout"`
}

// Action is one catalog entry.
type Action struct {
	ID            string               `yaml:"id"`
	Description   string               `yaml:"description"`
	Provider      string               `yaml:"provider"`
	Verb          string               `yaml:"verb"`
	Risk          core.Risk            `yaml:"risk"`
	Params        map[string]ParamSpec `yaml:"params"`
	AllowedScopes Scopes               `yaml:"allowedScopes"`
	Verify        VerifySpec           `yaml:"verify"`
}

// Catalog is the loaded set of actions.
type Catalog struct {
	actions map[string]*Action
}

// LoadCatalog loads every *.yaml file in dir.
func LoadCatalog(dir string) (*Catalog, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	var acts []*Action
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var a Action
		dec := yaml.NewDecoder(strings.NewReader(string(b)))
		dec.KnownFields(true)
		if err := dec.Decode(&a); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		acts = append(acts, &a)
	}
	return NewCatalog(acts)
}

// NewCatalog validates actions.
func NewCatalog(acts []*Action) (*Catalog, error) {
	c := &Catalog{actions: map[string]*Action{}}
	for _, a := range acts {
		if a.ID == "" || a.Provider == "" || a.Verb == "" {
			return nil, fmt.Errorf("catalog action needs id, provider, verb: %+v", a)
		}
		if _, dup := c.actions[a.ID]; dup {
			return nil, fmt.Errorf("duplicate catalog id %s", a.ID)
		}
		switch a.Risk {
		case core.RiskLow, core.RiskMedium, core.RiskHigh:
		default:
			return nil, fmt.Errorf("%s: risk must be low|medium|high", a.ID)
		}
		for name, p := range a.Params {
			if p.Type != "string" && p.Type != "integer" {
				return nil, fmt.Errorf("%s.%s: type must be string or integer", a.ID, name)
			}
			if p.Pattern != "" {
				re, err := regexp.Compile("^(?:" + p.Pattern + ")$")
				if err != nil {
					return nil, fmt.Errorf("%s.%s: bad pattern: %w", a.ID, name, err)
				}
				p.re = re
			}
			a.Params[name] = p
		}
		if a.Provider == "kubernetes" {
			for _, req := range []string{"cluster", "namespace", "deployment"} {
				if p, ok := a.Params[req]; !ok || !p.Required {
					return nil, fmt.Errorf("%s: kubernetes actions must declare required param %q", a.ID, req)
				}
			}
			if len(a.AllowedScopes.Clusters) == 0 || len(a.AllowedScopes.Namespaces) == 0 {
				return nil, fmt.Errorf("%s: allowedScopes.clusters and .namespaces are required", a.ID)
			}
		}
		c.actions[a.ID] = a
	}
	return c, nil
}

// Get returns an action by ID.
func (c *Catalog) Get(id string) (*Action, bool) {
	a, ok := c.actions[id]
	return a, ok
}

// IDs returns sorted catalog IDs.
func (c *Catalog) IDs() []string {
	out := make([]string, 0, len(c.actions))
	for id := range c.actions {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Validate checks params against the spec and scopes; unknown params are rejected.
func (a *Action) Validate(params map[string]string) error {
	var errs []string
	for k := range params {
		if _, ok := a.Params[k]; !ok {
			errs = append(errs, fmt.Sprintf("unknown parameter %q", k))
		}
	}
	names := make([]string, 0, len(a.Params))
	for n := range a.Params {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		spec := a.Params[name]
		v, ok := params[name]
		if !ok || v == "" {
			if spec.Required {
				errs = append(errs, fmt.Sprintf("missing required parameter %q", name))
			}
			continue
		}
		if spec.re != nil && !spec.re.MatchString(v) {
			errs = append(errs, fmt.Sprintf("%s=%q does not match %s", name, v, spec.Pattern))
		}
		if len(spec.Enum) > 0 && !contains(spec.Enum, v) {
			errs = append(errs, fmt.Sprintf("%s must be one of %v", name, spec.Enum))
		}
		if spec.Type == "integer" {
			n, err := strconv.Atoi(v)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s must be an integer", name))
			} else {
				if spec.Min != nil && n < *spec.Min {
					errs = append(errs, fmt.Sprintf("%s must be >= %d", name, *spec.Min))
				}
				if spec.Max != nil && n > *spec.Max {
					errs = append(errs, fmt.Sprintf("%s must be <= %d", name, *spec.Max))
				}
			}
		}
	}
	if c, ok := params["cluster"]; ok && len(a.AllowedScopes.Clusters) > 0 && !globAny(a.AllowedScopes.Clusters, c) {
		errs = append(errs, fmt.Sprintf("cluster %q is not allowed for %s", c, a.ID))
	}
	if ns, ok := params["namespace"]; ok && len(a.AllowedScopes.Namespaces) > 0 && !globAny(a.AllowedScopes.Namespaces, ns) {
		errs = append(errs, fmt.Sprintf("namespace %q is not allowed for %s", ns, a.ID))
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func globAny(patterns []string, v string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, v); ok {
			return true
		}
	}
	return false
}

// Risk returns an action's risk tier (satisfies triage.Catalog).
func (c *Catalog) Risk(id string) (core.Risk, bool) {
	a, ok := c.actions[id]
	if !ok {
		return "", false
	}
	return a.Risk, true
}

// ValidateParams validates params for an action ID (satisfies triage.Catalog).
func (c *Catalog) ValidateParams(id string, params map[string]string) error {
	a, ok := c.actions[id]
	if !ok {
		return fmt.Errorf("unknown action %q", id)
	}
	return a.Validate(params)
}
