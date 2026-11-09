package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

func TestWorkforceMember_AdoptionAndPrincipalNamespaces(t *testing.T) {
	api, srv := newWorkforceMemberAPI(t)
	created := fakeTime.Add(-time.Hour)
	api.change(func(a *workforceMemberAPI) {
		row := workforceRow("bot", "same-id", "owner")
		row.CreatedAt = created
		a.replace("wf-1", row)
	})
	// Legacy state-check helpers cannot represent string-indexed for_each addresses.
	// Keep all three same-ID principal namespaces explicit; no business assertion is dropped.
	config := fmt.Sprintf(`
provider "botyard" {
  endpoint = %q
  api_key = "byk_test"
  org_id = "org-1"
}
resource "botyard_workforce_member" "bot" {
  workforce_id = "wf-1"
  actor_type = "bot"
  actor_id = "same-id"
}
resource "botyard_workforce_member" "user" {
  workforce_id = "wf-1"
  actor_type = "user"
  actor_id = "same-id"
}
resource "botyard_workforce_member" "api_key" {
  workforce_id = "wf-1"
  actor_type = "api_key"
  actor_id = "same-id"
}
`, srv.URL)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: func(*terraform.State) error {
			api.mu.Lock()
			defer api.mu.Unlock()
			if len(api.rows["wf-1"]) != 1 || api.rows["wf-1"][0].ActorId != "creator" {
				return fmt.Errorf("destroy changed unmanaged ownership: %v", api.rows["wf-1"])
			}
			return nil
		},
		Steps: []resource.TestStep{
			{Config: config, Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(`botyard_workforce_member.bot`, "created_at", created.Format(time.RFC3339)),
				resource.TestCheckResourceAttr(`botyard_workforce_member.bot`, "role", "member"),
				resource.TestCheckResourceAttr(`botyard_workforce_member.user`, "id", "wf-1/user/same-id"),
				resource.TestCheckResourceAttr(`botyard_workforce_member.api_key`, "id", "wf-1/api_key/same-id"),
			)},
			{Config: config, PlanOnly: true},
		},
	})
}

func TestWorkforceMember_ChangingPrincipalReplacesOnlyItsRow(t *testing.T) {
	api, srv := newWorkforceMemberAPI(t)
	first := workforceMemberConfig(srv.URL, "")
	second := strings.Replace(first, `actor_id = "bot-1"`, `actor_id = "bot-2"`, 1)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{Config: first},
			{Config: second, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction("botyard_workforce_member.bot", plancheck.ResourceActionDestroyBeforeCreate),
			}}, Check: resource.TestCheckResourceAttr("botyard_workforce_member.bot", "id", "wf-1/bot/bot-2")},
		},
	})
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.rows["wf-1"]) != 1 || api.rows["wf-1"][0].ActorId != "creator" {
		t.Fatalf("replacement/destroy touched unmanaged rows: %v", api.rows["wf-1"])
	}
}

