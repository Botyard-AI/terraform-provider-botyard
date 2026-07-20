package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

// defaultMaxTTLSeconds is the provider-side default for max_ttl_seconds. The
// Runtime Vault API requires a concrete value (60-3600); this keeps the
// attribute optional (per the resource design) while defaulting to a
// conservative five-minute lease cap.
const defaultMaxTTLSeconds = 300

var (
	_ resource.Resource                   = (*VaultSecretResource)(nil)
	_ resource.ResourceWithConfigure      = (*VaultSecretResource)(nil)
	_ resource.ResourceWithImportState    = (*VaultSecretResource)(nil)
	_ resource.ResourceWithValidateConfig = (*VaultSecretResource)(nil)
	_ resource.ResourceWithModifyPlan     = (*VaultSecretResource)(nil)
)

// VaultSecretResource manages an org-scoped Runtime Vault secret (secret policy).
type VaultSecretResource struct {
	data *providerData
}

// VaultSecretResourceModel maps the botyard_vault_secret resource schema.
//
// SecretValue is a write-only argument: it is read from configuration on
// create/update and hashed into SecretValueHash, but is never persisted to
// Terraform state (the framework nullifies write-only values in plan/state, and
// no code path assigns it into a state model).
type VaultSecretResourceModel struct {
	ID              types.String `tfsdk:"id"`
	KeyPath         types.String `tfsdk:"key_path"`
	DisplayName     types.String `tfsdk:"display_name"`
	Description     types.String `tfsdk:"description"`
	Sensitivity     types.String `tfsdk:"sensitivity"`
	AllowAllBots    types.Bool   `tfsdk:"allow_all_bots"`
	MaxTTLSeconds   types.Int64  `tfsdk:"max_ttl_seconds"`
	SecretValue     types.String `tfsdk:"secret_value"`
	SecretValueHash types.String `tfsdk:"secret_value_hash"`
	BotIDs          types.Set    `tfsdk:"bot_ids"`
	LinkedBotCount  types.Int64  `tfsdk:"linked_bot_count"`
	CreatedAt       types.String `tfsdk:"created_at"`
	UpdatedAt       types.String `tfsdk:"updated_at"`
}

// NewVaultSecretResource is the resource factory registered with the provider.
func NewVaultSecretResource() resource.Resource {
	return &VaultSecretResource{}
}

// Metadata sets the resource type name.
func (r *VaultSecretResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_vault_secret"
}

// Schema defines the botyard_vault_secret resource schema.
func (r *VaultSecretResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages an organization-scoped Botyard Runtime Vault secret (a secret *policy*: the " +
			"encrypted value plus its access rules). The secret material is supplied through the write-only " +
			"`secret_value` argument, which is never stored in Terraform state — only a one-way SHA-256 fingerprint " +
			"(`secret_value_hash`) is kept so that changing the value triggers a rotation on the next apply.\n\n" +
			"~> **Terraform 1.11 or later is required** for the write-only `secret_value` argument.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Secret policy ID (UUID).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"key_path": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Dot-delimited Runtime Vault key path (e.g. `github.tokens.read_only`), unique " +
					"within the organization. Changing it forces replacement.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"display_name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Human-readable name shown to bots and admins.",
			},
			"description": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Optional description explaining the vault entry's purpose.",
			},
			"sensitivity": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString(string(client.RuntimeVaultSensitivitySecret)),
				MarkdownDescription: "Sensitivity classification: `secret` (default; masked and exfiltration-scanned) or `plain`.",
			},
			"allow_all_bots": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				MarkdownDescription: "When true, every bot in the organization may request this secret without an " +
					"explicit link. Defaults to `false`. Mutually exclusive with `bot_ids`.",
			},
			"max_ttl_seconds": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				Default:  int64default.StaticInt64(defaultMaxTTLSeconds),
				MarkdownDescription: fmt.Sprintf("Maximum lease TTL in seconds that this policy grants (60-3600). "+
					"Defaults to %d.", defaultMaxTTLSeconds),
			},
			"secret_value": schema.StringAttribute{
				Required:  true,
				Sensitive: true,
				WriteOnly: true,
				MarkdownDescription: "Plaintext secret value. **Write-only**: sent to the API on create/update but " +
					"never stored in Terraform state or plan. Requires Terraform 1.11+. Changing the value updates " +
					"`secret_value_hash`, which triggers a rotation on the next apply.",
			},
			"secret_value_hash": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "One-way SHA-256 (hex) fingerprint of `secret_value`, computed at plan time. Used " +
					"to detect value changes without persisting the secret. High-entropy secrets (API keys, tokens) " +
					"reveal nothing through this hash; a low-entropy secret could in principle be brute-forced by an " +
					"attacker who already has Terraform state access.",
			},
			"bot_ids": schema.SetAttribute{
				Optional:    true,
				Computed:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Authoritative set of bot IDs explicitly linked to this secret. Managed as a full " +
					"replacement set. Ignored when `allow_all_bots = true`. Omitting the argument preserves the last " +
					"applied set; set it to `[]` to remove all links.",
				PlanModifiers: []planmodifier.Set{setplanmodifier.UseStateForUnknown()},
			},
			"linked_bot_count": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "Number of bots explicitly linked to this policy via bot-links.",
			},
			"created_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Creation timestamp (RFC 3339).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"updated_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Last-update timestamp (RFC 3339).",
			},
		},
	}
}

