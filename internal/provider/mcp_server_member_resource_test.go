package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

// The API key the fake treats as the caller. Creating a server records it as
// the server's `owner`, exactly as the real API does for the creator (D3).
const (
	fakeSelfKeyID  = "key-self"
	fakeServerPath = "/v1/orgs/org-1/mcp-servers"
)

var fakeTime = time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

type fakeMember struct {
	ActorType string
	ActorID   string
	Role      string
}

type fakeMcpServer struct {
	ID       string
	Name     string
	Endpoint string
	Access   string
	Members  []fakeMember
}

// mcpMemberAPI is a stateful fake of the MCP server + member-list API that
// mirrors the real semantics on monorepo main:
//
//   - POST creates a restricted server and records the caller as its owner.
//   - PUT /members is an idempotent add-or-update of one row; returns the list.
//   - DELETE /members/{type}/{id} is 204, or 404 when the row is absent.
//   - Removing or demoting the last owner is 409.
//   - PUT /access sets open|restricted and returns the detail.
//
// Switches simulate the three 403s (list gate, may_confer, access gate). The
// counters let tests assert which writes a plan actually issued.
type mcpMemberAPI struct {
	mu      sync.Mutex
	t       *testing.T
	servers map[string]*fakeMcpServer
	nextID  int

	forbidMemberWrites bool // "Only an owner of this MCP server can change its members"
	forbidConferOwner  bool // may_confer refuses role=owner
	forbidAccess       bool // "Only an owner ... can change its access mode"

	patches    int
	accessPuts int
	memberPuts int
}

func newMcpMemberAPI(t *testing.T) (*mcpMemberAPI, *httptest.Server) {
	t.Helper()
	api := &mcpMemberAPI{t: t, servers: map[string]*fakeMcpServer{}}
	srv := httptest.NewServer(http.HandlerFunc(api.serve))
	t.Cleanup(srv.Close)
	return api, srv
}

func (a *mcpMemberAPI) serve(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer byk_test" {
		a.problem(w, http.StatusUnauthorized, "missing bearer")
		return
	}
	if !strings.HasPrefix(r.URL.Path, fakeServerPath) {
		a.t.Errorf("unexpected path %s %s", r.Method, r.URL.Path)
		a.problem(w, http.StatusNotFound, "no route")
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, fakeServerPath), "/")
	if rest == "" {
		if r.Method != http.MethodPost {
			a.problem(w, http.StatusMethodNotAllowed, "")
			return
		}
		a.create(w, r)
		return
	}
	parts := strings.Split(rest, "/")
	s, ok := a.servers[parts[0]]
	if !ok {
		a.problem(w, http.StatusNotFound, "MCP server not found")
		return
	}
	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		writeFakeJSON(w, http.StatusOK, a.detail(s))
	case len(parts) == 1 && r.Method == http.MethodPatch:
		a.patches++
		var body fakePatchBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Name != nil {
			s.Name = *body.Name
		}
		writeFakeJSON(w, http.StatusOK, a.detail(s))
	case len(parts) == 1 && r.Method == http.MethodDelete:
		delete(a.servers, s.ID)
		w.WriteHeader(http.StatusNoContent)
	case len(parts) == 2 && parts[1] == "access" && r.Method == http.MethodPut:
		if a.forbidAccess {
			a.problem(w, http.StatusForbidden, "Only an owner of this MCP server can change its access mode")
			return
		}
		a.accessPuts++
		var body client.McpServerAccessUpdate
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.Access = string(body.Access)
		writeFakeJSON(w, http.StatusOK, a.detail(s))
	case len(parts) == 2 && parts[1] == "members" && r.Method == http.MethodGet:
		writeFakeJSON(w, http.StatusOK, a.memberList(s))
	case len(parts) == 2 && parts[1] == "members" && r.Method == http.MethodPut:
		a.putMember(w, r, s)
	case len(parts) == 4 && parts[1] == "members" && r.Method == http.MethodDelete:
		a.deleteMember(w, s, parts[2], parts[3])
	default:
		a.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		a.problem(w, http.StatusMethodNotAllowed, "")
	}
}

