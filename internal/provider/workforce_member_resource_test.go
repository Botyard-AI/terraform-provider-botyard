package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

const workforcePath = "/v1/orgs/org-1/workforces"

type workforceMemberAPI struct {
	mu                                                   sync.Mutex
	t                                                    *testing.T
	boards                                               []client.WorkforceListItem
	rows                                                 map[string][]client.WorkforceMember
	ack                                                  bool
	listStatus, memberStatus, writeStatus                int
	listNull, membersNull, malformedMembers              bool
	missingWrittenRow, wrongWrittenRole, missingWriteAck bool
	oldWriteReplica                                      bool
	renameAfterList, renameBeforeWrite                   bool
	lists, gets, puts, removes, mutations                int
}

func newWorkforceMemberAPI(t *testing.T) (*workforceMemberAPI, *httptest.Server) {
	t.Helper()
	a := &workforceMemberAPI{t: t, ack: true,
		boards: []client.WorkforceListItem{{Id: "wf-1", Slug: "platform-development"}},
		rows:   map[string][]client.WorkforceMember{"wf-1": {workforceRow("api_key", "creator", "owner")}},
	}
	srv := httptest.NewServer(http.HandlerFunc(a.serve))
	t.Cleanup(srv.Close)
	return a, srv
}

func workforceRow(kind, id, role string) client.WorkforceMember {
	return client.WorkforceMember{ActorType: client.ActorType(kind), ActorId: id, Role: role, CreatedAt: fakeTime}
}

func workforceJSON[T interface {
	[]client.WorkforceListItem | []client.WorkforceMember | fakeProblem
}](w http.ResponseWriter, status int, value T) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (a *workforceMemberAPI) problem(w http.ResponseWriter, status int) {
	workforceJSON(w, status, fakeProblem{Status: status, Title: http.StatusText(status), Detail: "workforce request refused"})
}

