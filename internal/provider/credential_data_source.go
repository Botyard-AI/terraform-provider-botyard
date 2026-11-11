package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

var (
	_ datasource.DataSource              = (*CredentialDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*CredentialDataSource)(nil)
)

// CredentialDataSourceModel is the state for the singular botyard_credential.
//
// It mirrors one element of botyard_credentials, except that the vendor is
// `credential_provider`: `provider` is a reserved attribute name at a data
// source's root (it is the meta-argument that selects a provider
// configuration), so it cannot be used here.
type CredentialDataSourceModel struct {
	Slug               types.String `tfsdk:"slug"`
	CredentialID       types.String `tfsdk:"credential_id"`
	Label              types.String `tfsdk:"label"`
	CredentialProvider types.String `tfsdk:"credential_provider"`
	Scope              types.String `tfsdk:"scope"`
	AuthMethod         types.String `tfsdk:"auth_method"`
	KeyPrefix          types.String `tfsdk:"key_prefix"`
	IsDefault          types.Bool   `tfsdk:"is_default"`
	Enabled            types.Bool   `tfsdk:"enabled"`
	AvailableToAllBots types.Bool   `tfsdk:"available_to_all_bots"`
	Managed            types.Bool   `tfsdk:"managed"`
	OrgID              types.String `tfsdk:"org_id"`
	CreatedAt          types.String `tfsdk:"created_at"`
	UpdatedAt          types.String `tfsdk:"updated_at"`
}

// findCredentialBySlug returns the credential whose slug matches, or ok=false
// when none does. Slugs are unique per organization (the API enforces this
// across every live credential), so at most one can match.
func findCredentialBySlug(creds []client.CredentialResponse, slug string) (client.CredentialResponse, bool) {
	for _, c := range creds {
		if c.Slug == slug {
			return c, true
		}
	}
	return client.CredentialResponse{}, false
}

func credentialToSingularModel(c client.CredentialResponse) CredentialDataSourceModel {
	m := credentialToModel(c)
	return CredentialDataSourceModel{
		Slug:               m.Slug,
		CredentialID:       m.CredentialID,
		Label:              m.Label,
		CredentialProvider: m.Provider,
		Scope:              m.Scope,
		AuthMethod:         m.AuthMethod,
		KeyPrefix:          m.KeyPrefix,
		IsDefault:          m.IsDefault,
		Enabled:            m.Enabled,
		AvailableToAllBots: m.AvailableToAllBots,
		Managed:            m.Managed,
		OrgID:              m.OrgID,
		CreatedAt:          m.CreatedAt,
		UpdatedAt:          m.UpdatedAt,
	}
}

// CredentialDataSource resolves one organization credential by slug: the
// "give me the id for this credential" case that botyard_bot_credential_
// assignment needs, without hard-coding a UUID that differs per organization.
type CredentialDataSource struct {
	data *providerData
}

// NewCredentialDataSource is the data-source factory registered with the provider.
func NewCredentialDataSource() datasource.DataSource {
	return &CredentialDataSource{}
}

func (d *CredentialDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_credential"
}

func (d *CredentialDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	attrs := credentialElementAttributes()
	// `provider` is reserved at a data source's root; expose it under a
	// distinct name. bot_id is omitted: the lookup covers org-level
	// credentials only, which never have one.
	attrs["credential_provider"] = attrs["provider"]
	delete(attrs, "provider")
	delete(attrs, "bot_id")
	attrs["slug"] = schema.StringAttribute{
		Required: true,
		MarkdownDescription: "Slug of the credential to look up. Slugs are unique within an organization. " +
			"Every organization has the system-created `user-claude-subscription` credential, which puts a " +
			"bot on each person's own Claude subscription when it is first in the bot's `llm` list.",
	}

	resp.Schema = schema.Schema{
		MarkdownDescription: "Looks up one organization-level credential by slug (non-secret metadata only), " +
			"exposing its `credential_id` for use with `botyard_bot_credential_assignment`. Use it instead of " +
			"a hard-coded UUID, which differs per organization.\n\n" +
			"The vendor is exposed as `credential_provider` because `provider` is a reserved attribute name " +
			"at a data source's root. To list every credential, use `botyard_credentials`.\n\n" +
			"See [Provider credentials](https://docs.botyard.io/docs/provider-credentials) in the Botyard docs.",
		Attributes: attrs,
	}
}

func (d *CredentialDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.data = configureDataSourceProviderData(req, resp)
}

func (d *CredentialDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg CredentialDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	slug := cfg.Slug.ValueString()

	apiResp, err := d.data.client.ListCredentialsV1OrgsOrgIdCredentialsGetWithResponse(ctx, d.data.orgID, nil)
	if err != nil {
		resp.Diagnostics.AddError("Error reading credentials", fmt.Sprintf("Could not list credentials: %s", err))
		return
	}
	if apiResp.JSON200 == nil {
		resp.Diagnostics.AddError(
			"Unexpected response reading credentials",
			fmt.Sprintf("Listing credentials returned HTTP %d: %s", apiResp.StatusCode(), describeAPIError(apiResp.Body)),
		)
		return
	}

	c, found := findCredentialBySlug(*apiResp.JSON200, slug)
	if !found {
		resp.Diagnostics.AddError(
			"Credential not found",
			fmt.Sprintf("No organization credential with slug %q was found.", slug),
		)
		return
	}
	state := credentialToSingularModel(c)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