func (a *mcpMemberAPI) create(w http.ResponseWriter, r *http.Request) {
	var body fakeCreateBody
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Access != nil {
		a.t.Errorf("create body must not carry access (the API has no such field): %s", body.Access)
	}
	a.nextID++
	s := &fakeMcpServer{
		ID:       fmt.Sprintf("srv-%d", a.nextID),
		Name:     body.Name,
		Endpoint: body.EndpointURL,
		Access:   string(client.McpServerAccessRestricted),
		Members:  []fakeMember{{ActorType: "api_key", ActorID: fakeSelfKeyID, Role: mcpMemberRoleOwner}},
	}
	a.servers[s.ID] = s
	writeFakeJSON(w, http.StatusCreated, a.detail(s))
}

func (a *mcpMemberAPI) putMember(w http.ResponseWriter, r *http.Request, s *fakeMcpServer) {
	var body client.McpServerMemberAdd
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		a.problem(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	role := mcpMemberRoleMember
	if body.Role != nil {
		role = *body.Role
	}
	if a.forbidMemberWrites {
		a.problem(w, http.StatusForbidden, "Only an owner of this MCP server can change its members")
		return
	}
	if a.forbidConferOwner && role == mcpMemberRoleOwner {
		a.problem(w, http.StatusForbidden, "You cannot give somebody the 'owner' role on this MCP server, "+
			"because it carries permissions you do not hold on it yourself.")
		return
	}
	a.memberPuts++
	for i := range s.Members {
		m := &s.Members[i]
		if m.ActorType == string(body.ActorType) && m.ActorID == body.ActorId {
			if m.Role == mcpMemberRoleOwner && role != mcpMemberRoleOwner && a.owners(s) == 1 {
				a.problem(w, http.StatusConflict, "The last owner cannot be demoted")
				return
			}
			m.Role = role
			writeFakeJSON(w, http.StatusOK, a.memberList(s))
			return
		}
	}
	s.Members = append(s.Members, fakeMember{ActorType: string(body.ActorType), ActorID: body.ActorId, Role: role})
	writeFakeJSON(w, http.StatusOK, a.memberList(s))
}

func (a *mcpMemberAPI) deleteMember(w http.ResponseWriter, s *fakeMcpServer, actorType, actorID string) {
	if a.forbidMemberWrites {
		a.problem(w, http.StatusForbidden, "Only an owner of this MCP server can change its members")
		return
	}
	for i, m := range s.Members {
		if m.ActorType == actorType && m.ActorID == actorID {
			if m.Role == mcpMemberRoleOwner && a.owners(s) == 1 {
				a.problem(w, http.StatusConflict, "The last owner cannot be removed")
				return
			}
			s.Members = append(s.Members[:i], s.Members[i+1:]...)
			w.Header().Set("Content-Type", "application/json") // the empty-204 shape the wrapper tolerates
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	a.problem(w, http.StatusNotFound, "Member not found")
}

func (a *mcpMemberAPI) owners(s *fakeMcpServer) int {
	n := 0
	for _, m := range s.Members {
		if m.Role == mcpMemberRoleOwner {
			n++
		}
	}
	return n
}

func (a *mcpMemberAPI) memberList(s *fakeMcpServer) []client.McpServerMember {
	out := make([]client.McpServerMember, 0, len(s.Members))
	for _, m := range s.Members {
		out = append(out, client.McpServerMember{
			ActorType: client.ActorType(m.ActorType), ActorId: m.ActorID, Role: m.Role, CreatedAt: fakeTime,
		})
	}
	return out
}

func (a *mcpMemberAPI) detail(s *fakeMcpServer) client.ManagedRemoteMcpServerDetail {
	access := client.McpServerAccess(s.Access)
	return client.ManagedRemoteMcpServerDetail{
		McpServerId: s.ID, OrgId: "org-1", Slug: strings.ToLower(strings.ReplaceAll(s.Name, " ", "-")),
		Name: s.Name, Transport: client.McpServerTransportStreamableHttp,
		ObservedState: client.McpServerState("external"), DesiredState: client.McpServerDesiredState("running"),
		CreatedAt: fakeTime, UpdatedAt: fakeTime, ConfigGeneration: 1, ReconciledGeneration: 1,
		RuntimeKind: client.ManagedRemoteMcpServerDetailRuntimeKindManagedRemote, EndpointUrl: s.Endpoint,
		Access: &access,
	}
}

// The request bodies the fake reads. Only the fields it acts on are decoded;
// Access stays raw so "present at all" is detectable.
type fakeCreateBody struct {
	Name        string          `json:"name"`
	EndpointURL string          `json:"endpoint_url"`
	Access      json.RawMessage `json:"access"`
}

type fakePatchBody struct {
	Name *string `json:"name"`
}

type fakeProblem struct {
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
}

// fakeResponse is the closed set of bodies the fake returns.
type fakeResponse interface {
	client.ManagedRemoteMcpServerDetail | []client.McpServerMember
}

func writeFakeJSON[T fakeResponse](w http.ResponseWriter, status int, v T) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (a *mcpMemberAPI) problem(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(fakeProblem{Title: http.StatusText(status), Status: status, Detail: detail})
}

// --- test helpers operating on fake state (all take the lock) ---

func (a *mcpMemberAPI) only() *fakeMcpServer {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.servers) != 1 {
		a.t.Fatalf("want exactly one server in the fake, have %d", len(a.servers))
	}
	for _, s := range a.servers {
		return s
	}
	return nil
}

func (a *mcpMemberAPI) seed(actorType, actorID, role string) {
	s := a.only()
	a.mu.Lock()
	defer a.mu.Unlock()
	s.Members = append(s.Members, fakeMember{ActorType: actorType, ActorID: actorID, Role: role})
}

func (a *mcpMemberAPI) drop(actorType, actorID string) {
	s := a.only()
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, m := range s.Members {
		if m.ActorType == actorType && m.ActorID == actorID {
			s.Members = append(s.Members[:i], s.Members[i+1:]...)
			return
		}
	}
	a.t.Fatalf("drop: no member %s/%s", actorType, actorID)
}

func (a *mcpMemberAPI) roleOf(actorType, actorID string) string {
	s := a.only()
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, m := range s.Members {
		if m.ActorType == actorType && m.ActorID == actorID {
			return m.Role
		}
	}
	return ""
}

func (a *mcpMemberAPI) counts() (patches, accessPuts, memberPuts int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.patches, a.accessPuts, a.memberPuts
}

func (a *mcpMemberAPI) set(f func(a *mcpMemberAPI)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f(a)
}

// memberTestConfig renders a provider block pointed at the fake, one managed
// remote server (with an optional access line) and the given extra HCL.
func memberTestConfig(endpoint, accessLine, extra string) string {
	return fmt.Sprintf(`
provider "botyard" {
  endpoint = %q
  api_key  = "byk_test"
  org_id   = "org-1"
}

resource "botyard_mcp_server" "s" {
  runtime_kind = "managed_remote"
  name         = "Vendor"
  endpoint_url = "https://mcp.vendor.example.com"
  %s
}
%s
`, endpoint, accessLine, extra)
}

const botMemberHCL = `
resource "botyard_mcp_server_member" "bot" {
  mcp_server_id = botyard_mcp_server.s.id
  actor_type    = "bot"
  actor_id      = "bot-1"
  %s
}
`

func botMember(roleLine string) string { return fmt.Sprintf(botMemberHCL, roleLine) }

func checkFake(f func() error) resource.TestCheckFunc {
	return func(*terraform.State) error { return f() }
}

// TestMcpServerMember_Lifecycle drives the real Terraform core against the
// fake: create, the creator-row / derived-floor no-diff plan, role update,
// access round-trip, import, out-of-band removal, and destroy.
func TestMcpServerMember_Lifecycle(t *testing.T) {
	api, srv := newMcpMemberAPI(t)
	base := func(accessLine, roleLine string) string {
		return memberTestConfig(srv.URL, accessLine, botMember(roleLine))
	}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			// 1. create: role defaults to member; access unset adopts the server's.
			{
				Config: base("", ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("botyard_mcp_server_member.bot", "role", "member"),
					resource.TestCheckResourceAttr("botyard_mcp_server_member.bot", "id", "srv-1/bot/bot-1"),
					resource.TestCheckResourceAttr("botyard_mcp_server_member.bot", "created_at", "2026-09-20T12:00:00Z"),
					resource.TestCheckResourceAttr("botyard_mcp_server.s", "access", "restricted"),
					checkFake(func() error {
						if got := api.roleOf("bot", "bot-1"); got != "member" {
							return fmt.Errorf("fake role = %q, want member", got)
						}
						if got := api.roleOf("api_key", fakeSelfKeyID); got != "owner" {
							return fmt.Errorf("creator row role = %q, want owner (untouched)", got)
						}
						if _, access, _ := api.counts(); access != 0 {
							return fmt.Errorf("access PUTs = %d, want 0 when access is unset", access)
						}
						return nil
					}),
				),
			},
			// 2. creator owner row + a derived bot floor row + a hand-added
			//    user are all invisible to a config that declares one member.
			{
				PreConfig: func() {
					api.seed("bot", "bot-derived", mcpMemberRoleMember)
					api.seed("user", "user-app", mcpMemberRoleOwner)
				},
				Config:   base("", ""),
				PlanOnly: true,
			},
			// 3. role update in place.
			{
				Config: base("", `role = "owner"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("botyard_mcp_server_member.bot", plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("botyard_mcp_server_member.bot", "role", "owner"),
					checkFake(func() error {
						if got := api.roleOf("bot", "bot-1"); got != "owner" {
							return fmt.Errorf("fake role = %q, want owner", got)
						}
						return nil
					}),
				),
			},
			// 4. access: open goes through PUT /access only — no PATCH.
			{
				Config: base(`access = "open"`, `role = "owner"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("botyard_mcp_server.s", "access", "open"),
					checkFake(func() error {
						patches, access, _ := api.counts()
						if access != 1 || patches != 0 {
							return fmt.Errorf("access PUTs/PATCHes = %d/%d, want 1/0", access, patches)
						}
						return nil
					}),
				),
			},
			// 5. dropping `access` from config adopts the server's value: no diff.
			{
				Config:   base("", `role = "owner"`),
				PlanOnly: true,
			},
			// 6. import by {server}/{actor_type}/{actor_id}.
			{
				ResourceName:      "botyard_mcp_server_member.bot",
				ImportState:       true,
				ImportStateId:     "srv-1/bot/bot-1",
				ImportStateVerify: true,
			},
			// 7. removed outside Terraform: refresh drops it, plan re-creates.
			{
				PreConfig:          func() { api.drop("bot", "bot-1") },
				Config:             base("", `role = "owner"`),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				Config: base("", `role = "owner"`),
				Check: checkFake(func() error {
					if got := api.roleOf("bot", "bot-1"); got != "owner" {
						return fmt.Errorf("re-created role = %q, want owner", got)
					}
					return nil
				}),
			},
		},
	})
}