func (a *workforceMemberAPI) serve(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer byk_test" {
		a.problem(w, 401)
		return
	}
	if r.URL.Path == workforcePath && r.Method == http.MethodGet {
		a.lists++
		if a.listStatus != 0 {
			a.problem(w, a.listStatus)
			return
		}
		if a.listNull {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, "null")
			return
		}
		workforceJSON(w, 200, a.boards)
		if a.renameAfterList {
			a.boards[0].Slug = "renamed"
			a.renameAfterList = false
		}
		return
	}
	if !strings.HasPrefix(r.URL.Path, workforcePath+"/") {
		a.t.Errorf("unexpected path %s", r.URL.Path)
		a.problem(w, 404)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, workforcePath+"/"), "/")
	if len(parts) < 2 || parts[1] != "members" {
		a.t.Errorf("unexpected member path %s", r.URL.Path)
		a.problem(w, 404)
		return
	}
	boardID := ""
	for _, board := range a.boards {
		if board.Slug == parts[0] {
			boardID = board.Id
		}
	}
	if boardID == "" {
		a.problem(w, 404)
		return
	}
	if a.ack {
		w.Header().Set(client.WorkforceIdentityHeader, boardID)
	}
	if r.Method == http.MethodGet && len(parts) == 2 {
		a.gets++
		if r.URL.Query().Get("expected_workforce_id") != boardID {
			a.problem(w, 409)
			return
		}
		if a.memberStatus != 0 {
			a.problem(w, a.memberStatus)
			return
		}
		if a.membersNull || a.malformedMembers {
			w.Header().Set("Content-Type", "application/json")
			body := "null"
			if a.malformedMembers {
				body = `{broken`
			}
			_, _ = io.WriteString(w, body)
			return
		}
		workforceJSON(w, 200, a.rows[boardID])
		if a.renameBeforeWrite {
			a.renameBeforeWrite = false
			a.boards[0].Slug = "renamed"
			a.boards = append(a.boards, client.WorkforceListItem{Id: "wf-other", Slug: parts[0]})
			a.rows["wf-other"] = []client.WorkforceMember{workforceRow("user", "other-owner", "owner")}
		}
		return
	}
	if r.Method == http.MethodPut && len(parts) == 2 {
		a.puts++
		var body client.WorkforceMembersSet
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			a.t.Errorf("decode PUT: %v", err)
			a.problem(w, 422)
			return
		}
		if body.ExpectedWorkforceId == nil || *body.ExpectedWorkforceId == "" || len(body.Members) != 1 {
			a.t.Error("PUT must carry one member and its strict-body board-ID fence")
			a.problem(w, 422)
			return
		}
		if a.oldWriteReplica {
			a.problem(w, 422)
			return
		} // old StrictModel rejects the unknown field
		if *body.ExpectedWorkforceId != boardID || r.URL.Query().Get("expected_workforce_id") != boardID {
			a.problem(w, 409)
			return
		}
		if a.writeStatus != 0 {
			a.problem(w, a.writeStatus)
			return
		}
		member := body.Members[0]
		if member.Role == nil {
			a.t.Error("PUT must send the explicit desired role")
			a.problem(w, 422)
			return
		}
		for _, row := range a.rows[boardID] {
			if row.ActorType == member.ActorType && row.ActorId == member.ActorId && row.Role == "owner" && *member.Role != "owner" && a.ownerCount(boardID) == 1 {
				a.problem(w, 409)
				return
			}
		}
		row := workforceRow(string(member.ActorType), member.ActorId, *member.Role)
		a.replace(boardID, row)
		a.mutations++
		if a.missingWriteAck {
			w.Header().Del(client.WorkforceIdentityHeader)
		}
		if a.missingWrittenRow {
			workforceJSON(w, 200, []client.WorkforceMember{})
			return
		}
		if a.wrongWrittenRole {
			row.Role = "unexpected"
			workforceJSON(w, 200, []client.WorkforceMember{row})
			return
		}
		workforceJSON(w, 200, a.rows[boardID])
		return
	}
	if r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "delete" {
		a.removes++
		var body client.WorkforceMembersRemove
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			a.t.Errorf("decode removal: %v", err)
			a.problem(w, 422)
			return
		}
		if body.ExpectedWorkforceId == nil || len(body.Members) != 1 {
			a.t.Error("removal must carry one member and its strict-body ID fence")
			a.problem(w, 422)
			return
		}
		if a.oldWriteReplica {
			a.problem(w, 422)
			return
		}
		if *body.ExpectedWorkforceId != boardID || r.URL.Query().Get("expected_workforce_id") != boardID {
			a.problem(w, 409)
			return
		}
		if a.writeStatus != 0 {
			a.problem(w, a.writeStatus)
			return
		}
		member := body.Members[0]
		for i, row := range a.rows[boardID] {
			if row.ActorType == member.ActorType && row.ActorId == member.ActorId {
				if row.Role == "owner" && a.ownerCount(boardID) == 1 {
					a.problem(w, 409)
					return
				}
				a.rows[boardID] = append(a.rows[boardID][:i], a.rows[boardID][i+1:]...)
				a.mutations++
				if a.missingWriteAck {
					w.Header().Del(client.WorkforceIdentityHeader)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(204) // deliberately empty JSON-labelled body
				return
			}
		}
		a.problem(w, 404)
		return
	}
	a.t.Errorf("unexpected method/path %s %s", r.Method, r.URL.Path)
	a.problem(w, 405)
}

func (a *workforceMemberAPI) ownerCount(id string) int {
	count := 0
	for _, row := range a.rows[id] {
		if row.Role == "owner" {
			count++
		}
	}
	return count
}

// replace operates under the fake's mutex, including in the HTTP handler.
func (a *workforceMemberAPI) replace(id string, row client.WorkforceMember) {
	for i, existing := range a.rows[id] {
		if existing.ActorType == row.ActorType && existing.ActorId == row.ActorId {
			row.CreatedAt = existing.CreatedAt
			a.rows[id][i] = row
			return
		}
	}
	a.rows[id] = append(a.rows[id], row)
}

func (a *workforceMemberAPI) change(f func(*workforceMemberAPI)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f(a)
}

func (a *workforceMemberAPI) checkRows(wantBotRole string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	roles := map[string]string{}
	for _, row := range a.rows["wf-1"] {
		roles[string(row.ActorType)+"/"+row.ActorId] = row.Role
	}
	if roles["api_key/creator"] != "owner" {
		return fmt.Errorf("creator changed: %v", roles)
	}
	if got := roles["bot/bot-1"]; got != wantBotRole {
		return fmt.Errorf("bot role %q, want %q: %v", got, wantBotRole, roles)
	}
	return nil
}

