package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

var (
	_ datasource.DataSource              = (*CredentialsDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*CredentialsDataSource)(nil)
)

// CredentialModel maps one org credential into Terraform state. `credential_id`
// is the API's identifier field (not `id`); it is what `botyard_bot_credential_
// assignment` references as `credential_id`.
type CredentialModel struct {
	CredentialID       types.String `tfsdk:"credential_id"`
	Slug               types.String `tfsdk:"slug"`
	Label              types.String `tfsdk:"label"`
	Provider           types.String `tfsdk:"provider"`
	Scope              types.String `tfsdk:"scope"`
	AuthMethod         types.String `tfsdk:"auth_method"`
	KeyPrefix          types.String `tfsdk:"key_prefix"`
	IsDefault          types.Bool   `tfsdk:"is_default"`
	Enabled            types.Bool   `tfsdk:"enabled"`
	AvailableToAllBots types.Bool   `tfsdk:"available_to_all_bots"`
	Managed            types.Bool   `tfsdk:"managed"`
	BotID              types.String `tfsdk:"bot_id"`
	OrgID              types.String `tfsdk:"org_id"`
	CreatedAt          types.String `tfsdk:"created_at"`
	UpdatedAt          types.String `tfsdk:"updated_at"`
}

// CredentialsDataSourceModel is the top-level state for botyard_credentials.
type CredentialsDataSourceModel struct {
	Credentials []CredentialModel `tfsdk:"credentials"`
}

// credentialToModel maps an API CredentialResponse into the element model. Only
// non-secret metadata is surfaced (key_prefix is a truncated fingerprint, never
// the full secret).
func credentialToModel(c client.CredentialResponse) CredentialModel {
	return CredentialModel{
		CredentialID:       types.StringValue(c.CredentialId),
		Slug:               types.StringValue(c.Slug),
		Label:              types.StringValue(c.Label),
		Provider:           types.StringValue(string(c.Provider)),
		Scope:              types.StringValue(string(c.Scope)),
		AuthMethod:         types.StringValue(string(c.AuthMethod)),
		KeyPrefix:          strPtrToStr(c.KeyPrefix),
		IsDefault:          types.BoolValue(c.IsDefault),
		Enabled:            types.BoolValue(c.Enabled),
		AvailableToAllBots: types.BoolValue(c.AvailableToAllBots),
		Managed:            boolPtrToBool(c.Managed),
		BotID:              strPtrToStr(c.BotId),
		OrgID:              types.StringValue(c.OrgId),
		CreatedAt:          types.StringValue(c.CreatedAt.Format(time.RFC3339)),
		UpdatedAt:          types.StringValue(c.UpdatedAt.Format(time.RFC3339)),
	}
}

func credentialElementAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"credential_id":         schema.StringAttribute{Computed: true, MarkdownDescription: "Unique credential identifier (UUID). Use this as `credential_id` in `botyard_bot_credential_assignment`."},
		"slug":                  schema.StringAttribute{Computed: true, MarkdownDescription: "URL-safe credential identifier."},
		"label":                 schema.StringAttribute{Computed: true, MarkdownDescription: "Human-readable label."},
		"provider":              schema.StringAttribute{Computed: true, MarkdownDescription: "Provider vendor (e.g. `openai`, `anthropic`, `brave`)."},
		"scope":                 schema.StringAttribute{Computed: true, MarkdownDescription: "What the credential is used for — `llm`, `web_search`, `image_gen`, or `integration`."},
		"auth_method":           schema.StringAttribute{Computed: true, MarkdownDescription: "Authentication method — `api_key`, `oauth`, or `none`."},
		"key_prefix":            schema.StringAttribute{Computed: true, MarkdownDescription: "First few characters of the API key plus `...`, for identification only (never the full secret). May be null."},
		"is_default":            schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether this is the default credential for its scope."},
		"enabled":               schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether the credential is enabled."},
		"available_to_all_bots": schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether the credential is available to every bot in the organization."},
		"managed":               schema.BoolAttribute{Computed: true, MarkdownDescription: "True when Botyard provisions and funds this credential (managed credits) rather than BYOK."},
		"bot_id":                schema.StringAttribute{Computed: true, MarkdownDescription: "Bot this credential is private to (null for org-level credentials)."},
		"org_id":                schema.StringAttribute{Computed: true, MarkdownDescription: "Owning organization ID."},
		"created_at":            schema.StringAttribute{Computed: true, MarkdownDescription: "Creation timestamp (RFC 3339)."},
		"updated_at":            schema.StringAttribute{Computed: true, MarkdownDescription: "Last-update timestamp (RFC 3339)."},
	}
}

// CredentialsDataSource lists the organization's credentials (metadata only).
type CredentialsDataSource struct {
	data *providerData
}

// NewCredentialsDataSource is the data-source factory registered with the provider.
func NewCredentialsDataSource() datasource.DataSource {
	return &CredentialsDataSource{}
}

func (d *CredentialsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_credentials"
}

func (d *CredentialsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Lists the organization's credentials (non-secret metadata only), exposing each " +
			"`credential_id` for use with `botyard_bot_credential_assignment`.",
		Attributes: map[string]schema.Attribute{
			"credentials": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "All credentials in the organization.",
				NestedObject:        schema.NestedAttributeObject{Attributes: credentialElementAttributes()},
			},
		},
	}
}

func (d *CredentialsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.data = configureDataSourceProviderData(req, resp)
}

func (d *CredentialsDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
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

	creds := *apiResp.JSON200
	state := CredentialsDataSourceModel{Credentials: make([]CredentialModel, 0, len(creds))}
	for _, c := range creds {
		state.Credentials = append(state.Credentials, credentialToModel(c))
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