// A principal that is already on the list (e.g. a bot that got the derived
// floor from a tool assignment) is adopted on create, not an error.
func TestMcpServerMember_AdoptsExistingRow(t *testing.T) {
	api, srv := newMcpMemberAPI(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{Config: memberTestConfig(srv.URL, "", "")},
			{
				PreConfig: func() { api.seed("bot", "bot-1", mcpMemberRoleMember) },
				Config:    memberTestConfig(srv.URL, "", botMember(`role = "owner"`)),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("botyard_mcp_server_member.bot", "role", "owner"),
					checkFake(func() error {
						if _, _, puts := api.counts(); puts != 1 {
							return fmt.Errorf("member PUTs = %d, want 1 (adopt is one add-or-update)", puts)
						}
						s := api.only()
						api.mu.Lock()
						defer api.mu.Unlock()
						if len(s.Members) != 2 {
							return fmt.Errorf("members = %v, want creator + one adopted bot row", s.Members)
						}
						return nil
					}),
				),
			},
		},
	})
}

// Opening a server needs owner-level authority; a 403 surfaces on `access`
// with the remedy, and the created server stays in state (tainted).
func TestMcpServer_AccessForbidden(t *testing.T) {
	api, srv := newMcpMemberAPI(t)
	api.set(func(a *mcpMemberAPI) { a.forbidAccess = true })
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      memberTestConfig(srv.URL, `access = "open"`, ""),
				ExpectError: regexp.MustCompile(`only an\s+owner of the server`),
			},
		},
	})
}