func TestWorkforceMember_InvalidConfigurationDoesNotCallAPI(t *testing.T) {
	for _, tc := range []struct{ name, old, replacement string }{
		{"role", `actor_id = "bot-1"`, "actor_id = \"bot-1\"\nrole = \"viewer\""},
		{"principal kind", `actor_type = "bot"`, `actor_type = "teams_user"`},
		{"board identifier", `workforce_id = "wf-1"`, `workforce_id = "wf/other"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api, srv := newWorkforceMemberAPI(t)
			config := strings.Replace(workforceMemberConfig(srv.URL, ""), tc.old, tc.replacement, 1)
			resource.UnitTest(t, resource.TestCase{ProtoV6ProviderFactories: testAccProtoV6ProviderFactories, Steps: []resource.TestStep{{Config: config, ExpectError: regexp.MustCompile(`(?s)(must be|must have|must contain|Use an identifier)`)}}})
			api.mu.Lock()
			defer api.mu.Unlock()
			if api.lists != 0 || api.mutations != 0 {
				t.Fatalf("invalid config called API: lists=%d mutations=%d", api.lists, api.mutations)
			}
		})
	}
}

func TestWorkforceMember_ReadErrorsPreserveState(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*workforceMemberAPI)
	}{
		{"403", func(a *workforceMemberAPI) { a.memberStatus = 403 }},
		{"malformed body", func(a *workforceMemberAPI) { a.malformedMembers = true }},
		{"missing required row timestamp", func(a *workforceMemberAPI) {
			row := workforceRow("bot", "bot-1", "member")
			row.CreatedAt = time.Time{}
			a.replace("wf-1", row)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api, srv := newWorkforceMemberAPI(t)
			api.change(tc.change)
			r := workforceResourceFor(t, srv.URL)
			state := workforceState(t, workforceModel("member"))
			response := fwresource.ReadResponse{State: state}
			r.Read(context.Background(), fwresource.ReadRequest{State: state}, &response)
			if !response.Diagnostics.HasError() || !response.State.Raw.Equal(state.Raw) {
				t.Fatalf("failed read must preserve state, diagnostics=%v", response.Diagnostics)
			}
		})
	}
}

func TestWorkforceMember_LastOwnerDiagnostics(t *testing.T) {
	api, srv := newWorkforceMemberAPI(t)
	r := workforceResourceFor(t, srv.URL)
	model := workforceModel("member")
	model.ActorType = types.StringValue("api_key")
	model.ActorID = types.StringValue("creator")
	model.ID = types.StringValue("wf-1/api_key/creator")
	var create fwresource.CreateResponse
	r.put(context.Background(), &model, &create.Diagnostics)
	assertDiag(t, create.Diagnostics.Errors(), "conflict", "another principal owner", "terraform state rm")
	model.Role = types.StringValue("owner")
	var remove fwresource.DeleteResponse
	r.Delete(context.Background(), fwresource.DeleteRequest{State: workforceState(t, model)}, &remove)
	assertDiag(t, remove.Diagnostics.Errors(), "conflict", "another principal owner", "terraform state rm")
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.mutations != 0 {
		t.Fatalf("last owner changed: %d mutations", api.mutations)
	}
}

// A real server commits the write; only its response is lost. This discriminates
// no-retry behavior from a fake that returns an error without any side effect.
type lostWorkforceResponse struct{ base http.RoundTripper }

func (transport lostWorkforceResponse) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if request.Method == http.MethodPut {
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		return nil, fmt.Errorf("simulated lost response after committed membership write")
	}
	return response, nil
}

func TestWorkforceMember_LostWriteResponseIsNotRetried(t *testing.T) {
	api, srv := newWorkforceMemberAPI(t)
	c, err := client.NewClientWithResponses(srv.URL, client.WithHTTPClient(&http.Client{Transport: lostWorkforceResponse{base: http.DefaultTransport}}), client.WithRequestEditorFn(bearerAuth("byk_test")))
	if err != nil {
		t.Fatal(err)
	}
	r := &WorkforceMemberResource{data: &providerData{client: c, orgID: "org-1"}}
	model := workforceModel("member")
	var response fwresource.CreateResponse
	r.put(context.Background(), &model, &response.Diagnostics)
	assertDiag(t, response.Diagnostics.Errors(), "may have been written", "No automatic retry")
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.puts != 1 || api.mutations != 1 {
		t.Fatalf("lost acknowledgement retried or did not reach real side effect: puts=%d mutations=%d", api.puts, api.mutations)
	}
}

func TestWorkforceMember_CancelledRequestDoesNotWrite(t *testing.T) {
	api, srv := newWorkforceMemberAPI(t)
	r := workforceResourceFor(t, srv.URL)
	model := workforceModel("member")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var response fwresource.CreateResponse
	r.put(ctx, &model, &response.Diagnostics)
	assertDiag(t, response.Diagnostics.Errors(), "context canceled")
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.lists != 0 || api.mutations != 0 {
		t.Fatalf("cancelled operation reached API: lists=%d mutations=%d", api.lists, api.mutations)
	}
}
