package provider

import (
	"context"
	"fmt"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"testing"

	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
)

func TestWorkforceMember_Delete404RechecksIdentity(t *testing.T) {
	for _, rowGone := range []bool{false, true} {
		t.Run(fmt.Sprintf("row_gone_%t", rowGone), func(t *testing.T) {
			api, _ := newWorkforceMemberAPI(t)
			api.change(func(a *workforceMemberAPI) { a.replace("wf-1", workforceRow("bot", "bot-1", "member")) })
			posts := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method == http.MethodPost {
					posts++
					api.change(func(a *workforceMemberAPI) {
						a.boards[0].Slug = "renamed"
						if rowGone {
							a.rows["wf-1"] = a.rows["wf-1"][:1]
						}
					})
				}
				api.serve(w, req)
			}))
			defer srv.Close()
			state := workforceState(t, workforceModel())
			response := fwresource.DeleteResponse{State: state}
			workforceResourceFor(t, srv.URL).Delete(context.Background(), fwresource.DeleteRequest{State: state}, &response)
			if rowGone {
				if response.Diagnostics.HasError() {
					t.Fatal(response.Diagnostics)
				}
			} else if !response.Diagnostics.HasError() || !response.State.Raw.Equal(state.Raw) {
				t.Fatalf("live renamed membership must preserve state with error: %v", response.Diagnostics)
			}
			api.mu.Lock()
			defer api.mu.Unlock()
			if posts != 1 || api.lists != 2 || api.mutations != 0 {
				t.Fatalf("posts=%d lists=%d mutations=%d", posts, api.lists, api.mutations)
			}
		})
	}
}

func TestWorkforceMember_MalformedBoardIdentitiesPreserveState(t *testing.T) {
	for _, body := range []string{`[{}]`, `[null]`, `[{"id":null,"slug":"platform-development"}]`, `[{"id":"other","slug":null}]`} {
		t.Run(body, func(t *testing.T) {
			writes := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method != http.MethodGet {
					writes++
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, body)
			}))
			defer srv.Close()
			r := workforceResourceFor(t, srv.URL)
			state := workforceState(t, workforceModel())
			read := fwresource.ReadResponse{State: state}
			r.Read(context.Background(), fwresource.ReadRequest{State: state}, &read)
			if !read.Diagnostics.HasError() || !read.State.Raw.Equal(state.Raw) {
				t.Fatalf("malformed list lost read state: %v", read.Diagnostics)
			}
			remove := fwresource.DeleteResponse{State: state}
			r.Delete(context.Background(), fwresource.DeleteRequest{State: state}, &remove)
			if !remove.Diagnostics.HasError() || !remove.State.Raw.Equal(state.Raw) || writes != 0 {
				t.Fatalf("malformed list allowed delete: %v writes=%d", remove.Diagnostics, writes)
			}
		})
	}
}

// Exercise a real Terraform Create, not just the internal write helper. The
// server commits the row before replacing its response with an unusable one.
func TestWorkforceMember_UncertainTerraformCreate(t *testing.T) {
	for _, mode := range []string{"null", "proxy 502", "missing acknowledgement", "lost response"} {
		t.Run(mode, func(t *testing.T) {
			api, _ := newWorkforceMemberAPI(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method != http.MethodPut {
					api.serve(w, req)
					return
				}
				committed := httptest.NewRecorder()
				api.serve(committed, req)
				if committed.Code != http.StatusOK {
					t.Errorf("write did not commit: %d", committed.Code)
				}
				w.Header().Set("Content-Type", "application/json")
				switch mode {
				case "proxy 502":
					w.WriteHeader(http.StatusBadGateway)
					_, _ = io.WriteString(w, `{"title":"upstream response lost"}`)
				case "lost response":
					hijacker, ok := w.(http.Hijacker)
					if !ok {
						t.Error("test server does not support connection loss")
						return
					}
					conn, _, err := hijacker.Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = conn.Close()
				case "null":
					_, _ = io.WriteString(w, "null")
				default:
					_, _ = w.Write(committed.Body.Bytes())
				}
			}))
			defer srv.Close()
			checkedState := false
			checkAbsent := func(state *terraform.State) error {
				checkedState = true
				for _, module := range state.Modules {
					for _, row := range module.Resources {
						if row.Type == "botyard_workforce_member" {
							return fmt.Errorf("uncertain create unexpectedly tracked a grant: %v", row)
						}
					}
				}
				return nil
			}
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Steps: []resource.TestStep{
					{Config: workforceMemberConfig(srv.URL, ""), ExpectError: regexp.MustCompile(`(?s)may have been written.*terraform import.*wf-1/bot/bot-1.*deliberately remove.*No\s+automatic\s+retry`)},
					{RefreshState: true, ExpectNonEmptyPlan: true, Check: checkAbsent},
				},
			})
			api.mu.Lock()
			defer api.mu.Unlock()
			if !checkedState || api.puts != 1 || api.mutations != 1 || findWorkforceMember(api.rows["wf-1"], "bot", "bot-1") == nil {
				t.Fatalf("must leave untracked committed grant without retry: checked=%t puts=%d mutations=%d", checkedState, api.puts, api.mutations)
			}
		})
	}
}

func TestWorkforceMember_WriteRedirectNeverReplayed(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		for _, method := range []string{http.MethodPut, http.MethodPost} {
			t.Run(fmt.Sprintf("%s_%d", method, status), func(t *testing.T) {
				api, _ := newWorkforceMemberAPI(t)
				if method == http.MethodPost {
					api.change(func(a *workforceMemberAPI) { a.replace("wf-1", workforceRow("bot", "bot-1", "member")) })
				}
				writes, redirected := 0, 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					if req.URL.Path == "/redirected" {
						redirected++
						w.WriteHeader(http.StatusOK)
						return
					}
					if req.Method == method {
						writes++
						committed := httptest.NewRecorder()
						api.serve(committed, req)
						if committed.Code != http.StatusOK && committed.Code != http.StatusNoContent {
							t.Errorf("write did not commit: %d", committed.Code)
						}
						w.Header().Set("Location", "/redirected")
						w.WriteHeader(status)
						return
					}
					api.serve(w, req)
				}))
				defer srv.Close()
				r := workforceResourceFor(t, srv.URL)
				if method == http.MethodPut {
					plan := workforceModel()
					var diagnostics diag.Diagnostics
					r.put(context.Background(), &plan, &diagnostics)
					if !diagnostics.HasError() || !strings.Contains(diagnostics.Errors()[0].Detail(), "may have been written") {
						t.Fatalf("expected uncertain write diagnostic: %v", diagnostics)
					}
				} else {
					state := workforceState(t, workforceModel())
					response := fwresource.DeleteResponse{State: state}
					r.Delete(context.Background(), fwresource.DeleteRequest{State: state}, &response)
					if !response.Diagnostics.HasError() || !response.State.Raw.Equal(state.Raw) || !strings.Contains(response.Diagnostics.Errors()[0].Detail(), "may have succeeded") {
						t.Fatalf("expected preserved state and uncertain removal diagnostic: %v", response.Diagnostics)
					}
				}
				if writes != 1 || redirected != 0 {
					t.Fatalf("write requests=%d redirected requests=%d; want 1/0", writes, redirected)
				}
				api.mu.Lock()
				defer api.mu.Unlock()
				if api.mutations != 1 {
					t.Fatalf("mutations=%d; want exactly 1", api.mutations)
				}
			})
		}
	}
}