// A 403 from may_confer on granting `owner` names the role problem.
func TestMcpServerMember_ConferForbidden(t *testing.T) {
	api, srv := newMcpMemberAPI(t)
	api.set(func(a *mcpMemberAPI) { a.forbidConferOwner = true })
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      memberTestConfig(srv.URL, "", botMember(`role = "owner"`)),
				ExpectError: regexp.MustCompile(`Not permitted to grant this MCP server role`),
			},
		},
	})
}

// --- direct method tests for the refusal paths -----------------------------

func memberResourceFor(t *testing.T, srvURL string) *McpServerMemberResource {
	t.Helper()
	return &McpServerMemberResource{data: &providerData{client: newBotClient(t, srvURL, "byk_test"), orgID: "org-1"}}
}

func memberSchema(t *testing.T) fwresource.SchemaResponse {
	t.Helper()
	var sch fwresource.SchemaResponse
	(&McpServerMemberResource{}).Schema(context.Background(), fwresource.SchemaRequest{}, &sch)
	return sch
}

func memberState(t *testing.T, m McpServerMemberResourceModel) tfsdk.State {
	t.Helper()
	sch := memberSchema(t)
	st := tfsdk.State{Schema: sch.Schema, Raw: tftypes.NewValue(sch.Schema.Type().TerraformType(context.Background()), nil)}
	if d := st.Set(context.Background(), &m); d.HasError() {
		t.Fatalf("state set: %v", d)
	}
	return st
}