// Configure receives the shared provider data.
func (r *VaultSecretResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	data, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError("Unexpected Resource Configure Type",
			fmt.Sprintf("Expected *providerData, got: %T. This is a bug in the provider.", req.ProviderData))
		return
	}
	r.data = data
}

// ValidateConfig enforces cross-field rules that the schema cannot express.
func (r *VaultSecretResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cfg VaultSecretResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(validateVaultSecretConfig(cfg)...)
}

// validateVaultSecretConfig is the pure, unit-testable configuration validation.
func validateVaultSecretConfig(cfg VaultSecretResourceModel) diag.Diagnostics {
	var diags diag.Diagnostics

	// bot_ids is meaningless (and ignored by the API) when allow_all_bots is true.
	if cfg.AllowAllBots.ValueBool() && !cfg.BotIDs.IsNull() && !cfg.BotIDs.IsUnknown() && len(cfg.BotIDs.Elements()) > 0 {
		diags.AddAttributeError(path.Root("bot_ids"), "Conflicting bot access configuration",
			"`bot_ids` cannot be set when `allow_all_bots = true`; all bots already have access.")
	}

	// sensitivity enum (schema default keeps it non-null, but a user may set it).
	if !cfg.Sensitivity.IsNull() && !cfg.Sensitivity.IsUnknown() {
		switch cfg.Sensitivity.ValueString() {
		case string(client.RuntimeVaultSensitivitySecret), string(client.RuntimeVaultSensitivityPlain):
		default:
			diags.AddAttributeError(path.Root("sensitivity"), "Invalid sensitivity",
				"sensitivity must be `secret` or `plain`.")
		}
	}

	// max_ttl_seconds range.
	if !cfg.MaxTTLSeconds.IsNull() && !cfg.MaxTTLSeconds.IsUnknown() {
		if v := cfg.MaxTTLSeconds.ValueInt64(); v < 60 || v > 3600 {
			diags.AddAttributeError(path.Root("max_ttl_seconds"), "max_ttl_seconds out of range",
				"max_ttl_seconds must be between 60 and 3600.")
		}
	}

	return diags
}

// ModifyPlan hashes the write-only secret_value from configuration into the
// computed secret_value_hash. This is what surfaces a value change as a plan
// diff (the write-only value itself is nullified in plan/state), driving an
// update-in-place rotation without ever persisting the secret.
func (r *VaultSecretResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // resource is being destroyed; no plan to modify.
	}
	var secret types.String
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("secret_value"), &secret)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("secret_value_hash"), hashSecretValue(secret))...)
}

