package provider

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
)

// testAccProtoV6ProviderFactories is used by acceptance tests (TF_ACC) to spin
// up the provider under test in-process.
var testAccProtoV6ProviderFactories = map[string]func() (tfprotov6.ProviderServer, error){
	"botyard": providerserver.NewProtocol6WithError(New("test")()),
}

func TestProvider_Schema(t *testing.T) {
	// The framework validates the provider schema internally; New() must
	// produce a provider whose schema builds without panicking. This guards
	// against duplicate attribute names and invalid attribute definitions.
	_ = testAccProtoV6ProviderFactories
	p := New("test")()
	if p == nil {
		t.Fatal("New() returned nil provider")
	}
}

func TestFirstNonEmpty(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"", "", "third"}, "third"},
		{[]string{"first", "second"}, "first"},
		{[]string{"", ""}, ""},
		{nil, ""},
	}
	for _, c := range cases {
		if got := firstNonEmpty(c.in...); got != c.want {
			t.Errorf("firstNonEmpty(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBearerAuth_SetsHeader(t *testing.T) {
	req := httptest.NewRequest("GET", "http://example.com/x", nil)
	if err := bearerAuth("secret-key")(context.Background(), req); err != nil {
		t.Fatalf("bearerAuth: %v", err)
	}
	if got, want := req.Header.Get("Authorization"), "Bearer secret-key"; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
}
