package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

var (
	_ resource.Resource                = (*BotResource)(nil)
	_ resource.ResourceWithConfigure   = (*BotResource)(nil)
	_ resource.ResourceWithImportState = (*BotResource)(nil)
)

// BotResource manages a Botyard bot's desired-state record.
//
// Phase A covers the bot's lifecycle and core identity: create (POST /bots),
// read, metadata update (description/avatar_url via PATCH), delete, and import.
// Phase B adds the bot's OpenClaw config as an optional nested `config` block,
// applied via the create POST and PATCH /config (see bot_config.go). The bot's
// skill/tool/credential assignments remain managed by later phases / dedicated
// resources, not here.
type BotResource struct {
	data *providerData
}

// BotResourceModel maps the botyard_bot resource schema.
//
// Deliberately omitted per the epic-59 decision (task #825): `tier` (deprecated
// platform-wide) and `cluster_id` (internal provisioner placement). Also omitted
// from this resource: files/skills/tools/resources/access — those belong to
// later phases. The nested `config` block is added in Phase B (see botConfigModel).
type BotResourceModel struct {
	// Writable identity.
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
	AvatarURL   types.String `tfsdk:"avatar_url"`

	// OpenClaw config overrides (Phase B). Nil when the practitioner does not
	// declare a `config` block; managed via the create POST and PATCH /config.
	Config *botConfigModel `tfsdk:"config"`

	// Server-owned identity (computed).
	ID        types.String `tfsdk:"id"`
	Slug      types.String `tfsdk:"slug"`
	OrgID     types.String `tfsdk:"org_id"`
	Namespace types.String `tfsdk:"namespace"`

	// Runtime placement (computed — not settable via create or the metadata
	// PATCH). `runtime_privilege_mode` is mutable via a dedicated PATCH but is
	// surfaced read-only in Phase A; making it writable is a planned follow-up.
	RuntimeClass         types.String `tfsdk:"runtime_class"`
	StorageClass         types.String `tfsdk:"storage_class"`
	RuntimePrivilegeMode types.String `tfsdk:"runtime_privilege_mode"`
	DurableRootOwnsHome  types.Bool   `tfsdk:"durable_root_owns_home"`

	// Control-plane telemetry (computed).
	Access           types.String `tfsdk:"access"`
	DesiredState     types.String `tfsdk:"desired_state"`
	HealthStatus     types.String `tfsdk:"health_status"`
	OnboardingState  types.String `tfsdk:"onboarding_state"`
	ConfigGeneration types.Int64  `tfsdk:"config_generation"`
	CreatedAt        types.String `tfsdk:"created_at"`
	UpdatedAt        types.String `tfsdk:"updated_at"`
}

// NewBotResource is the resource factory registered with the provider.
func NewBotResource() resource.Resource {
	return &BotResource{}
}

// Metadata sets the resource type name.
func (r *BotResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_bot"
}

