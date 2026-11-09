package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