func workforceMemberConfig(endpoint, roleLine string) string {
	return fmt.Sprintf(`
provider "botyard" {
  endpoint = %q
  api_key = "byk_test"
  org_id = "org-1"
}
resource "botyard_workforce_member" "bot" {
  workforce_id = "wf-1"
  actor_type = "bot"
  actor_id = "bot-1"
  %s
}
`, endpoint, roleLine)
}

func TestWorkforceMember_Lifecycle(t *testing.T) {
	api, srv := newWorkforceMemberAPI(t)
	config := func(role string) string { return workforceMemberConfig(srv.URL, role) }
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             func(*terraform.State) error { return api.checkRows("") },
		Steps: []resource.TestStep{
			{Config: config(""), Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("botyard_workforce_member.bot", "id", "wf-1/bot/bot-1"),
				resource.TestCheckResourceAttr("botyard_workforce_member.bot", "role", "member"),
				resource.TestCheckResourceAttr("botyard_workforce_member.bot", "workforce_slug", "platform-development"),
				checkFake(func() error { return api.checkRows("member") }),
			)},
			{PreConfig: func() {
				api.change(func(a *workforceMemberAPI) { a.replace("wf-1", workforceRow("user", "manual", "owner")) })
			}, Config: config(""), PlanOnly: true},
			{Config: config(`role = "owner"`), ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction("botyard_workforce_member.bot", plancheck.ResourceActionUpdate)}}, Check: resource.TestCheckResourceAttr("botyard_workforce_member.bot", "role", "owner")},
			{PreConfig: func() { api.change(func(a *workforceMemberAPI) { a.boards[0].Slug = "renamed" }) }, Config: config(`role = "owner"`), Check: resource.TestCheckResourceAttr("botyard_workforce_member.bot", "workforce_slug", "renamed")},
			{ResourceName: "botyard_workforce_member.bot", ImportState: true, ImportStateId: "wf-1/bot/bot-1", ImportStateVerify: true},
			{PreConfig: func() {
				api.change(func(a *workforceMemberAPI) { a.replace("wf-1", workforceRow("bot", "bot-1", "member")) })
			}, Config: config(`role = "owner"`), PlanOnly: true, ExpectNonEmptyPlan: true},
			{Config: config(`role = "owner"`), Check: checkFake(func() error { return api.checkRows("owner") })},
			{PreConfig: func() {
				api.change(func(a *workforceMemberAPI) {
					rows := a.rows["wf-1"]
					for i, row := range rows {
						if row.ActorType == client.ActorTypeBot && row.ActorId == "bot-1" {
							a.rows["wf-1"] = append(rows[:i], rows[i+1:]...)
							return
						}
					}
				})
			}, Config: config(`role = "owner"`), PlanOnly: true, ExpectNonEmptyPlan: true},
			{Config: config(`role = "owner"`), Check: checkFake(func() error { return api.checkRows("owner") })},
		},
	})
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.rows["wf-1"]) != 2 {
		t.Fatalf("destroy must leave exactly creator + unmanaged user, got %v", api.rows["wf-1"])
	}
}

func workforceResourceFor(t *testing.T, endpoint string) *WorkforceMemberResource {
	t.Helper()
	return &WorkforceMemberResource{data: &providerData{client: newBotClient(t, endpoint, "byk_test"), orgID: "org-1"}}
}

func workforceModel(role string) WorkforceMemberResourceModel {
	return WorkforceMemberResourceModel{ID: types.StringValue("wf-1/bot/bot-1"), WorkforceID: types.StringValue("wf-1"), WorkforceSlug: types.StringValue("platform-development"), ActorType: types.StringValue("bot"), ActorID: types.StringValue("bot-1"), Role: types.StringValue(role), CreatedAt: types.StringValue(fakeTime.Format("2006-01-02T15:04:05Z07:00"))}
}

func workforceState(t *testing.T, model WorkforceMemberResourceModel) tfsdk.State {
	t.Helper()
	var schema fwresource.SchemaResponse
	(&WorkforceMemberResource{}).Schema(context.Background(), fwresource.SchemaRequest{}, &schema)
	state := tfsdk.State{Schema: schema.Schema, Raw: tftypes.NewValue(schema.Schema.Type().TerraformType(context.Background()), nil)}
	if diags := state.Set(context.Background(), &model); diags.HasError() {
		t.Fatalf("state: %v", diags)
	}
	return state
}