func (r *VaultSecretResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan VaultSecretResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)

	var secret types.String
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("secret_value"), &secret)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if secret.IsNull() || secret.IsUnknown() {
		resp.Diagnostics.AddAttributeError(path.Root("secret_value"), "Missing secret_value",
			"secret_value must be a known, non-null value at apply time.")
		return
	}

	body, diags := buildCreateBody(ctx, plan, secret.ValueString())
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	apiResp, err := r.data.client.CreateSecretPolicyV1OrgsOrgIdSecretPoliciesPostWithBodyWithResponse(
		ctx, r.data.orgID, "application/json", bytes.NewReader(body))
	if err != nil {
		resp.Diagnostics.AddError("Error creating vault secret", err.Error())
		return
	}
	if apiResp.JSON201 == nil {
		resp.Diagnostics.AddError("Unexpected response creating vault secret",
			fmt.Sprintf("Create returned HTTP %d: %s", apiResp.StatusCode(), describeAPIError(apiResp.Body)))
		return
	}
	mapPolicy(apiResp.JSON201, &plan)
	plan.SecretValueHash = hashSecretValue(secret)

	// The create body carried bot_ids, but the authoritative linked set (empty
	// when allow_all_bots is true) is read back from the bot-links endpoint.
	if !r.readBotLinks(ctx, apiResp.JSON201.PolicyId, &plan, &resp.Diagnostics) {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *VaultSecretResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state VaultSecretResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	apiResp, err := r.data.client.GetSecretPolicyV1OrgsOrgIdSecretPoliciesPolicyIdGetWithResponse(
		ctx, r.data.orgID, state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Error reading vault secret", err.Error())
		return
	}
	if apiResp.StatusCode() == 404 {
		resp.State.RemoveResource(ctx)
		return
	}
	if apiResp.JSON200 == nil {
		resp.Diagnostics.AddError("Unexpected response reading vault secret",
			fmt.Sprintf("Read returned HTTP %d: %s", apiResp.StatusCode(), describeAPIError(apiResp.Body)))
		return
	}
	// mapPolicy preserves secret_value_hash (the remote never returns the value,
	// so the last-applied fingerprint in state is authoritative).
	mapPolicy(apiResp.JSON200, &state)
	if !r.readBotLinks(ctx, state.ID.ValueString(), &state, &resp.Diagnostics) {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *VaultSecretResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state VaultSecretResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)

	var secret types.String
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("secret_value"), &secret)...)
	if resp.Diagnostics.HasError() {
		return
	}

	newHash := hashSecretValue(secret)
	secretChanged := !newHash.Equal(state.SecretValueHash)

	body, diags := buildUpdateBody(plan, secret.ValueString(), secretChanged)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	apiResp, err := r.data.client.UpdateSecretPolicyV1OrgsOrgIdSecretPoliciesPolicyIdPatchWithBodyWithResponse(
		ctx, r.data.orgID, state.ID.ValueString(), "application/json", bytes.NewReader(body))
	if err != nil {
		resp.Diagnostics.AddError("Error updating vault secret", err.Error())
		return
	}
	if apiResp.JSON200 == nil {
		resp.Diagnostics.AddError("Unexpected response updating vault secret",
			fmt.Sprintf("Update returned HTTP %d: %s", apiResp.StatusCode(), describeAPIError(apiResp.Body)))
		return
	}
	mapPolicy(apiResp.JSON200, &plan)
	plan.SecretValueHash = newHash

	// Reconcile the authoritative bot-links set when it changed.
	if !botIDsEqual(ctx, plan.BotIDs, state.BotIDs, &resp.Diagnostics) {
		if resp.Diagnostics.HasError() {
			return
		}
		if !r.putBotLinks(ctx, state.ID.ValueString(), plan.BotIDs, &resp.Diagnostics) {
			return
		}
	}
	if !r.readBotLinks(ctx, state.ID.ValueString(), &plan, &resp.Diagnostics) {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *VaultSecretResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state VaultSecretResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	apiResp, err := r.data.client.DeleteSecretPolicyV1OrgsOrgIdSecretPoliciesPolicyIdDeleteWithResponse(
		ctx, r.data.orgID, state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Error deleting vault secret", err.Error())
		return
	}
	switch apiResp.StatusCode() {
	case 200, 202, 204, 404:
		// deleted or already gone (links cascade-delete with the policy)
	default:
		resp.Diagnostics.AddError("Unexpected response deleting vault secret",
			fmt.Sprintf("Delete returned HTTP %d: %s", apiResp.StatusCode(), describeAPIError(apiResp.Body)))
	}
}

