package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMembershipWriteClientPreservesConfigurationAndReadRedirects(t *testing.T) {
	redirects, destinations := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test" {
			t.Error("request editor was lost")
		}
		if r.URL.Path == "/destination" {
			destinations++
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Location", "/destination")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	configured := &http.Client{Timeout: 3 * time.Second, Transport: http.DefaultTransport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { redirects++; return nil }}
	c, err := NewClientWithResponses(srv.URL, WithHTTPClient(configured), WithRequestEditorFn(func(_ context.Context, r *http.Request) error {
		r.Header.Set("Authorization", "Bearer test")
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	writes, err := c.membershipWriteClient()
	if err != nil {
		t.Fatal(err)
	}
	cloned := writes.ClientInterface.(*Client).Client.(*http.Client)
	if cloned == configured || cloned.Timeout != configured.Timeout || cloned.Transport != configured.Transport {
		t.Fatal("membership policy must copy and retain existing HTTP settings")
	}
	_, err = c.SetWorkforceMember(context.Background(), "org", "board", "id", ActorType("bot"), "bot", "member")
	if err == nil || !strings.Contains(err.Error(), "redirect refused") || redirects != 0 || destinations != 0 {
		t.Fatalf("write redirect followed or lost diagnostic: %v redirects=%d destinations=%d", err, redirects, destinations)
	}
	response, err := c.ListWorkforcesV1OrgsOrgIdWorkforcesGet(context.Background(), "org")
	if err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if redirects != 1 || destinations != 1 {
		t.Fatalf("original read redirect policy changed: %d/%d", redirects, destinations)
	}
}
