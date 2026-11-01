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

func TestRunResourcesDefaultsAndOverride(t *testing.T) {
	base := "name: p\nsites: [\"https://x.test\"]\n"
	p, err := Load([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	if r := p.RunResources(); r.CPU != "1" || r.Memory != "3Gi" {
		t.Errorf("default = %+v", r)
	}
	p, err = Load([]byte(base + "resources: {cpu: 500m, memory: 4Gi}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if r := p.RunResources(); r.CPU != "500m" || r.Memory != "4Gi" {
		t.Errorf("override = %+v", r)
	}
	p, err = LoadJSON([]byte(`{"name":"p","sites":["https://x.test"],"resources":{"memory":"2Gi"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if r := p.RunResources(); r.CPU != "1" || r.Memory != "2Gi" {
		t.Errorf("partial override = %+v", r)
	}
}

func TestResourcesRejected(t *testing.T) {
	for _, res := range []string{
		`{"cpu":"100m"}`, `{"cpu":"2"}`, `{"cpu":"5"}`, `{"cpu":"lots"}`,
		`{"memory":"512Mi"}`, `{"memory":"16Gi"}`, `{"memory":"big"}`,
	} {
		j := `{"name":"p","sites":["https://x.test"],"resources":` + res + `}`
		if _, err := LoadJSON([]byte(j)); err == nil {
			t.Errorf("LoadJSON accepted resources %s", res)
		}
		y := "name: p\nsites: [\"https://x.test\"]\nresources: " + res + "\n"
		if _, err := Load([]byte(y)); err == nil {
			t.Errorf("Load accepted resources %s", res)
		}
	}
	for _, res := range []string{`{"cpu":"250m","memory":"1Gi"}`, `{"cpu":"1","memory":"8Gi"}`} {
		if _, err := LoadJSON([]byte(`{"name":"p","sites":["https://x.test"],"resources":` + res + `}`)); err != nil {
			t.Errorf("rejected boundary %s: %v", res, err)
		}
	}
}
