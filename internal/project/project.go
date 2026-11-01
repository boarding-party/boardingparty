// Package project defines the boardingparty project config.
package project

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"

	"k8s.io/apimachinery/pkg/api/resource"

	"sigs.k8s.io/yaml"
)

type Project struct {
	Name       string   `json:"name"`
	Sites      []string `json:"sites"` // staging URLs; hosts become the allow-list
	ExtraHosts []string `json:"extraHosts"`
	Model      string   `json:"model"` // optional override
	Roles      []Role   `json:"roles"`
	// Resources sizes each run pod's agent container. Optional; see
	// Project.RunResources for the defaults and bounds.
	Resources *Resources `json:"resources,omitempty"`
}

// Resources is the run pod's CPU and memory, as Kubernetes quantities.
type Resources struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
}

// Run pod resource defaults and bounds. A run pod is never unbounded.
const (
	DefaultCPU    = "1"
	DefaultMemory = "3Gi"
	minCPU        = "250m"
	maxCPU        = "4"
	minMemory     = "1Gi"
	maxMemory     = "8Gi"
)

// RunResources returns the CPU and memory for a run pod: the project's values,
// with the defaults filling anything unset.
func (p Project) RunResources() Resources {
	r := Resources{CPU: DefaultCPU, Memory: DefaultMemory}
	if p.Resources != nil {
		if p.Resources.CPU != "" {
			r.CPU = p.Resources.CPU
		}
		if p.Resources.Memory != "" {
			r.Memory = p.Resources.Memory
		}
	}
	return r
}

func checkQuantity(field, v, lo, hi string) error {
	q, err := resource.ParseQuantity(v)
	if err != nil {
		return fmt.Errorf("resources.%s %q is not a valid quantity: %w", field, v, err)
	}
	if q.Cmp(resource.MustParse(lo)) < 0 || q.Cmp(resource.MustParse(hi)) > 0 {
		return fmt.Errorf("resources.%s %q must be between %s and %s", field, v, lo, hi)
	}
	return nil
}

type Role struct {
	Name               string    `json:"name"`
	Description        string    `json:"description"`
	CanWatchRecordings bool      `json:"canWatchRecordings"`
	Personas           []Persona `json:"personas"` // explicit in Plan 1; generated in Plan 2
	Stories            []Story   `json:"stories"`
}

type Persona struct {
	Name     string            `json:"name"`
	Traits   []string          `json:"traits"`   // e.g. "impatient", "phone"
	Identity map[string]string `json:"identity"` // synthetic: email, password, address...
}

type Story struct {
	Name string `json:"name"`
	Goal string `json:"goal"`
}

// Load parses and validates a project from YAML.
func Load(b []byte) (Project, error) {
	var p Project
	if err := yaml.UnmarshalStrict(b, &p); err != nil {
		return Project{}, fmt.Errorf("parse project: %w", err)
	}
	if err := p.validate(); err != nil {
		return Project{}, err
	}
	return p, nil
}

// LoadJSON parses and validates a project from JSON, with the same rules as Load.
func LoadJSON(b []byte) (Project, error) {
	var p Project
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Project{}, fmt.Errorf("parse project: %w", err)
	}
	if dec.More() {
		return Project{}, fmt.Errorf("parse project: trailing data")
	}
	if err := p.validate(); err != nil {
		return Project{}, err
	}
	return p, nil
}

func (p Project) validate() error {
	if len(p.Sites) == 0 {
		return fmt.Errorf("project needs at least one site")
	}
	for _, s := range p.Sites {
		u, err := url.Parse(s)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("site %q must be an https:// URL", s)
		}
	}
	if r := p.Resources; r != nil {
		if r.CPU != "" {
			if err := checkQuantity("cpu", r.CPU, minCPU, maxCPU); err != nil {
				return err
			}
		}
		if r.Memory != "" {
			if err := checkQuantity("memory", r.Memory, minMemory, maxMemory); err != nil {
				return err
			}
		}
	}
	seen := map[string]bool{}
	personas := map[string]bool{} // runs and persona state are keyed by persona name
	for _, r := range p.Roles {
		if r.Name == "" {
			return fmt.Errorf("role needs a name")
		}
		if seen[r.Name] {
			return fmt.Errorf("duplicate role %q", r.Name)
		}
		seen[r.Name] = true
		for _, pe := range r.Personas {
			if pe.Name == "" {
				return fmt.Errorf("role %q: persona needs a name", r.Name)
			}
			if personas[pe.Name] {
				return fmt.Errorf("duplicate persona %q", pe.Name)
			}
			personas[pe.Name] = true
		}
		for _, st := range r.Stories {
			if st.Goal == "" {
				return fmt.Errorf("role %q: story %q needs a goal", r.Name, st.Name)
			}
		}
	}
	return nil
}