func memberModel(serverID, actorType, actorID, role string) McpServerMemberResourceModel {
	return McpServerMemberResourceModel{
		ID:          types.StringValue(mcpServerMemberID(serverID, actorType, actorID)),
		McpServerID: types.StringValue(serverID),
		ActorType:   types.StringValue(actorType),
		ActorID:     types.StringValue(actorID),
		Role:        types.StringValue(role),
		CreatedAt:   types.StringValue("2026-09-20T12:00:00Z"),
	}
}

// fakeWithServer creates one server in the fake through its own API.
func fakeWithServer(t *testing.T) (*mcpMemberAPI, *httptest.Server) {
	t.Helper()
	api, srv := newMcpMemberAPI(t)
	api.set(func(a *mcpMemberAPI) {
		a.nextID++
		a.servers["srv-1"] = &fakeMcpServer{
			ID: "srv-1", Name: "Vendor", Endpoint: "https://x", Access: "restricted",
			Members: []fakeMember{{ActorType: "api_key", ActorID: fakeSelfKeyID, Role: mcpMemberRoleOwner}},
		}
	})
	return api, srv
}

func TestMcpServerMember_Delete(t *testing.T) {
	ctx := context.Background()

	t.Run("404 is success", func(t *testing.T) {
		_, srv := fakeWithServer(t)
		r := memberResourceFor(t, srv.URL)
		resp := &fwresource.DeleteResponse{}
		r.Delete(ctx, fwresource.DeleteRequest{State: memberState(t, memberModel("srv-1", "bot", "gone", "member"))}, resp)
		if resp.Diagnostics.HasError() {
			t.Fatalf("404 on delete should be treated as already gone: %v", resp.Diagnostics)
		}
	})

	t.Run("server gone is success", func(t *testing.T) {
		_, srv := fakeWithServer(t)
		r := memberResourceFor(t, srv.URL)
		resp := &fwresource.DeleteResponse{}
		r.Delete(ctx, fwresource.DeleteRequest{State: memberState(t, memberModel("srv-404", "bot", "b", "member"))}, resp)
		if resp.Diagnostics.HasError() {
			t.Fatalf("delete on a vanished server should succeed: %v", resp.Diagnostics)
		}
	})

	t.Run("409 last owner names the remedy", func(t *testing.T) {
		_, srv := fakeWithServer(t)
		r := memberResourceFor(t, srv.URL)
		resp := &fwresource.DeleteResponse{}
		r.Delete(ctx, fwresource.DeleteRequest{State: memberState(t,
			memberModel("srv-1", "api_key", fakeSelfKeyID, mcpMemberRoleOwner))}, resp)
		assertDiag(t, resp.Diagnostics.Errors(), "Cannot change the last owner", "cannot be removed", "terraform state rm", "The last owner cannot be removed")
	})

	t.Run("403 list gate", func(t *testing.T) {
		api, srv := fakeWithServer(t)
		api.seed("bot", "b", mcpMemberRoleMember)
		api.set(func(a *mcpMemberAPI) { a.forbidMemberWrites = true })
		r := memberResourceFor(t, srv.URL)
		resp := &fwresource.DeleteResponse{}
		r.Delete(ctx, fwresource.DeleteRequest{State: memberState(t, memberModel("srv-1", "bot", "b", "member"))}, resp)
		assertDiag(t, resp.Diagnostics.Errors(), "Not permitted to change this MCP server's members")
	})
}

