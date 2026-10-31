package project

import (
	"os"
	"strings"
	"testing"
)

func TestLoadRejects(t *testing.T) {
	tests := []struct{ name, yaml string }{
		{"empty sites", "name: p\nsites: []\nroles: []\n"},
		{"http site", "name: p\nsites: [\"http://x.test\"]\n"},
		{"duplicate role", `name: p
sites: ["https://x.test"]
roles:
  - {name: a}
  - {name: a}
`},
		{"persona without name", `name: p
sites: ["https://x.test"]
roles:
  - name: a
    personas: [{traits: [x]}]
`},
		{"duplicate persona across roles", `name: p
sites: ["https://x.test"]
roles:
  - {name: a, personas: [{name: ann}]}
  - {name: b, personas: [{name: ann}]}
`},
		{"story without goal", `name: p
sites: ["https://x.test"]
roles:
  - name: a
    stories: [{name: s, goal: ""}]
`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Load([]byte(tc.yaml)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestLoadExample(t *testing.T) {
	b, err := os.ReadFile("../../examples/todomvc.yaml")
	if err != nil {
		t.Fatal(err)
	}
	p, err := Load(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Roles) != 1 || p.Roles[0].Name != "visitor" {
		t.Fatalf("roles: %+v", p.Roles)
	}
	r := p.Roles[0]
	if len(r.Personas) != 1 || len(r.Stories) != 1 {
		t.Fatalf("role: %+v", r)
	}
	if !strings.Contains(r.Stories[0].Goal, "2 items left") {
		t.Fatalf("goal: %q", r.Stories[0].Goal)
	}
}
