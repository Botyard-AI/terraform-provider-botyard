package provider

import (
	"context"
	"net/http"
	"os"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
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

	// Unknown values (e.g. interpolated from a not-yet-known resource) cannot be
	// resolved at configure time — fail with a clear, attribute-scoped error.
	if cfg.APIKey.IsUnknown() {
		resp.Diagnostics.AddAttributeError(path.Root("api_key"),
			"Unknown Botyard API key",
			"The provider cannot be configured with an unknown api_key value. Set it statically or via BOTYARD_API_KEY.")
	}
	if cfg.OrgID.IsUnknown() {
		resp.Diagnostics.AddAttributeError(path.Root("org_id"),
			"Unknown Botyard organization ID",
			"The provider cannot be configured with an unknown org_id value. Set it statically or via BOTYARD_ORG_ID.")
	}
	if resp.Diagnostics.HasError() {
		return
	}

	endpoint := firstNonEmpty(cfg.Endpoint.ValueString(), os.Getenv("BOTYARD_ENDPOINT"), defaultEndpoint)
	apiKey := firstNonEmpty(cfg.APIKey.ValueString(), os.Getenv("BOTYARD_API_KEY"))
	orgID := firstNonEmpty(cfg.OrgID.ValueString(), os.Getenv("BOTYARD_ORG_ID"))

	if apiKey == "" {
		resp.Diagnostics.AddAttributeError(path.Root("api_key"),
			"Missing Botyard API key",
			"Set the api_key provider attribute or the BOTYARD_API_KEY environment variable.")
	}
	if orgID == "" {
		resp.Diagnostics.AddAttributeError(path.Root("org_id"),
			"Missing Botyard organization ID",
			"Set the org_id provider attribute or the BOTYARD_ORG_ID environment variable.")
	}
	if resp.Diagnostics.HasError() {
		return
	}

	c, err := client.NewClientWithResponses(strings.TrimRight(endpoint, "/"),
		client.WithRequestEditorFn(bearerAuth(apiKey)))
	if err != nil {
		resp.Diagnostics.AddError("Unable to create Botyard API client", err.Error())
		return
	}

	data := &providerData{client: c, orgID: orgID}
	resp.DataSourceData = data
	resp.ResourceData = data
}

func (p *BotyardProvider) Resources(_ context.Context) []func() resource.Resource {
	return nil
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
