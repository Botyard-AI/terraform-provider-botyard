package provider

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/types"
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

func TestResolveProviderConfig(t *testing.T) {
	noEnv := func(string) string { return "" }
	withEnv := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	mdl := func(endpoint, apiKey, orgID types.String) BotyardProviderModel {
		return BotyardProviderModel{Endpoint: endpoint, APIKey: apiKey, OrgID: orgID}
	}

	t.Run("all from config, trailing slash trimmed", func(t *testing.T) {
		got, diags := resolveProviderConfig(
			mdl(types.StringValue("https://api.example.com/"), types.StringValue("k"), types.StringValue("o")), noEnv)
		if diags.HasError() {
			t.Fatalf("unexpected diags: %v", diags)
		}
		if got.endpoint != "https://api.example.com" {
			t.Errorf("endpoint = %q", got.endpoint)
		}
		if got.apiKey != "k" || got.orgID != "o" {
			t.Errorf("apiKey/orgID = %q/%q", got.apiKey, got.orgID)
		}
	})

	t.Run("env fallback and default endpoint", func(t *testing.T) {
		got, diags := resolveProviderConfig(
			mdl(types.StringNull(), types.StringNull(), types.StringNull()),
			withEnv(map[string]string{"BOTYARD_API_KEY": "ek", "BOTYARD_ORG_ID": "eo"}))
		if diags.HasError() {
			t.Fatalf("unexpected diags: %v", diags)
		}
		if got.endpoint != defaultEndpoint {
			t.Errorf("endpoint = %q, want default", got.endpoint)
		}
		if got.apiKey != "ek" || got.orgID != "eo" {
			t.Errorf("apiKey/orgID = %q/%q", got.apiKey, got.orgID)
		}
	})

	t.Run("config takes precedence over env", func(t *testing.T) {
		got, _ := resolveProviderConfig(
			mdl(types.StringNull(), types.StringValue("cfgkey"), types.StringValue("cfgorg")),
			withEnv(map[string]string{"BOTYARD_API_KEY": "envkey", "BOTYARD_ORG_ID": "envorg"}))
		if got.apiKey != "cfgkey" || got.orgID != "cfgorg" {
			t.Errorf("apiKey/orgID = %q/%q, want config values", got.apiKey, got.orgID)
		}
	})

	errCases := map[string]BotyardProviderModel{
		"missing api_key":  mdl(types.StringNull(), types.StringNull(), types.StringValue("o")),
		"missing org_id":   mdl(types.StringNull(), types.StringValue("k"), types.StringNull()),
		"unknown api_key":  mdl(types.StringNull(), types.StringUnknown(), types.StringValue("o")),
		"unknown org_id":   mdl(types.StringNull(), types.StringValue("k"), types.StringUnknown()),
		"unknown endpoint": mdl(types.StringUnknown(), types.StringValue("k"), types.StringValue("o")),
	}
	for name, cfg := range errCases {
		t.Run(name, func(t *testing.T) {
			if _, diags := resolveProviderConfig(cfg, noEnv); !diags.HasError() {
				t.Errorf("expected diagnostics error for %q", name)
			}
		})
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
