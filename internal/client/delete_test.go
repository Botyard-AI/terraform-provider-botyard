package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestDeleteWrappers_DoNotParseBody covers the delete wrappers across the
// response shapes the API actually returns. The regression case is a 204 No
// Content served with a JSON content-type and an empty body: the generated
// ...DeleteWithResponse parser json-unmarshals that empty body and returns
// "unexpected end of JSON input", which previously surfaced as a spurious
// "Error deleting …" even though the record was gone. The wrappers read the
// status directly and must not error on it.
func TestDeleteWrappers_DoNotParseBody(t *testing.T) {
	cases := map[string]struct {
		status      int
		contentType string
		body        string
	}{
		"204 empty body, json content-type (the bug)": {http.StatusNoContent, "application/json", ""},
		"204 empty body, no content-type":             {http.StatusNoContent, "", ""},
		"200 with json body":                          {http.StatusOK, "application/json", `{"ok":true}`},
		"404 already gone (problem+json)":             {http.StatusNotFound, "application/problem+json", `{"title":"Not Found","status":404}`},
		"500 error with problem body":                 {http.StatusInternalServerError, "application/problem+json", `{"detail":"boom","status":500}`},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var gotMethod, gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod, gotPath = r.Method, r.URL.Path
				if tc.contentType != "" {
					w.Header().Set("Content-Type", tc.contentType)
				}
				w.WriteHeader(tc.status)
				if tc.body != "" {
					_, _ = w.Write([]byte(tc.body))
				}
			}))
			defer srv.Close()

			c, err := NewClientWithResponses(srv.URL)
			if err != nil {
				t.Fatalf("NewClientWithResponses: %v", err)
			}

			// mcp_server delete
			status, body, err := c.DeleteMcpServer(context.Background(), "org-1", "mcp-1")
			if err != nil {
				t.Fatalf("DeleteMcpServer returned err: %v", err)
			}
			if status != tc.status {
				t.Errorf("DeleteMcpServer status = %d, want %d", status, tc.status)
			}
			if string(body) != tc.body {
				t.Errorf("DeleteMcpServer body = %q, want %q", string(body), tc.body)
			}
			if gotMethod != http.MethodDelete {
				t.Errorf("method = %q, want DELETE", gotMethod)
			}
			if want := "/v1/orgs/org-1/mcp-servers/mcp-1"; gotPath != want {
				t.Errorf("path = %q, want %q", gotPath, want)
			}

			// secret_policy (vault secret) delete
			status, body, err = c.DeleteSecretPolicy(context.Background(), "org-1", "pol-1")
			if err != nil {
				t.Fatalf("DeleteSecretPolicy returned err: %v", err)
			}
			if status != tc.status {
				t.Errorf("DeleteSecretPolicy status = %d, want %d", status, tc.status)
			}
			if string(body) != tc.body {
				t.Errorf("DeleteSecretPolicy body = %q, want %q", string(body), tc.body)
			}
			if want := "/v1/orgs/org-1/secret-policies/pol-1"; gotPath != want {
				t.Errorf("path = %q, want %q", gotPath, want)
			}
		})
	}
}