func TestWorkforceMember_RefusesUnsafeWrites(t *testing.T) {
	cases := []struct {
		name   string
		change func(*workforceMemberAPI)
		want   string
	}{
		{"old API", func(a *workforceMemberAPI) { a.ack = false }, "acknowledge workforce identity"},
		{"mixed old write replica", func(a *workforceMemberAPI) { a.oldWriteReplica = true }, "HTTP 422"},
		{"list forbidden", func(a *workforceMemberAPI) { a.listStatus = 403 }, "HTTP 403"},
		{"member forbidden", func(a *workforceMemberAPI) { a.memberStatus = 403 }, "forbidden"},
		{"list null", func(a *workforceMemberAPI) { a.listNull = true }, "Unexpected response listing"},
		{"members null", func(a *workforceMemberAPI) { a.membersNull = true }, "Unexpected response"},
		{"members malformed", func(a *workforceMemberAPI) { a.malformedMembers = true }, "Error reading"},
		{"board gone", func(a *workforceMemberAPI) { a.boards = []client.WorkforceListItem{} }, "Workforce not found"},
		{"write forbidden", func(a *workforceMemberAPI) { a.writeStatus = 403 }, "forbidden"},
		{"last owner or identity conflict", func(a *workforceMemberAPI) { a.writeStatus = 409 }, "conflict"},
		{"principal not found", func(a *workforceMemberAPI) { a.writeStatus = 404 }, "principal not found"},
		{"slug reused before PUT", func(a *workforceMemberAPI) { a.renameBeforeWrite = true }, "conflict"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api, srv := newWorkforceMemberAPI(t)
			api.change(tc.change)
			r := workforceResourceFor(t, srv.URL)
			model := workforceModel("member")
			var diags fwresource.CreateResponse
			r.put(context.Background(), &model, &diags.Diagnostics)
			assertDiag(t, diags.Diagnostics.Errors(), tc.want)
			api.mu.Lock()
			defer api.mu.Unlock()
			if api.mutations != 0 {
				t.Fatalf("unsafe request mutated memberships: %d", api.mutations)
			}
			if api.puts > 1 {
				t.Fatalf("write retried %d times", api.puts)
			}
		})
	}
}

func TestWorkforceMember_InvalidWriteAcknowledgements(t *testing.T) {
	cases := []struct {
		name   string
		change func(*workforceMemberAPI)
		want   string
	}{
		{"missing row", func(a *workforceMemberAPI) { a.missingWrittenRow = true }, "Invalid workforce member write response"},
		{"wrong role", func(a *workforceMemberAPI) { a.wrongWrittenRole = true }, "Invalid workforce member write response"},
		{"missing identity ack", func(a *workforceMemberAPI) { a.missingWriteAck = true }, "acknowledge workforce identity"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api, srv := newWorkforceMemberAPI(t)
			api.change(tc.change)
			r := workforceResourceFor(t, srv.URL)
			model := workforceModel("member")
			var response fwresource.CreateResponse
			r.put(context.Background(), &model, &response.Diagnostics)
			assertDiag(t, response.Diagnostics.Errors(), tc.want)
			api.mu.Lock()
			defer api.mu.Unlock()
			if api.puts != 1 || api.mutations != 1 {
				t.Fatalf("expected one write, no retry: puts=%d mutations=%d", api.puts, api.mutations)
			}
		})
	}
}

