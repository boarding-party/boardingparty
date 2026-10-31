// Package mcpserver exposes the per-run MCP tools that workers call.
package mcpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tiny-systems/boardingparty/internal/runs"
	"github.com/tiny-systems/boardingparty/internal/store"
)

const errNotActive = "unknown or finished run"

const pathPrefix = "/mcp/run/"

// Handler serves /mcp/run/{token}. The token in the path scopes every tool to one run.
// directorURL is the base URL workers use to reach the director's upload endpoint.
func Handler(svc *runs.Service, st *store.Store, directorURL string) http.Handler {
	directorURL = strings.TrimRight(directorURL, "/")
	return mcp.NewStreamableHTTPHandler(func(r *http.Request) *Server {
		return newServer(svc, st, directorURL, tokenFromPath(r.URL.Path))
	}, &mcp.StreamableHTTPOptions{Stateless: true})
}

// Server is an alias to keep the handler signature short.
type Server = mcp.Server

func tokenFromPath(p string) string {
	i := strings.Index(p, pathPrefix)
	if i < 0 {
		return ""
	}
	return strings.Trim(p[i+len(pathPrefix):], "/")
}

func toolErr(msg string) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}, nil, nil
}

func text(msg string) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: msg}}}, nil, nil
}

type noArgs struct{}

type persona struct {
	Name     string            `json:"name"`
	Role     string            `json:"role"`
	Traits   []string          `json:"traits"`
	Identity map[string]string `json:"identity"`
}

type story struct {
	Name string `json:"name"`
	Goal string `json:"goal"`
}

type assignment struct {
	Persona      persona         `json:"persona"`
	Story        story           `json:"story"`
	Sites        []string        `json:"sites"`
	PersonaState json.RawMessage `json:"personaState"`
	UploadURL    string          `json:"uploadURL"`
}

type saveArgs struct {
	State map[string]any `json:"state" jsonschema:"the persona's memory to keep for future runs"`
}

type bugArgs struct {
	Summary       string `json:"summary" jsonschema:"one-line description of the bug"`
	Step          string `json:"step" jsonschema:"the step where it happened"`
	Evidence      string `json:"evidence" jsonschema:"artifact name or other evidence"`
	SeverityGuess string `json:"severity_guess" jsonschema:"high, medium or low"`
	Note          string `json:"note" jsonschema:"anything else worth knowing"`
}

type finishArgs struct {
	Outcome string `json:"outcome" jsonschema:"done, stuck or failed"`
	Summary string `json:"summary" jsonschema:"what happened"`
}

func newServer(svc *runs.Service, st *store.Store, directorURL, token string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "boardingparty", Version: "0.1.0"}, nil)

	// active resolves the run, mapping an inactive token to the tool-visible error.
	active := func() (store.Run, *mcp.CallToolResult, error) {
		r, err := svc.Active(token)
		if errors.Is(err, runs.ErrNotActive) {
			res, _, _ := toolErr(errNotActive)
			return store.Run{}, res, nil
		}
		return r, nil, err
	}

	mcp.AddTool(s, &mcp.Tool{Name: "next_assignment",
		Description: "Get your persona, story, sites to test, saved persona state and the artifact upload URL."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, any, error) {
			r, bad, err := active()
			if bad != nil || err != nil {
				return bad, nil, err
			}
			p, err := st.Project()
			if err != nil {
				return nil, nil, fmt.Errorf("load project: %w", err)
			}
			a := assignment{Sites: p.Sites, UploadURL: directorURL + "/artifacts/" + token + "/"}
			a.Persona.Name, a.Persona.Role = r.Persona, r.Role
			a.Story.Name = r.Story
			for _, role := range p.Roles {
				if role.Name != r.Role {
					continue
				}
				for _, pe := range role.Personas {
					if pe.Name == r.Persona {
						a.Persona.Traits, a.Persona.Identity = pe.Traits, pe.Identity
					}
				}
				for _, sy := range role.Stories {
					if sy.Name == r.Story {
						a.Story.Goal = sy.Goal
					}
				}
			}
			state, err := st.PersonaState(r.Persona)
			if err != nil {
				return nil, nil, err
			}
			if len(state) == 0 {
				state = json.RawMessage("null")
			}
			a.PersonaState = state
			b, err := json.Marshal(a)
			if err != nil {
				return nil, nil, err
			}
			return text(string(b))
		})

	mcp.AddTool(s, &mcp.Tool{Name: "persona_save",
		Description: "Save the persona's state so later runs remember it."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in saveArgs) (*mcp.CallToolResult, any, error) {
			r, bad, err := active()
			if bad != nil || err != nil {
				return bad, nil, err
			}
			b, err := json.Marshal(in.State)
			if err != nil {
				return toolErr("state is not valid JSON: " + err.Error())
			}
			if err := st.SavePersonaState(r.Persona, b); err != nil {
				return nil, nil, err
			}
			return text("saved")
		})

	mcp.AddTool(s, &mcp.Tool{Name: "report_bug",
		Description: "Report a bug you found. Returns the bug id."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in bugArgs) (*mcp.CallToolResult, any, error) {
			r, bad, err := active()
			if bad != nil || err != nil {
				return bad, nil, err
			}
			switch in.SeverityGuess {
			case "high", "medium", "low":
			default:
				return toolErr(fmt.Sprintf("severity_guess must be high, medium or low, got %q", in.SeverityGuess))
			}
			if strings.TrimSpace(in.Summary) == "" {
				return toolErr("summary is required")
			}
			raw := make([]byte, 8)
			if _, err := rand.Read(raw); err != nil {
				return nil, nil, err
			}
			b := store.Bug{ID: hex.EncodeToString(raw), RunID: r.ID, Summary: in.Summary, Step: in.Step,
				Evidence: in.Evidence, Severity: in.SeverityGuess, Note: in.Note, Created: time.Now().UTC()}
			if err := st.AddBug(b); err != nil {
				return nil, nil, err
			}
			return text(b.ID)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "finish_run",
		Description: "Finish your run. outcome is done, stuck or failed. Call this last."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in finishArgs) (*mcp.CallToolResult, any, error) {
			if _, bad, err := active(); bad != nil || err != nil {
				return bad, nil, err
			}
			var state store.RunState
			switch in.Outcome {
			case "done":
				state = store.RunDone
			case "stuck":
				state = store.RunStuck
			case "failed":
				state = store.RunFailed
			default:
				return toolErr(fmt.Sprintf("outcome must be done, stuck or failed, got %q", in.Outcome))
			}
			err := svc.Finish(ctx, token, state, in.Summary)
			if errors.Is(err, runs.ErrNotActive) {
				return toolErr(errNotActive)
			}
			if err != nil {
				return nil, nil, err
			}
			return text("finished")
		})
	return s
}
