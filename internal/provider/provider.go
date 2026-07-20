// Package provider implements the Botyard Terraform provider: the provider
// configuration, its resources, and data sources.
package provider

import (
	"context"
	"net/http"
	"os"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

// defaultEndpoint is the production Botyard API base URL used when neither the
// provider config nor BOTYARD_ENDPOINT is set.
const defaultEndpoint = "https://api.botyard.io"

// Ensure BotyardProvider satisfies the provider.Provider interface.
var _ provider.Provider = (*BotyardProvider)(nil)

// BotyardProvider is the Terraform provider for Botyard.
type BotyardProvider struct {
	// version is set at build time and surfaced in the User-Agent / provider
	// metadata.
	version string
}

// BotyardProviderModel maps the provider configuration block.
type BotyardProviderModel struct {
	Endpoint types.String `tfsdk:"endpoint"`
	APIKey   types.String `tfsdk:"api_key"`
	OrgID    types.String `tfsdk:"org_id"`
}

// providerData is the shared, configured state handed to every resource and
// data source via their Configure methods.
type providerData struct {
	client *client.ClientWithResponses
	orgID  string
}

// New returns a provider factory bound to the given build version.
func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &BotyardProvider{version: version}
	}
}

func (p *BotyardProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "botyard"
	resp.Version = p.version
}

func (p *BotyardProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The Botyard provider manages Botyard platform resources through the public API. " +
			"Authenticate with an organization-scoped API key; every resource is scoped to the configured organization.",
		Attributes: map[string]schema.Attribute{
			"endpoint": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Base URL of the Botyard API. May also be set via the `BOTYARD_ENDPOINT` " +
					"environment variable. Defaults to `" + defaultEndpoint + "`.",
			},
			"api_key": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "Organization-scoped Botyard API key (`byk_...`). May also be set via the " +
					"`BOTYARD_API_KEY` environment variable. Prefer the environment variable so the key does not " +
					"land in Terraform configuration or state.",
			},
			"org_id": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Botyard organization ID that resources are managed within. May also be set " +
					"via the `BOTYARD_ORG_ID` environment variable.",
			},
		},
	}
}

func (p *BotyardProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg BotyardProviderModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	resolved, diags := resolveProviderConfig(cfg, os.Getenv)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	c, err := client.NewClientWithResponses(resolved.endpoint,
		client.WithRequestEditorFn(bearerAuth(resolved.apiKey)))
	if err != nil {
		resp.Diagnostics.AddError("Unable to create Botyard API client", err.Error())
		return
	}

	data := &providerData{client: c, orgID: resolved.orgID}
	resp.DataSourceData = data
	resp.ResourceData = data
}

// resolvedConfig holds effective provider settings after env fallbacks.
type resolvedConfig struct {
	endpoint string
	apiKey   string
	orgID    string
}

// resolveProviderConfig validates the provider model and resolves effective
// values, applying BOTYARD_* environment fallbacks. getenv is injected so the
// resolution is unit-testable. Unknown or missing required values produce
// attribute-scoped diagnostics.
func resolveProviderConfig(cfg BotyardProviderModel, getenv func(string) string) (resolvedConfig, diag.Diagnostics) {
	var diags diag.Diagnostics

	// Unknown values (e.g. interpolated from a not-yet-known resource) cannot be
	// resolved at configure time — fail with a clear, attribute-scoped error.
	for _, u := range []struct {
		val  interface{ IsUnknown() bool }
		attr string
		env  string
	}{
		{cfg.Endpoint, "endpoint", "BOTYARD_ENDPOINT"},
		{cfg.APIKey, "api_key", "BOTYARD_API_KEY"},
		{cfg.OrgID, "org_id", "BOTYARD_ORG_ID"},
	} {
		if u.val.IsUnknown() {
			diags.AddAttributeError(path.Root(u.attr),
				"Unknown Botyard provider configuration",
				"The provider cannot be configured with an unknown "+u.attr+" value. "+
					"Set it statically or via the "+u.env+" environment variable.")
		}
	}
	if diags.HasError() {
		return resolvedConfig{}, diags
	}

	endpoint := firstNonEmpty(cfg.Endpoint.ValueString(), getenv("BOTYARD_ENDPOINT"), defaultEndpoint)
	apiKey := firstNonEmpty(cfg.APIKey.ValueString(), getenv("BOTYARD_API_KEY"))
	orgID := firstNonEmpty(cfg.OrgID.ValueString(), getenv("BOTYARD_ORG_ID"))

	if apiKey == "" {
		diags.AddAttributeError(path.Root("api_key"),
			"Missing Botyard API key",
			"Set the api_key provider attribute or the BOTYARD_API_KEY environment variable.")
	}
	if orgID == "" {
		diags.AddAttributeError(path.Root("org_id"),
			"Missing Botyard organization ID",
			"Set the org_id provider attribute or the BOTYARD_ORG_ID environment variable.")
	}

	return resolvedConfig{
		endpoint: strings.TrimRight(endpoint, "/"),
		apiKey:   apiKey,
		orgID:    orgID,
	}, diags
}

func (p *BotyardProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewMcpServerResource,
		NewVaultSecretResource,
	}
}

func (p *BotyardProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		NewBotDataSource,
	}
}

// bearerAuth returns a request editor that attaches the API key as a Bearer
// token on every request.
func bearerAuth(apiKey string) client.RequestEditorFn {
	return func(_ context.Context, r *http.Request) error {
		r.Header.Set("Authorization", "Bearer "+apiKey)
		return nil
	}
}

// firstNonEmpty returns the first non-empty string in vals, or "".
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