func TestWorkforceMember_ReadRenameAndAbsence(t *testing.T) {
	ctx := context.Background()
	t.Run("rename during lookup retries read only", func(t *testing.T) {
		api, srv := newWorkforceMemberAPI(t)
		api.change(func(a *workforceMemberAPI) {
			a.renameAfterList = true
			a.replace("wf-1", workforceRow("bot", "bot-1", "member"))
		})
		r := workforceResourceFor(t, srv.URL)
		state := workforceState(t, workforceModel("member"))
		response := fwresource.ReadResponse{State: state}
		r.Read(ctx, fwresource.ReadRequest{State: state}, &response)
		if response.Diagnostics.HasError() {
			t.Fatal(response.Diagnostics)
		}
		var model WorkforceMemberResourceModel
		if diags := response.State.Get(ctx, &model); diags.HasError() {
			t.Fatal(diags)
		}
		if model.WorkforceSlug.ValueString() != "renamed" {
			t.Fatalf("did not follow ID to new slug: %v", model)
		}
		api.mu.Lock()
		defer api.mu.Unlock()
		if api.lists != 2 || api.mutations != 0 {
			t.Fatalf("lists=%d mutations=%d", api.lists, api.mutations)
		}
	})
	for _, parentGone := range []bool{false, true} {
		t.Run(fmt.Sprintf("parent_gone_%t", parentGone), func(t *testing.T) {
			api, srv := newWorkforceMemberAPI(t)
			if parentGone {
				api.change(func(a *workforceMemberAPI) { a.boards = []client.WorkforceListItem{} })
			}
			r := workforceResourceFor(t, srv.URL)
			state := workforceState(t, workforceModel("member"))
			response := fwresource.ReadResponse{State: state}
			r.Read(ctx, fwresource.ReadRequest{State: state}, &response)
			if response.Diagnostics.HasError() || !response.State.Raw.IsNull() {
				t.Fatalf("missing row/board not removed: %v", response.Diagnostics)
			}
		})
	}
}

func TestWorkforceMember_Delete(t *testing.T) {
	for _, tc := range []struct {
		name      string
		setup     func(*workforceMemberAPI)
		want      string
		mutations int
	}{
		{"empty 204", func(a *workforceMemberAPI) {}, "", 1},
		{"already absent", func(a *workforceMemberAPI) { a.rows["wf-1"] = a.rows["wf-1"][:1] }, "", 0},
		{"board absent", func(a *workforceMemberAPI) { a.boards = []client.WorkforceListItem{} }, "", 0},
		{"old API", func(a *workforceMemberAPI) { a.ack = false }, "acknowledge workforce identity", 0},
		{"old write replica", func(a *workforceMemberAPI) { a.oldWriteReplica = true }, "HTTP 422", 0},
		{"forbidden", func(a *workforceMemberAPI) { a.writeStatus = 403 }, "forbidden", 0},
		{"last owner conflict", func(a *workforceMemberAPI) { a.writeStatus = 409 }, "conflict", 0},
		{"row disappeared after read", func(a *workforceMemberAPI) { a.writeStatus = 404 }, "", 0},
		{"slug reuse", func(a *workforceMemberAPI) { a.renameBeforeWrite = true }, "conflict", 0},
		{"missing ack after success", func(a *workforceMemberAPI) { a.missingWriteAck = true }, "Missing workforce identity acknowledgement", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api, srv := newWorkforceMemberAPI(t)
			api.change(func(a *workforceMemberAPI) { a.replace("wf-1", workforceRow("bot", "bot-1", "member")); tc.setup(a) })
			r := workforceResourceFor(t, srv.URL)
			response := fwresource.DeleteResponse{}
			r.Delete(context.Background(), fwresource.DeleteRequest{State: workforceState(t, workforceModel("member"))}, &response)
			if tc.want != "" {
				assertDiag(t, response.Diagnostics.Errors(), tc.want)
			} else if response.Diagnostics.HasError() {
				t.Fatal(response.Diagnostics)
			}
			api.mu.Lock()
			defer api.mu.Unlock()
			if api.mutations != tc.mutations || api.removes > 1 {
				t.Fatalf("mutations=%d removals=%d", api.mutations, api.removes)
			}
		})
	}
}

func TestWorkforceMember_ImportID(t *testing.T) {
	for _, value := range []string{"wf-1/user/user-1", "wf-1/bot/bot-1", "wf-1/api_key/key-1"} {
		id, kind, actor, err := parseWorkforceMemberID(value)
		if err != nil || workforceMemberID(id, kind, actor) != value {
			t.Fatalf("valid import %q: %v", value, err)
		}
	}
	for _, value := range []string{"", "wf-1", "wf-1/bot/", "/bot/bot-1", "wf-1/teams_user/u", "wf-1/bot/b/extra", "wf-1/bot/ bad ", "wf-1/mcp_workload/w"} {
		if _, _, _, err := parseWorkforceMemberID(value); err == nil {
			t.Fatalf("accepted invalid import %q", value)
		}
	}
}