func TestMcpServerMember_UpdateRefusals(t *testing.T) {
	ctx := context.Background()
	sch := memberSchema(t)
	plan := func(m McpServerMemberResourceModel) tfsdk.Plan {
		st := memberState(t, m)
		return tfsdk.Plan{Schema: sch.Schema, Raw: st.Raw}
	}
	newState := func() tfsdk.State {
		return tfsdk.State{Schema: sch.Schema, Raw: tftypes.NewValue(sch.Schema.Type().TerraformType(ctx), nil)}
	}

	t.Run("409 on demoting the last owner", func(t *testing.T) {
		_, srv := fakeWithServer(t)
		r := memberResourceFor(t, srv.URL)
		resp := &fwresource.UpdateResponse{State: newState()}
		r.Update(ctx, fwresource.UpdateRequest{Plan: plan(memberModel("srv-1", "api_key", fakeSelfKeyID, mcpMemberRoleMember))}, resp)
		assertDiag(t, resp.Diagnostics.Errors(), "Cannot change the last owner", "cannot be demoted")
	})

	t.Run("403 list gate on member role", func(t *testing.T) {
		api, srv := fakeWithServer(t)
		api.set(func(a *mcpMemberAPI) { a.forbidMemberWrites = true })
		r := memberResourceFor(t, srv.URL)
		resp := &fwresource.UpdateResponse{State: newState()}
		r.Update(ctx, fwresource.UpdateRequest{Plan: plan(memberModel("srv-1", "bot", "b", mcpMemberRoleMember))}, resp)
		assertDiag(t, resp.Diagnostics.Errors(), "Not permitted to change this MCP server's members")
	})

	t.Run("404 server not visible", func(t *testing.T) {
		_, srv := fakeWithServer(t)
		r := memberResourceFor(t, srv.URL)
		resp := &fwresource.UpdateResponse{State: newState()}
		r.Update(ctx, fwresource.UpdateRequest{Plan: plan(memberModel("srv-x", "bot", "b", mcpMemberRoleMember))}, resp)
		assertDiag(t, resp.Diagnostics.Errors(), "MCP server not found", "restricted")
	})
}