// ImportState imports an existing secret policy by ID. The secret value cannot
// be imported (it is never returned by the API); the first apply after import
// re-pushes the configured secret_value.
func (r *VaultSecretResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// readBotLinks fetches the authoritative linked-bot set and writes it into the
// model. Returns false (with diagnostics appended) on error.
func (r *VaultSecretResource) readBotLinks(ctx context.Context, policyID string, m *VaultSecretResourceModel, diags *diag.Diagnostics) bool {
	apiResp, err := r.data.client.GetSecretPolicyBotLinksV1OrgsOrgIdSecretPoliciesPolicyIdBotLinksGetWithResponse(
		ctx, r.data.orgID, policyID)
	if err != nil {
		diags.AddError("Error reading vault secret bot links", err.Error())
		return false
	}
	if apiResp.JSON200 == nil {
		diags.AddError("Unexpected response reading vault secret bot links",
			fmt.Sprintf("Bot-links read returned HTTP %d: %s", apiResp.StatusCode(), describeAPIError(apiResp.Body)))
		return false
	}
	m.BotIDs = strSliceToSet(ctx, apiResp.JSON200.BotIds, diags)
	return !diags.HasError()
}

// putBotLinks replaces the full linked-bot set for the policy.
func (r *VaultSecretResource) putBotLinks(ctx context.Context, policyID string, set types.Set, diags *diag.Diagnostics) bool {
	ids := setToStrSlice(ctx, set, diags)
	if diags.HasError() {
		return false
	}
	if ids == nil {
		ids = []string{}
	}
	apiResp, err := r.data.client.PutSecretPolicyBotLinksV1OrgsOrgIdSecretPoliciesPolicyIdBotLinksPutWithResponse(
		ctx, r.data.orgID, policyID, client.SecretPolicyBotLinksRequest{BotIds: ids})
	if err != nil {
		diags.AddError("Error updating vault secret bot links", err.Error())
		return false
	}
	if apiResp.JSON200 == nil {
		diags.AddError("Unexpected response updating vault secret bot links",
			fmt.Sprintf("Bot-links update returned HTTP %d: %s", apiResp.StatusCode(), describeAPIError(apiResp.Body)))
		return false
	}
	return true
}

// buildCreateBody marshals the create request. The secret value is passed
// explicitly (read from configuration, not plan) to make it obvious the
// write-only value never flows through the state model.
func buildCreateBody(ctx context.Context, plan VaultSecretResourceModel, secretValue string) ([]byte, diag.Diagnostics) {
	var diags diag.Diagnostics
	body := client.SecretPolicyCreateRequest{
		KeyPath:       plan.KeyPath.ValueString(),
		DisplayName:   plan.DisplayName.ValueString(),
		Description:   strToPtr(plan.Description),
		Value:         &secretValue,
		Sensitivity:   sensitivityPtr(plan.Sensitivity),
		AllowAllBots:  plan.AllowAllBots.ValueBool(),
		MaxTtlSeconds: int(plan.MaxTTLSeconds.ValueInt64()),
		BotIds:        setToStrSlicePtr(ctx, plan.BotIDs, &diags),
	}
	out, err := json.Marshal(body)
	if err != nil {
		diags.AddError("Error encoding vault secret", err.Error())
	}
	return out, diags
}

// buildUpdateBody builds a sparse PATCH body as map[string]json.RawMessage. The
// metadata fields are always sent (reconciled to the desired plan values); the
// write-only value is included only when its hash changed, so an unchanged
// secret is never needlessly re-encrypted/rotated.
func buildUpdateBody(plan VaultSecretResourceModel, secretValue string, secretChanged bool) ([]byte, diag.Diagnostics) {
	var diags diag.Diagnostics
	body := map[string]json.RawMessage{
		"display_name":    rawString(plan.DisplayName),
		"description":     rawString(plan.Description),
		"sensitivity":     rawString(plan.Sensitivity),
		"allow_all_bots":  rawBool(plan.AllowAllBots),
		"max_ttl_seconds": rawInt64(plan.MaxTTLSeconds),
	}
	if secretChanged {
		b, err := json.Marshal(secretValue)
		if err != nil {
			diags.AddError("Error encoding vault secret value", err.Error())
		} else {
			body["value"] = b
		}
	}
	out, err := json.Marshal(body)
	if err != nil {
		diags.AddError("Error encoding vault secret update", err.Error())
	}
	return out, diags
}

// mapPolicy writes an API policy response into the model. It deliberately does
// not touch SecretValue, SecretValueHash, or BotIDs: the secret is write-only,
// its hash is owned by ModifyPlan/Create/Update, and the linked-bot set is read
// from the bot-links endpoint.
func mapPolicy(p *client.SecretPolicyResponse, m *VaultSecretResourceModel) {
	m.ID = types.StringValue(p.PolicyId)
	m.KeyPath = types.StringValue(p.KeyPath)
	m.DisplayName = types.StringValue(p.DisplayName)
	m.Description = strPtrToStr(p.Description)
	m.Sensitivity = types.StringValue(string(p.Sensitivity))
	m.AllowAllBots = types.BoolValue(p.AllowAllBots)
	m.MaxTTLSeconds = types.Int64Value(int64(p.MaxTtlSeconds))
	m.LinkedBotCount = types.Int64Value(int64(p.LinkedBotCount))
	m.CreatedAt = types.StringValue(p.CreatedAt.Format(time.RFC3339))
	m.UpdatedAt = types.StringValue(p.UpdatedAt.Format(time.RFC3339))
}

// --- helpers ---

// hashSecretValue returns the hex SHA-256 of a write-only value, propagating
// null/unknown so plan-time interpolation stays "known after apply".
func hashSecretValue(v types.String) types.String {
	if v.IsUnknown() {
		return types.StringUnknown()
	}
	if v.IsNull() {
		return types.StringNull()
	}
	sum := sha256.Sum256([]byte(v.ValueString()))
	return types.StringValue(hex.EncodeToString(sum[:]))
}

func sensitivityPtr(s types.String) *client.RuntimeVaultSensitivity {
	if s.IsNull() || s.IsUnknown() {
		return nil
	}
	v := client.RuntimeVaultSensitivity(s.ValueString())
	return &v
}

func setToStrSlice(ctx context.Context, s types.Set, diags *diag.Diagnostics) []string {
	if s.IsNull() || s.IsUnknown() {
		return nil
	}
	out := make([]string, 0, len(s.Elements()))
	diags.Append(s.ElementsAs(ctx, &out, false)...)
	return out
}

func setToStrSlicePtr(ctx context.Context, s types.Set, diags *diag.Diagnostics) *[]string {
	out := setToStrSlice(ctx, s, diags)
	if out == nil {
		return nil
	}
	return &out
}

func strSliceToSet(ctx context.Context, ids []string, diags *diag.Diagnostics) types.Set {
	v, d := types.SetValueFrom(ctx, types.StringType, ids)
	diags.Append(d...)
	return v
}

// botIDsEqual reports whether two bot-id sets are order-independently equal. An
// unknown planned set (e.g. UseStateForUnknown not yet resolved) is treated as
// equal so it does not force a spurious bot-links write.
func botIDsEqual(ctx context.Context, a, b types.Set, diags *diag.Diagnostics) bool {
	if a.IsUnknown() {
		return true
	}
	as := setToStrSlice(ctx, a, diags)
	bs := setToStrSlice(ctx, b, diags)
	if len(as) != len(bs) {
		return false
	}
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

func rawBool(b types.Bool) json.RawMessage {
	if b.IsNull() || b.IsUnknown() {
		return json.RawMessage("null")
	}
	out, err := json.Marshal(b.ValueBool())
	if err != nil {
		return json.RawMessage("null")
	}
	return out
}