// Schema defines the botyard_bot resource schema.
func (r *BotResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	// stableComputedString marks an immutable, server-owned string that never
	// changes after create — UseStateForUnknown keeps no-op plans clean.
	stableComputedString := func(desc string) schema.StringAttribute {
		return schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: desc,
			PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
		}
	}
	// liveComputedString marks a server-owned string that can change over the
	// bot's life (telemetry) — it must refresh on every read, so no
	// UseStateForUnknown.
	liveComputedString := func(desc string) schema.StringAttribute {
		return schema.StringAttribute{Computed: true, MarkdownDescription: desc}
	}

	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a Botyard bot's desired-state record within the configured organization. " +
			"Creating the resource persists the bot and triggers provisioner reconciliation best-effort. " +
			"It manages the bot's core identity (name, description, avatar) and its OpenClaw `config` " +
			"overrides, and exposes runtime placement and control-plane state as read-only attributes; " +
			"the bot's skill/tool/credential assignments are managed separately.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Bot display name (1–255 chars). Immutable — changing it forces replacement, " +
					"as the API derives the bot's slug from the name at creation and does not support renaming.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"description": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Short human-facing description / role for the bot (max 500 chars). Display " +
					"metadata only — not injected into the bot's system prompt. Omit to leave unset.",
			},
			"avatar_url": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Avatar image URL (a DiceBear data URI or a custom URL). Optional — the API " +
					"stores no avatar when omitted. Removing it from the config clears the stored value (sends JSON null).",
			},

			"config": botConfigSchemaAttribute(),

			"id":        stableComputedString("Unique bot identifier (UUID)."),
			"slug":      stableComputedString("URL-friendly bot identifier, derived from the name at creation. Used as the import ID."),
			"org_id":    stableComputedString("Organization ID that owns the bot."),
			"namespace": stableComputedString("Kubernetes namespace for the bot runtime."),

			"runtime_class":          stableComputedString("Container runtime class for the bot pod (e.g. `kata_qemu`, `runc`, or a cluster default). Set by the platform; not user-configurable."),
			"storage_class":          stableComputedString("Storage class for the bot's workspace PVC. Set by the platform; not user-configurable."),
			"runtime_privilege_mode": stableComputedString("Privilege level of the bot runtime pod. Read-only in this provider version."),
			"durable_root_owns_home": schema.BoolAttribute{
				Computed:            true,
				MarkdownDescription: "Operator rollout flag for the single-block durable root layout. When true, `/home/openclaw` lives inside the durable root overlay.",
				PlanModifiers:       []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},

			"access":            liveComputedString("Bot visibility within its organization (`open` or `restricted`)."),
			"desired_state":     liveComputedString("Control-plane desired lifecycle state."),
			"health_status":     liveComputedString("Bot health monitoring status."),
			"onboarding_state":  liveComputedString("Guided-setup lifecycle state of the bot."),
			"config_generation": schema.Int64Attribute{Computed: true, MarkdownDescription: "Monotonic config generation counter."},
			"created_at":        stableComputedString("Creation timestamp (RFC 3339)."),
			"updated_at":        liveComputedString("Last-update timestamp (RFC 3339)."),
		},
	}
}