func TestMcpServerMember_ReadServerGoneRemoves(t *testing.T) {
	ctx := context.Background()
	_, srv := fakeWithServer(t)
	r := memberResourceFor(t, srv.URL)
	st := memberState(t, memberModel("srv-404", "bot", "b", "member"))
	resp := &fwresource.ReadResponse{State: st}
	r.Read(ctx, fwresource.ReadRequest{State: st}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("diags: %v", resp.Diagnostics)
	}
	if !resp.State.Raw.IsNull() {
		t.Error("a 404 on the member list should remove the membership from state")
	}
}

// assertDiag requires exactly one error diagnostic whose summary+detail
// contains every wanted fragment.
func assertDiag(t *testing.T, errs diag.Diagnostics, want ...string) {
	t.Helper()
	if len(errs) != 1 {
		t.Fatalf("want exactly one error, got %d: %v", len(errs), errs)
	}
	text := errs[0].Summary() + "\n" + errs[0].Detail()
	for _, w := range want {
		if !strings.Contains(text, w) {
			t.Errorf("diagnostic missing %q:\n%s", w, text)
		}
	}
}

func TestParseMcpServerMemberID(t *testing.T) {
	ok := []struct{ in, s, typ, id string }{
		{"srv/bot/b1", "srv", "bot", "b1"},
		{"srv/user/u1", "srv", "user", "u1"},
		{"srv/api_key/k1", "srv", "api_key", "k1"},
	}
	for _, c := range ok {
		s, typ, id, err := parseMcpServerMemberID(c.in)
		if err != nil || s != c.s || typ != c.typ || id != c.id {
			t.Errorf("parse(%q) = %q,%q,%q,%v", c.in, s, typ, id, err)
		}
	}
	for _, bad := range []string{"", "srv", "srv/bot", "srv/bot/", "/bot/b", "srv/team/b", "srv/bot/b/extra", "srv/mcp_workload/w"} {
		if _, _, _, err := parseMcpServerMemberID(bad); err == nil {
			t.Errorf("parse(%q) should fail", bad)
		}
	}
}

func TestAccessHelpers(t *testing.T) {
	open := client.McpServerAccessOpen
	if got := accessFromDetail(&open, types.StringValue("restricted")); got.ValueString() != "open" {
		t.Errorf("detail value wins: got %v", got)
	}
	if got := accessFromDetail(nil, types.StringUnknown()); !got.IsNull() {
		t.Errorf("unknown prior + absent → null, got %v", got)
	}
	if got := accessFromDetail(nil, types.StringValue("open")); got.ValueString() != "open" {
		t.Errorf("absent keeps prior, got %v", got)
	}
	if accessNeedsPut(types.StringNull(), types.StringValue("restricted")) ||
		accessNeedsPut(types.StringUnknown(), types.StringValue("restricted")) ||
		accessNeedsPut(types.StringValue("open"), types.StringValue("open")) {
		t.Error("accessNeedsPut: unset/unknown/equal must not PUT")
	}
	if !accessNeedsPut(types.StringValue("open"), types.StringValue("restricted")) {
		t.Error("accessNeedsPut: differing value must PUT")
	}
}

func TestOnlyAccessChanged(t *testing.T) {
	a := managedModel()
	a.Access = types.StringValue("restricted")
	b := a
	b.Access = types.StringValue("open")
	if !onlyAccessChanged(b, a) {
		t.Error("access-only diff should skip PATCH")
	}
	b.Name = types.StringValue("renamed")
	if onlyAccessChanged(b, a) {
		t.Error("a name change must still PATCH")
	}
}
