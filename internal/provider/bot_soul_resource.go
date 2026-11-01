package provider

import (
	"context"
	"fmt"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

var (
	_ resource.Resource                = (*BotSoulResource)(nil)
	_ resource.ResourceWithConfigure   = (*BotSoulResource)(nil)
	_ resource.ResourceWithImportState = (*BotSoulResource)(nil)
)

// BotSoulResource manages the native bot's singleton, platform-owned soul.
// It deliberately does not own the bot or erase instructions on destruction.
type BotSoulResource struct{ data *providerData }

type BotSoulResourceModel struct {
	ID      types.String `tfsdk:"id"`
	BotSlug types.String `tfsdk:"bot_slug"`
	Content types.String `tfsdk:"content"`
	Version types.Int64  `tfsdk:"version"`
}

func NewBotSoulResource() resource.Resource { return &BotSoulResource{} }

func (r *BotSoulResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_bot_soul"
}

func (r *BotSoulResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages the complete soul (instructions) of a hosted native bot (`hosting_type = \"hosted\"`, " +
			"`harness = \"botyard_native\"`) through the platform's bot-files API, not `native_config`. " +
			"Use at most one `botyard_bot_soul` per bot. Creating this resource adopts and, if different, replaces the existing soul. " +
			"Edits made outside Terraform are detected on refresh and overwritten on the next apply. " +
			"Pack soul changes are also outside edits: this resource owns the **whole** soul, including any pack text.\n\n" +
			"**Removal is non-destructive:** destroying this resource or changing `bot_slug` leaves the previous bot's soul and revision history intact. " +
			"To clear the soul explicitly, apply `content = \"\"` before removing the resource. " +
			"Writes create a revision only when content differs and take effect for subsequent native turns. " +
			"This is a separate resource applied after bot creation; the bot initially has its platform/pack soul.\n\n" +
			"Requires API key permissions `bot:file.read`, `bot:file.update`, and permission to read the bot. " +
			"Content is stored in Terraform state and may appear in plans; do not put secrets in it.",
		Attributes: map[string]schema.Attribute{
			"id":       schema.StringAttribute{Computed: true, MarkdownDescription: "The bot slug.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"bot_slug": schema.StringAttribute{Required: true, MarkdownDescription: "Slug of the hosted native bot. Changing it forces replacement; the old soul is preserved.", Validators: []validator.String{stringvalidator.LengthAtLeast(1)}, PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			"content":  schema.StringAttribute{Required: true, MarkdownDescription: "Complete soul markdown, for example `file(\"${path.module}/SOUL.md\")`. Maximum 102400 characters. An empty string explicitly clears the soul. Never include secrets.", Validators: []validator.String{stringvalidator.LengthAtMost(102400)}},
			"version":  schema.Int64Attribute{Computed: true, MarkdownDescription: "Current platform soul revision number."},
		},
	}
}

func (r *BotSoulResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	data, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError("Unexpected Resource Configure Type", fmt.Sprintf("Expected *providerData, got %T.", req.ProviderData))
		return
	}
	r.data = data
}

func (r *BotSoulResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan BotSoulResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.apply(ctx, &plan, &resp.Diagnostics)
	if !resp.Diagnostics.HasError() {
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	}
}

func (r *BotSoulResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan BotSoulResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.apply(ctx, &plan, &resp.Diagnostics)
	if !resp.Diagnostics.HasError() {
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	}
}

func (r *BotSoulResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state BotSoulResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !r.nativeBot(ctx, state.BotSlug.ValueString(), true, &resp.Diagnostics) {
		if !resp.Diagnostics.HasError() {
			resp.State.RemoveResource(ctx)
		}
		return
	}
	soul, missing := r.readSoul(ctx, state.BotSlug.ValueString(), &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if missing || soul == nil {
		resp.State.RemoveResource(ctx)
		return
	}
	state.ID = state.BotSlug
	state.Content = types.StringValue(soul.Content)
	state.Version = types.Int64Value(int64(soul.Version))
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *BotSoulResource) Delete(_ context.Context, _ resource.DeleteRequest, _ *resource.DeleteResponse) {
	// Stop managing, without deleting the bot's instructions or history.
}

func (r *BotSoulResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if req.ID == "" {
		resp.Diagnostics.AddError("Invalid import ID", "Import using the native bot's slug.")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("bot_slug"), req.ID)...)
}

// nativeBot fails closed on unsupported harnesses or failed reads. A 404 is
// removable state only during refresh, never a successful create/update.
func (r *BotSoulResource) nativeBot(ctx context.Context, slug string, allowMissing bool, diags *diag.Diagnostics) bool {
	bot, err := r.data.client.GetBotV1OrgsOrgIdBotsBotSlugGetWithResponse(ctx, r.data.orgID, slug)
	if err != nil {
		diags.AddError("Error reading soul's bot", err.Error())
		return false
	}
	if allowMissing && bot.StatusCode() == http.StatusNotFound {
		return false
	}
	if bot.JSON200 == nil {
		soulAPIError(diags, "reading the bot", bot.StatusCode())
		return false
	}
	if bot.JSON200.Harness != client.BotHarnessBotyardNative || bot.JSON200.HostingType != client.BotHostingTypeHosted {
		diags.AddAttributeError(path.Root("bot_slug"), "Unsupported bot runtime", "botyard_bot_soul currently supports only hosted bots with harness = \"botyard_native\".")
		return false
	}
	return true
}

func (r *BotSoulResource) readSoul(ctx context.Context, slug string, diags *diag.Diagnostics) (*client.BotFileResponse, bool) {
	resp, err := r.data.client.ListBotFilesV1OrgsOrgIdBotsBotSlugFilesGetWithResponse(ctx, r.data.orgID, slug)
	if err != nil {
		diags.AddError("Error reading bot soul", err.Error())
		return nil, false
	}
	if resp.StatusCode() == http.StatusNotFound {
		return nil, true
	}
	if resp.JSON200 == nil {
		soulAPIError(diags, "reading soul files", resp.StatusCode())
		return nil, false
	}
	return findSoul(resp.JSON200.Files, diags), false
}

func findSoul(files []client.BotFileResponse, diags *diag.Diagnostics) *client.BotFileResponse {
	var soul *client.BotFileResponse
	for i := range files {
		if files[i].FileType != client.BotFileTypeSoul {
			continue
		}
		if soul != nil {
			diags.AddError("Invalid soul response", "The API returned multiple current soul revisions.")
			return nil
		}
		soul = &files[i]
	}
	return soul
}

func (r *BotSoulResource) apply(ctx context.Context, plan *BotSoulResourceModel, diags *diag.Diagnostics) {
	slug := plan.BotSlug.ValueString()
	if !r.nativeBot(ctx, slug, false, diags) {
		return
	}
	current, missing := r.readSoul(ctx, slug, diags)
	if diags.HasError() {
		return
	}
	if missing {
		soulAPIError(diags, "reading soul files", http.StatusNotFound)
		return
	}
	if current == nil || current.Content != plan.Content.ValueString() {
		resp, err := r.data.client.UpdateBotFilesV1OrgsOrgIdBotsBotSlugFilesPutWithResponse(ctx, r.data.orgID, slug,
			client.BotFileUpdateRequest{Files: []client.BotFileEntry{{FileType: client.BotFileTypeSoul, Content: plan.Content.ValueString()}}})
		if err != nil {
			diags.AddError("Error writing bot soul", err.Error())
			return
		}
		if resp.JSON201 == nil {
			soulAPIError(diags, "writing the soul", resp.StatusCode())
			return
		}
		current = findSoul(resp.JSON201.Files, diags)
		if diags.HasError() {
			return
		}
		if current == nil || current.Content != plan.Content.ValueString() {
			diags.AddError("Invalid soul write response", "The API did not return the requested soul content. Refresh and retry; the write may have committed.")
			return
		}
	}
	plan.ID = plan.BotSlug
	plan.Version = types.Int64Value(int64(current.Version))
}

// Do not echo error response bodies: validation errors can contain the soul.
func soulAPIError(diags *diag.Diagnostics, operation string, status int) {
	diags.AddError("Error managing bot soul", fmt.Sprintf("API returned HTTP %d while %s. Check the bot slug and API key permissions (bot read, bot:file.read, bot:file.update). Response body omitted to protect soul content.", status, operation))
}
