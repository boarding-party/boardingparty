// Package project defines the boardingparty project config.
package project

import (
	"fmt"
	"net/url"

	"sigs.k8s.io/yaml"
)

type Project struct {
	Name       string   `json:"name"`
	Sites      []string `json:"sites"` // staging URLs; hosts become the allow-list
	ExtraHosts []string `json:"extraHosts"`
	Model      string   `json:"model"` // optional override
	Roles      []Role   `json:"roles"`
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
	seen := map[string]bool{}
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
		}
		for _, st := range r.Stories {
			if st.Goal == "" {
				return fmt.Errorf("role %q: story %q needs a goal", r.Name, st.Name)
			}
		}
	}
	return nil
}
