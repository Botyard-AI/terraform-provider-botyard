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

	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

const fakeBotsPath = "/v1/orgs/org-1/bots/"

type fakeBotMemberRow struct {
	ID   string
	Role string
}

type fakeBotMembers struct {
	Users []fakeBotMemberRow
	Bots  []fakeBotMemberRow
}

// botMemberAPI is a stateful fake of the per-member bot-access API:
//
//   - GET /members and GET /member-bots list the rows.
//   - PATCH /members and PATCH /member-bots apply `upsert` then `remove` and
//     return the whole list. Removing an absent row is a no-op; a request whose
//     result has no human owner is 422, exactly as the API refuses it.
//   - An unknown bot slug is 404.
//
// forbidWrites simulates the owner gate (403 "Only bot owners ..."). Every
// PATCH body is recorded so tests can assert a resource touches one row only.
type botMemberAPI struct {
	mu   sync.Mutex
	t    *testing.T
	bots map[string]*fakeBotMembers

	forbidWrites bool
	patchBodies  []string
}

func newBotMemberAPI(t *testing.T) (*botMemberAPI, *httptest.Server) {
	t.Helper()
	api := &botMemberAPI{t: t, bots: map[string]*fakeBotMembers{
		"b1": {Users: []fakeBotMemberRow{{ID: "user-creator", Role: botMemberRoleOwner}}},
	}}
	srv := httptest.NewServer(http.HandlerFunc(api.serve))
	t.Cleanup(srv.Close)
	return api, srv
}

func (a *botMemberAPI) serve(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer byk_test" {
		botMemberProblem(w, http.StatusUnauthorized, "missing bearer")
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, fakeBotsPath), "/"), "/")
	if !strings.HasPrefix(r.URL.Path, fakeBotsPath) || len(parts) != 2 {
		a.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		botMemberProblem(w, http.StatusNotFound, "no route")
		return
	}
	b, ok := a.bots[parts[0]]
	if !ok {
		botMemberProblem(w, http.StatusNotFound, "Bot not found")
		return
	}
	users := parts[1] == "members"
	if !users && parts[1] != "member-bots" {
		a.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		botMemberProblem(w, http.StatusNotFound, "no route")
		return
	}
	switch r.Method {
	case http.MethodGet:
		a.writeList(w, b, users)
	case http.MethodPatch:
		a.patch(w, r, b, users)
	default:
		a.t.Errorf("unexpected request %s %s (whole-list PUT must never be used)", r.Method, r.URL.Path)
		botMemberProblem(w, http.StatusMethodNotAllowed, "")
	}
}

// patchBody is the union of BotMembersPatch and BotMemberBotsPatch the fake
// reads: humans carry user_id/role_id, bots bot_id/role.
type patchBody struct {
	Upsert []struct {
		UserID string  `json:"user_id"`
		RoleID *string `json:"role_id"`
		BotID  string  `json:"bot_id"`
		Role   *string `json:"role"`
	} `json:"upsert"`
	Remove []string `json:"remove"`
}