// Configure receives the shared provider data.
func (r *BotResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *BotResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan BotResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body, diags := buildBotCreateBody(plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	apiResp, err := r.data.client.CreateBotV1OrgsOrgIdBotsPostWithBodyWithResponse(
		ctx, r.data.orgID, "application/json", bytes.NewReader(body))
	if err != nil {
		resp.Diagnostics.AddError("Error creating bot", err.Error())
		return
	}
	if apiResp.JSON201 == nil {
		resp.Diagnostics.AddError("Unexpected response creating bot",
			fmt.Sprintf("Create returned HTTP %d: %s", apiResp.StatusCode(), describeAPIError(apiResp.Body)))
		return
	}
	mapBotResource(apiResp.JSON201, &plan)
	// The create POST embeds the config, so the 201 already reflects the merged
	// desired_config — refresh the declared config leaves from it.
	mapBotConfig(&apiResp.JSON201.DesiredConfig, plan.Config)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *BotResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state BotResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	apiResp, err := r.data.client.GetBotV1OrgsOrgIdBotsBotSlugGetWithResponse(ctx, r.data.orgID, state.Slug.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Error reading bot", err.Error())
		return
	}
	switch botReadDisposition(apiResp.StatusCode(), apiResp.JSON200) {
	case botReadGone:
		// A bot deletion is a soft-delete (desired_state=deleted) that the
		// reconciler later purges — treat both "gone" (404) and "tombstoned"
		// (still readable but desired_state=deleted) as removed from state.
		resp.State.RemoveResource(ctx)
	case botReadUnexpected:
		resp.Diagnostics.AddError("Unexpected response reading bot",
			fmt.Sprintf("Read returned HTTP %d: %s", apiResp.StatusCode(), describeAPIError(apiResp.Body)))
	case botReadOK:
		mapBotResource(apiResp.JSON200, &state)
		mapBotConfig(&apiResp.JSON200.DesiredConfig, state.Config)
		resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
	}
}

func (r *BotResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state BotResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body, err := buildBotUpdateBody(plan)
	if err != nil {
		resp.Diagnostics.AddError("Error encoding bot update", err.Error())
		return
	}

	apiResp, err := r.data.client.UpdateBotV1OrgsOrgIdBotsBotSlugPatchWithBodyWithResponse(
		ctx, r.data.orgID, state.Slug.ValueString(), "application/json", bytes.NewReader(body))
	if err != nil {
		resp.Diagnostics.AddError("Error updating bot", err.Error())
		return
	}
	if apiResp.JSON200 == nil {
		resp.Diagnostics.AddError("Unexpected response updating bot",
			fmt.Sprintf("Update returned HTTP %d: %s", apiResp.StatusCode(), describeAPIError(apiResp.Body)))
		return
	}
	// The bot's config lives behind a dedicated PATCH /config endpoint (BotUpdate
	// carries no config), so apply it separately when a config block is declared.
	// The config PATCH returns the full, freshly-merged bot, so prefer its
	// response for the final state mapping.
	final := apiResp.JSON200
	if plan.Config != nil {
		cfgResp, ok := r.updateBotConfig(ctx, state.Slug.ValueString(), plan.Config, &resp.Diagnostics)
		if !ok {
			return
		}
		final = cfgResp
	}

	mapBotResource(final, &plan)
	mapBotConfig(&final.DesiredConfig, plan.Config)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// updateBotConfig applies the declared config via PATCH /config. The body wraps
// the sparse OpenClawConfigPatch object in a BotConfigUpdate ({"config": {...}}).
// Returns the merged bot on success; on failure it records a diagnostic and
// returns ok=false so the caller aborts without setting state.
func (r *BotResource) updateBotConfig(ctx context.Context, slug string, cfg *botConfigModel, diags *diag.Diagnostics) (*client.BotResponse, bool) {
	body, err := json.Marshal(map[string]json.RawMessage{"config": buildBotConfigPatch(cfg)})
	if err != nil {
		diags.AddError("Error encoding bot config", err.Error())
		return nil, false
	}
	cfgResp, err := r.data.client.UpdateBotConfigV1OrgsOrgIdBotsBotSlugConfigPatchWithBodyWithResponse(
		ctx, r.data.orgID, slug, "application/json", bytes.NewReader(body))
	if err != nil {
		diags.AddError("Error updating bot config", err.Error())
		return nil, false
	}
	if cfgResp.JSON200 == nil {
		diags.AddError("Unexpected response updating bot config",
			fmt.Sprintf("Config update returned HTTP %d: %s", cfgResp.StatusCode(), describeAPIError(cfgResp.Body)))
		return nil, false
	}
	return cfgResp.JSON200, true
}

func (r *BotResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state BotResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	apiResp, err := r.data.client.DeleteBotV1OrgsOrgIdBotsBotSlugDeleteWithResponse(ctx, r.data.orgID, state.Slug.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Error deleting bot", err.Error())
		return
	}
	if !botDeleteStatusAccepted(apiResp.StatusCode()) {
		resp.Diagnostics.AddError("Unexpected response deleting bot",
			fmt.Sprintf("Delete returned HTTP %d: %s", apiResp.StatusCode(), describeAPIError(apiResp.Body)))
	}
}

// ImportState imports an existing bot by its slug (the API's URL identifier).
func (r *BotResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("slug"), req, resp)
}

// buildBotCreateBody builds the POST /bots body as a sparse
// map[string]json.RawMessage. `config` is required by the API but its fields are
// all optional; buildBotConfigPatch renders the declared config block as a
// sparse OpenClawConfigPatch object, or `{}` when no config block is declared —
// the API merges it over OpenClaw defaults, so an empty object nulls nothing.
// Building the body by hand (rather than via the generated BotCreate struct)
// avoids a zero-value OpenClawConfigPatch marshaling its fields as explicit
// nulls, and keeps the wire body to exactly the fields the strict BotCreate
// model accepts (it rejects unknown keys).
func buildBotCreateBody(plan BotResourceModel) ([]byte, diag.Diagnostics) {
	var diags diag.Diagnostics
	body := map[string]json.RawMessage{
		"name":        rawString(plan.Name),
		"description": rawString(plan.Description),
		"avatar_url":  rawString(plan.AvatarURL),
		"config":      buildBotConfigPatch(plan.Config),
	}
	out, err := json.Marshal(body)
	if err != nil {
		diags.AddError("Error encoding bot", err.Error())
	}
	return out, diags
}

// buildBotUpdateBody builds the PATCH /bots/{slug} body as a sparse
// map[string]json.RawMessage limited to the Phase A metadata fields the API
// accepts on update. The bot's config is updated via a separate endpoint, and
// tier/resources/runtime_privilege_mode are out of Phase A scope. Terraform
// config is the source of truth, so each managed field is sent every update
// (a value sets it; JSON null clears it).
func buildBotUpdateBody(plan BotResourceModel) ([]byte, error) {
	body := map[string]json.RawMessage{
		"description": rawString(plan.Description),
		"avatar_url":  rawString(plan.AvatarURL),
	}
	return json.Marshal(body)
}

// botReadResult classifies how Read should react to a GET /bots/{slug} response.
type botReadResult int

const (
	// botReadOK — the bot exists and is live; map it into state.
	botReadOK botReadResult = iota
	// botReadGone — the bot is absent (404) or soft-deleted
	// (desired_state=deleted); remove it from state.
	botReadGone
	// botReadUnexpected — a non-404 response with no parseable body; surface an
	// error.
	botReadUnexpected
)

// botReadDisposition is the pure, unit-testable Read decision. Delete is a
// soft-delete that leaves a readable tombstone until the reconciler purges the
// row, so a 200 with desired_state=deleted is treated the same as a 404.
func botReadDisposition(status int, b *client.BotResponse) botReadResult {
	if status == 404 {
		return botReadGone
	}
	if b == nil {
		return botReadUnexpected
	}
	if b.DesiredState == client.DesiredStateDeleted {
		return botReadGone
	}
	return botReadOK
}

// botDeleteStatusAccepted reports whether a DELETE /bots/{slug} status means the
// bot is gone or on its way out (soft-deleted, accepted, no-content, or already
// absent). Any other status is an error.
func botDeleteStatusAccepted(code int) bool {
	switch code {
	case 200, 202, 204, 404:
		return true
	default:
		return false
	}
}

// mapBotResource writes an API BotResponse into the resource model. It never
// touches `tier` or `cluster_id` (deliberately not modeled) and reads back
// `description`/`avatar_url` so server-applied defaults land in state.
func mapBotResource(b *client.BotResponse, m *BotResourceModel) {
	m.Name = types.StringValue(b.Name)
	m.Description = strPtrToStr(b.Description)
	m.AvatarURL = strPtrToStr(b.AvatarUrl)

	m.ID = types.StringValue(b.Id)
	m.Slug = types.StringValue(b.Slug)
	m.OrgID = types.StringValue(b.OrgId)
	m.Namespace = types.StringValue(b.Namespace)

	m.RuntimeClass = types.StringValue(string(b.RuntimeClass))
	m.StorageClass = types.StringValue(string(b.StorageClass))
	m.RuntimePrivilegeMode = types.StringValue(string(b.RuntimePrivilegeMode))
	m.DurableRootOwnsHome = types.BoolValue(b.DurableRootOwnsHome)

	m.Access = types.StringValue(string(b.Access))
	m.DesiredState = types.StringValue(string(b.DesiredState))
	m.HealthStatus = types.StringValue(string(b.HealthStatus))
	m.OnboardingState = types.StringValue(string(b.OnboardingState))
	m.ConfigGeneration = types.Int64Value(int64(b.ConfigGeneration))
	m.CreatedAt = types.StringValue(b.CreatedAt.Format(time.RFC3339))
	m.UpdatedAt = types.StringValue(b.UpdatedAt.Format(time.RFC3339))
}