func (a *botMemberAPI) patch(w http.ResponseWriter, r *http.Request, b *fakeBotMembers, users bool) {
	var raw json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		botMemberProblem(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	a.patchBodies = append(a.patchBodies, string(raw))
	if a.forbidWrites {
		botMemberProblem(w, http.StatusForbidden, "Only bot owners can manage members")
		return
	}
	var body patchBody
	if err := json.Unmarshal(raw, &body); err != nil {
		botMemberProblem(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	rows := b.Bots
	if users {
		rows = b.Users
	}
	next := append([]fakeBotMemberRow(nil), rows...)
	for _, u := range body.Upsert {
		id, rolePtr := u.BotID, u.Role
		if users {
			id, rolePtr = u.UserID, u.RoleID
		}
		role := botMemberRoleMember
		if rolePtr != nil {
			role = *rolePtr
		}
		found := false
		for i := range next {
			if next[i].ID == id {
				next[i].Role, found = role, true
			}
		}
		if !found {
			next = append(next, fakeBotMemberRow{ID: id, Role: role})
		}
	}
	for _, id := range body.Remove {
		for i := range next {
			if next[i].ID == id {
				next = append(next[:i], next[i+1:]...)
				break
			}
		}
	}
	if users && !hasOwner(next) {
		botMemberProblem(w, http.StatusUnprocessableEntity, "A bot must keep at least one owner")
		return
	}
	if users {
		b.Users = next
	} else {
		b.Bots = next
	}
	a.writeList(w, b, users)
}

func hasOwner(rows []fakeBotMemberRow) bool {
	for _, r := range rows {
		if r.Role == botMemberRoleOwner {
			return true
		}
	}
	return false
}

func (a *botMemberAPI) writeList(w http.ResponseWriter, b *fakeBotMembers, users bool) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if users {
		out := make([]client.BotMemberResponse, 0, len(b.Users))
		for _, u := range b.Users {
			out = append(out, client.BotMemberResponse{
				UserId: u.ID, RoleId: client.BotMemberRole(u.Role), CreatedAt: fakeTime,
				Email: u.ID + "@example.com", Name: "Name " + u.ID,
			})
		}
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	out := make([]client.BotMemberBotResponse, 0, len(b.Bots))
	for _, m := range b.Bots {
		out = append(out, client.BotMemberBotResponse{
			BotId: m.ID, Role: client.BotMemberBotResponseRole(m.Role), CreatedAt: fakeTime,
			Slug: "slug-" + m.ID, Name: "Bot " + m.ID,
		})
	}
	_ = json.NewEncoder(w).Encode(out)
}

// botMemberProblemBody carries error_code, which the owner-403 detector keys on.
type botMemberProblemBody struct {
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Detail    string `json:"detail"`
	ErrorCode string `json:"error_code,omitempty"`
}

func botMemberProblem(w http.ResponseWriter, status int, detail string) {
	code := ""
	if status == http.StatusForbidden {
		code = "forbidden"
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(botMemberProblemBody{
		Title: http.StatusText(status), Status: status, Detail: detail, ErrorCode: code,
	})
}

// --- helpers operating on fake state (all take the lock) ---

func (a *botMemberAPI) rows(users bool) []fakeBotMemberRow {
	a.mu.Lock()
	defer a.mu.Unlock()
	if users {
		return append([]fakeBotMemberRow(nil), a.bots["b1"].Users...)
	}
	return append([]fakeBotMemberRow(nil), a.bots["b1"].Bots...)
}

func (a *botMemberAPI) roleOf(users bool, id string) string {
	for _, r := range a.rows(users) {
		if r.ID == id {
			return r.Role
		}
	}
	return ""
}

func (a *botMemberAPI) seed(users bool, id, role string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	row := fakeBotMemberRow{ID: id, Role: role}
	if users {
		a.bots["b1"].Users = append(a.bots["b1"].Users, row)
	} else {
		a.bots["b1"].Bots = append(a.bots["b1"].Bots, row)
	}
}

func (a *botMemberAPI) drop(users bool, id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	b := a.bots["b1"]
	rows := &b.Bots
	if users {
		rows = &b.Users
	}
	for i, r := range *rows {
		if r.ID == id {
			*rows = append((*rows)[:i], (*rows)[i+1:]...)
			return
		}
	}
	a.t.Fatalf("drop: no row %s", id)
}

func (a *botMemberAPI) bodies() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.patchBodies...)
}

func (a *botMemberAPI) set(f func(a *botMemberAPI)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f(a)
}

func botMemberTestConfig(endpoint, extra string) string {
	return fmt.Sprintf(`
provider "botyard" {
  endpoint = %q
  api_key  = "byk_test"
  org_id   = "org-1"
}
%s
`, endpoint, extra)
}

func userMemberHCL(name, userID, roleLine string) string {
	return fmt.Sprintf(`
resource "botyard_bot_member" %q {
  bot_slug = "b1"
  user_id  = %q
  %s
}
`, name, userID, roleLine)
}

func botMemberBotHCL(name, botID, roleLine string) string {
	return fmt.Sprintf(`
resource "botyard_bot_member_bot" %q {
  bot_slug      = "b1"
  member_bot_id = %q
  %s
}
`, name, botID, roleLine)
}

// TestBotMember_Lifecycle drives Terraform core against the fake: two resources
// on one bot created in parallel, rows added in the app invisible, role update
// in place, import, out-of-band removal, and destroy leaving foreign rows alone.
func TestBotMember_Lifecycle(t *testing.T) {
	api, srv := newBotMemberAPI(t)
	two := func(roleLine string) string {
		return botMemberTestConfig(srv.URL,
			userMemberHCL("alice", "user-alice", roleLine)+userMemberHCL("bob", "user-bob", ""))
	}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: two(""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("botyard_bot_member.alice", "role", "member"),
					resource.TestCheckResourceAttr("botyard_bot_member.alice", "id", "b1/user-alice"),
					resource.TestCheckResourceAttr("botyard_bot_member.alice", "email", "user-alice@example.com"),
					resource.TestCheckResourceAttr("botyard_bot_member.alice", "created_at", "2026-09-20T12:00:00Z"),
					checkFake(func() error {
						// Both parallel creates survive: neither overwrote the other.
						if api.roleOf(true, "user-alice") != "member" || api.roleOf(true, "user-bob") != "member" {
							return fmt.Errorf("rows = %v, want alice and bob as members", api.rows(true))
						}
						if api.roleOf(true, "user-creator") != "owner" {
							return fmt.Errorf("creator row changed: %v", api.rows(true))
						}
						for _, b := range api.bodies() {
							if strings.Contains(b, "remove") || strings.Count(b, "user_id") != 1 {
								return fmt.Errorf("create must upsert exactly one row, sent %s", b)
							}
						}
						return nil
					}),
				),
			},
			// A row added in the app is not drift.
			{
				PreConfig: func() { api.seed(true, "user-app", botMemberRoleMember) },
				Config:    two(""),
				PlanOnly:  true,
			},
			{
				Config: two(`role = "owner"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("botyard_bot_member.alice", plancheck.ResourceActionUpdate),
					},
				},
				Check: checkFake(func() error {
					if got := api.roleOf(true, "user-alice"); got != "owner" {
						return fmt.Errorf("alice role = %q, want owner", got)
					}
					return nil
				}),
			},
			{
				ResourceName:      "botyard_bot_member.alice",
				ImportState:       true,
				ImportStateId:     "b1/user-alice",
				ImportStateVerify: true,
			},
			// Removed in the app: refresh drops it and the plan re-creates it.
			{
				PreConfig:          func() { api.drop(true, "user-bob") },
				Config:             two(`role = "owner"`),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			// Destroy only alice: bob, the creator and the app-added row stay.
			{
				Config: botMemberTestConfig(srv.URL, userMemberHCL("bob", "user-bob", "")),
				Check: checkFake(func() error {
					if api.roleOf(true, "user-alice") != "" {
						return fmt.Errorf("alice should be removed: %v", api.rows(true))
					}
					for _, id := range []string{"user-bob", "user-creator", "user-app"} {
						if api.roleOf(true, id) == "" {
							return fmt.Errorf("%s should remain: %v", id, api.rows(true))
						}
					}
					return nil
				}),
			},
		},
	})
}

func TestBotMemberBot_Lifecycle(t *testing.T) {
	api, srv := newBotMemberAPI(t)
	cfg := func(roleLine string) string {
		return botMemberTestConfig(srv.URL, botMemberBotHCL("helper", "bot-2", roleLine))
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: cfg(""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("botyard_bot_member_bot.helper", "role", "member"),
					resource.TestCheckResourceAttr("botyard_bot_member_bot.helper", "id", "b1/bot-2"),
					resource.TestCheckResourceAttr("botyard_bot_member_bot.helper", "member_bot_slug", "slug-bot-2"),
				),
			},
			{
				PreConfig: func() { api.seed(false, "bot-app", botMemberRoleMember) },
				Config:    cfg(""),
				PlanOnly:  true,
			},
			{
				Config: cfg(`role = "owner"`),
				Check: checkFake(func() error {
					if got := api.roleOf(false, "bot-2"); got != "owner" {
						return fmt.Errorf("bot-2 role = %q, want owner", got)
					}
					return nil
				}),
			},
			{
				ResourceName:      "botyard_bot_member_bot.helper",
				ImportState:       true,
				ImportStateId:     "b1/bot-2",
				ImportStateVerify: true,
			},
			{
				Config: botMemberTestConfig(srv.URL, ""),
				Check: checkFake(func() error {
					if api.roleOf(false, "bot-2") != "" || api.roleOf(false, "bot-app") == "" {
						return fmt.Errorf("destroy must remove only bot-2: %v", api.rows(false))
					}
					return nil
				}),
			},
		},
	})
}

func TestBotMember_ForbiddenNamesTheKeyOwnershipRule(t *testing.T) {
	api, srv := newBotMemberAPI(t)
	api.set(func(a *botMemberAPI) { a.forbidWrites = true })
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config:      botMemberTestConfig(srv.URL, userMemberHCL("alice", "user-alice", "")),
			ExpectError: regexp.MustCompile(`An API key\s+owns\s+only\s+the\s+bots\s+it\s+created`),
		}},
	})
}

// --- direct method tests for refusal and gone paths ------------------------

func botMemberResourceFor(t *testing.T, srvURL string) *BotMemberResource {
	t.Helper()
	return &BotMemberResource{data: &providerData{client: newBotClient(t, srvURL, "byk_test"), orgID: "org-1"}}
}

func botMemberState(t *testing.T, slug, userID, role string) tfsdk.State {
	t.Helper()
	var sch fwresource.SchemaResponse
	(&BotMemberResource{}).Schema(context.Background(), fwresource.SchemaRequest{}, &sch)
	st := tfsdk.State{Schema: sch.Schema, Raw: tftypes.NewValue(sch.Schema.Type().TerraformType(context.Background()), nil)}
	m := BotMemberResourceModel{
		ID: types.StringValue(slug + "/" + userID), BotSlug: types.StringValue(slug),
		UserID: types.StringValue(userID), Role: types.StringValue(role),
		Email: types.StringValue(""), Name: types.StringValue(""),
		CreatedAt: types.StringValue("2026-09-20T12:00:00Z"),
	}
	if d := st.Set(context.Background(), &m); d.HasError() {
		t.Fatalf("state set: %v", d)
	}
	return st
}

func TestBotMember_Delete(t *testing.T) {
	ctx := context.Background()

	t.Run("absent row is success", func(t *testing.T) {
		_, srv := newBotMemberAPI(t)
		resp := &fwresource.DeleteResponse{}
		botMemberResourceFor(t, srv.URL).Delete(ctx,
			fwresource.DeleteRequest{State: botMemberState(t, "b1", "user-gone", "member")}, resp)
		if resp.Diagnostics.HasError() {
			t.Fatalf("removing an absent member must succeed: %v", resp.Diagnostics)
		}
	})

	t.Run("bot gone is success", func(t *testing.T) {
		_, srv := newBotMemberAPI(t)
		resp := &fwresource.DeleteResponse{}
		botMemberResourceFor(t, srv.URL).Delete(ctx,
			fwresource.DeleteRequest{State: botMemberState(t, "b-404", "user-x", "member")}, resp)
		if resp.Diagnostics.HasError() {
			t.Fatalf("delete on a vanished bot should succeed: %v", resp.Diagnostics)
		}
	})

	t.Run("last owner 422 names the remedy", func(t *testing.T) {
		_, srv := newBotMemberAPI(t)
		resp := &fwresource.DeleteResponse{}
		botMemberResourceFor(t, srv.URL).Delete(ctx,
			fwresource.DeleteRequest{State: botMemberState(t, "b1", "user-creator", "owner")}, resp)
		assertDiag(t, resp.Diagnostics.Errors(), "Botyard rejected the membership change",
			"at least one owner", "terraform state rm")
	})
}

func TestBotMember_ReadBotGoneRemoves(t *testing.T) {
	ctx := context.Background()
	_, srv := newBotMemberAPI(t)
	st := botMemberState(t, "b-404", "user-x", "member")
	resp := &fwresource.ReadResponse{State: st}
	botMemberResourceFor(t, srv.URL).Read(ctx, fwresource.ReadRequest{State: st}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("diags: %v", resp.Diagnostics)
	}
	if !resp.State.Raw.IsNull() {
		t.Error("a 404 on the member list should remove the membership from state")
	}
}

func TestParseBotMembershipID(t *testing.T) {
	slug, id, err := parseBotMembershipID("b1/u1", "user_id")
	if err != nil || slug != "b1" || id != "u1" {
		t.Errorf("parse = %q,%q,%v", slug, id, err)
	}
	for _, bad := range []string{"", "b1", "b1/", "/u1", "b1/u1/x"} {
		if _, _, err := parseBotMembershipID(bad, "user_id"); err == nil {
			t.Errorf("parse(%q) should fail", bad)
		}
	}
}
