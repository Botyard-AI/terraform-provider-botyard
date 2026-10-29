package provider

// The `pack` block on botyard_bot (PlatDev #4383, packs P2).
//
// Kept in its own file so the parallel v0.7.0 bot track, which also edits
// bot_resource.go, rebases against a handful of call sites rather than this
// whole feature.
//
// Lifecycle, in API terms:
//   - Create with `pack` set: POST /bots/from-pack (instead of POST /bots),
//     then the sparse PATCH /config when a config block is declared.
//   - Read and import: GET /packs/installs?bot_ids=<id>.
//   - Removing the block: DELETE /packs/installs, the bot is kept.
//   - Destroying the bot: DELETE /bots/{slug} as before; no uninstall first.
//
// A bot is never replaced on account of its pack: recreating a bot loses its
// conversations and memory. Every change the API cannot make in place is a
// plan-time error instead (see modifyPackPlan).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

// Connection preview setup kinds that `create` cannot satisfy.
const (
	connectionSetupOAuth       = "oauth"
	connectionSetupUnavailable = "unavailable"
)

// botPackModel is the `pack` block.
//
// settings and connections are create-time inputs: the API records the
// result of a choice, not the choice, so neither can be read back. Changing
// them on an existing install is a warning, not a diff that would never
// converge (see modifyPackPlan).
type botPackModel struct {
	PackVersionID types.String             `tfsdk:"pack_version_id"`
	Settings      []botPackSettingModel    `tfsdk:"settings"`
	Connections   []botPackConnectionModel `tfsdk:"connections"`

	BotPackInstallID   types.String `tfsdk:"bot_pack_install_id"`
	PackID             types.String `tfsdk:"pack_id"`
	PackSlug           types.String `tfsdk:"pack_slug"`
	VersionNo          types.Int64  `tfsdk:"version_no"`
	PendingConnections types.List   `tfsdk:"pending_connections"`
}

var pendingConnectionAttrTypes = map[string]attr.Type{
	"pack_version_connection_id":   types.StringType,
	"bot_pack_connection_setup_id": types.StringType,
	"state":                        types.StringType,
}

var pendingConnectionObjectType = types.ObjectType{AttrTypes: pendingConnectionAttrTypes}

func botPackSchemaAttribute() schema.SingleNestedAttribute {
	stable := func(desc string) schema.StringAttribute {
		return schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: desc,
			PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
		}
	}
	return schema.SingleNestedAttribute{
		Optional: true,
		MarkdownDescription: "A Bot Pack to create this bot from. When set, the bot is created with " +
			"`POST /bots/from-pack`, which installs the pack in the same transaction. Removing the block " +
			"uninstalls the pack and keeps the bot. Adding it to an existing bot, or changing " +
			"`pack_version_id`, is refused at plan time: the API has no install-on-existing-bot or upgrade " +
			"path yet, and the provider never replaces a bot (that would lose its conversations and memory). " +
			"`avatar_url` left unset means the pack's avatar is kept.",
		Attributes: map[string]schema.Attribute{
			"pack_version_id": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The pack version to install, e.g. " +
					"`data.botyard_pack.support.latest.pack_version_id`. Changing it is a plan-time error " +
					"until the API offers in-place upgrades.",
			},
			"settings":    packSettingsSchemaAttribute(),
			"connections": packConnectionsSchemaAttribute(),

			"bot_pack_install_id": stable("The install's id."),
			"pack_id":             stable("The installed pack's id."),
			"pack_slug":           stable("The installed pack's slug."),
			"version_no": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "The installed version's number.",
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"pending_connections": schema.ListNestedAttribute{
				Computed: true,
				MarkdownDescription: "Connections whose setup is not done, in connection order. Finish them in " +
					"the app; they refresh on every read.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"pack_version_connection_id":   schema.StringAttribute{Computed: true, MarkdownDescription: "The connection."},
					"bot_pack_connection_setup_id": schema.StringAttribute{Computed: true, MarkdownDescription: "Its setup."},
					"state":                        schema.StringAttribute{Computed: true, MarkdownDescription: "Where the setup is, e.g. `needs_input`."},
				}},
			},
		},
	}
}

// modifyPackPlan holds the plan-time guards that need prior state or the API.
//
// state is nil on create. Every refusal names what to do instead, because the
// alternative the framework offers, RequiresReplace, would destroy the bot.
func (r *BotResource) modifyPackPlan(ctx context.Context, plan, state *BotResourceModel, diags *diag.Diagnostics) {
	if plan == nil || plan.Pack == nil {
		return
	}
	root := path.Root("pack")
	if state != nil {
		switch {
		case state.Pack == nil:
			diags.AddAttributeError(root, "Cannot add a pack to an existing bot",
				"This bot exists without a pack install that Terraform manages. The Botyard API cannot yet "+
					"install a pack onto an existing bot (`pack_install_existing_bot_disabled`), and the "+
					"provider will not replace the bot to do it, since that would lose its conversations "+
					"and memory. Remove the `pack` block, or create a new `botyard_bot` with it. If the pack "+
					"was uninstalled outside Terraform, remove the block to match.")
		case !plan.Pack.PackVersionID.IsUnknown() && !state.Pack.PackVersionID.IsNull() &&
			plan.Pack.PackVersionID.ValueString() != state.Pack.PackVersionID.ValueString():
			diags.AddAttributeError(root.AtName("pack_version_id"), "Cannot change an installed pack version",
				fmt.Sprintf("This bot runs pack version %s. The Botyard API has no in-place upgrade yet, and "+
					"the provider will not replace the bot, since that would lose its conversations and "+
					"memory. Keep `pack_version_id` at the installed version for now.",
					state.Pack.PackVersionID.ValueString()))
		default:
			if !packChoicesEqual(plan.Pack, state.Pack) {
				diags.AddAttributeWarning(root, "Pack choices only apply at creation",
					"`pack.settings` and `pack.connections` are applied when the bot is created from the pack. "+
						"Changing them on an existing bot has no effect on the install; change the bot's "+
						"settings, or bind connections, in the app.")
			}
		}
		return
	}
	r.checkCreateChoices(ctx, plan.Pack, diags)
}

// checkCreateChoices refuses, at plan time, the `create` connection choices
// that from-pack would refuse mid-apply. Mirrors the API's
// `oauth_create_not_supported` (Jakob, J-C card 3/3: refuse_plan_time).
func (r *BotResource) checkCreateChoices(ctx context.Context, p *botPackModel, diags *diag.Diagnostics) {
	var creates []int
	for i, c := range p.Connections {
		if !c.Choice.IsUnknown() && c.Choice.ValueString() == packConnChoiceCreate && c.Create != nil {
			creates = append(creates, i)
		}
	}
	if len(creates) == 0 || p.PackVersionID.IsUnknown() || r.data == nil {
		return
	}
	previews, ok := r.previewConnections(ctx, p.PackVersionID.ValueString(), diags)
	if !ok {
		return
	}
	for _, i := range creates {
		c := p.Connections[i]
		at := path.Root("pack").AtName("connections").AtListIndex(i)
		if c.PackVersionConnectionID.IsUnknown() {
			continue
		}
		id := c.PackVersionConnectionID.ValueString()
		pv, found := previews[id]
		if !found {
			diags.AddAttributeError(at.AtName("pack_version_connection_id"), "Unknown pack connection",
				fmt.Sprintf("Pack version %s has no MCP connection %q that `create` can connect.", p.PackVersionID.ValueString(), id))
			continue
		}
		diags.Append(createChoiceDiagnostics(at, c, pv)...)
	}
}

// connectionPreview is what the plan-time `create` check needs from a
// PackMcpConnectionPreview.
type connectionPreview struct {
	setupKind string
	metered   bool
}

// createChoiceDiagnostics is the pure decision behind checkCreateChoices.
func createChoiceDiagnostics(at path.Path, c botPackConnectionModel, pv connectionPreview) diag.Diagnostics {
	var diags diag.Diagnostics
	switch pv.setupKind {
	case connectionSetupOAuth:
		diags.AddAttributeError(at.AtName("choice"), "Connection needs a vendor sign-in",
			"This connection signs in at the vendor (OAuth), which Terraform cannot do "+
				"(`oauth_create_not_supported`). Connect the account in the Botyard app under MCP servers "+
				"first, then set `choice = \"use_server\"` with its `mcp_server_id`, or `choice = \"leave_pending\"`.")
		return diags
	case connectionSetupUnavailable:
		diags.AddAttributeError(at.AtName("choice"), "Connection cannot be created",
			"The API reports no way to connect a new account for this connection in this org. Use "+
				"`choice = \"use_server\"` with an existing server, or `choice = \"leave_pending\"`.")
		return diags
	}
	if declared := c.Create.Setup.Kind; !declared.IsUnknown() && declared.ValueString() != pv.setupKind {
		diags.AddAttributeError(at.AtName("create").AtName("setup").AtName("kind"), "Wrong setup kind",
			fmt.Sprintf("This connection's setup kind is %q, but `create.setup.kind` is %q.", pv.setupKind, declared.ValueString()))
	}
	if pv.metered && !c.Create.AcceptMetered.IsUnknown() && !c.Create.AcceptMetered.ValueBool() {
		diags.AddAttributeError(at.AtName("create").AtName("accept_metered"), "Metered connection",
			"A new server for this connection is billed as a hosted MCP server. Set `accept_metered = true` to accept that.")
	}
	return diags
}

// previewConnections reads POST /packs/connections/preview for one pack
// version, keyed by MCP connection id. Vault connections take no choice and
// are left out.
func (r *BotResource) previewConnections(ctx context.Context, pvID string, diags *diag.Diagnostics) (map[string]connectionPreview, bool) {
	resp, err := r.data.client.PreviewPackConnectionsV1OrgsOrgIdPacksConnectionsPreviewPostWithResponse(ctx, r.data.orgID,
		client.PreviewPackConnectionsV1OrgsOrgIdPacksConnectionsPreviewPostJSONRequestBody{PackVersionIds: []string{pvID}})
	if err != nil {
		diags.AddError("Error previewing pack connections", err.Error())
		return nil, false
	}
	if resp.JSON200 == nil {
		diags.AddError("Unexpected response previewing pack connections",
			fmt.Sprintf("Preview returned HTTP %d: %s", resp.StatusCode(), describeAPIError(resp.Body)))
		return nil, false
	}
	if len(resp.JSON200.NotFound) > 0 || len(resp.JSON200.Versions) == 0 {
		diags.AddAttributeError(path.Root("pack").AtName("pack_version_id"), "Pack version not found",
			fmt.Sprintf("Pack version %s is unknown, or a version of a pack this org cannot see.", pvID))
		return nil, false
	}
	return decodeConnectionPreviews(resp.JSON200.Versions[0], diags)
}

func decodeConnectionPreviews(v client.PackVersionConnectionPreview, diags *diag.Diagnostics) (map[string]connectionPreview, bool) {
	out := map[string]connectionPreview{}
	for _, item := range v.Connections {
		kind, err := item.Discriminator()
		if err != nil {
			diags.AddError("Error decoding pack connection preview", err.Error())
			return nil, false
		}
		if kind != client.PackConnectionKindMcpCatalogEntry {
			continue
		}
		mcp, err := item.AsPackMcpConnectionPreview()
		if err != nil {
			diags.AddError("Error decoding pack connection preview", err.Error())
			return nil, false
		}
		setupKind, err := mcp.Setup.Discriminator()
		if err != nil {
			diags.AddError("Error decoding pack connection setup", err.Error())
			return nil, false
		}
		out[mcp.PackVersionConnectionId] = connectionPreview{setupKind: setupKind, metered: mcp.Metered}
	}
	return out, true
}

// buildFromPackBody renders POST /bots/from-pack for one bot from the
// generated request types, so the strict PackBotRequest (no unknown keys)
// and its unions are the compiler's problem, not a hand-written shape.
func buildFromPackBody(plan BotResourceModel) ([]byte, error) {
	p := plan.Pack
	req := client.PackBotRequest{
		PackVersionId: p.PackVersionID.ValueString(),
		Name:          plan.Name.ValueStringPointer(),
		Description:   plan.Description.ValueStringPointer(),
	}
	if !plan.HostingType.IsNull() && !plan.HostingType.IsUnknown() {
		v := client.BotHostingType(plan.HostingType.ValueString())
		req.HostingType = &v
	}
	if !plan.Harness.IsNull() && !plan.Harness.IsUnknown() {
		v := client.BotHarness(plan.Harness.ValueString())
		req.Harness = &v
	}
	if !plan.AvatarURL.IsNull() {
		var av client.PackBotRequest_Avatar
		if err := av.FromPackBotCustomAvatar(client.PackBotCustomAvatar{
			Kind: client.PackBotCustomAvatarKindCustom, AvatarUrl: plan.AvatarURL.ValueString(),
		}); err != nil {
			return nil, err
		}
		req.Avatar = &av
	}
	settings, conns, err := decodePackChoices(p)
	if err != nil {
		return nil, err
	}
	if len(settings) > 0 {
		items := make([]client.PackBotRequest_Settings_Item, 0, len(settings))
		for _, c := range settings {
			item, err := buildSettingChoice(c)
			if err != nil {
				return nil, err
			}
			items = append(items, item)
		}
		req.Settings = &items
	}
	if len(conns) > 0 {
		items := make([]client.PackBotRequest_Connections_Item, 0, len(conns))
		for _, c := range conns {
			item, err := buildConnectionChoice(c)
			if err != nil {
				return nil, err
			}
			items = append(items, item)
		}
		req.Connections = &items
	}
	return json.Marshal(client.BotsFromPacksBody{Bots: []client.PackBotRequest{req}})
}

// createBotFromPack runs POST /bots/from-pack and returns the new bot and its
// install id.
func (r *BotResource) createBotFromPack(ctx context.Context, plan BotResourceModel, diags *diag.Diagnostics) (*client.BotResponse, string, bool) {
	body, err := buildFromPackBody(plan)
	if err != nil {
		diags.AddError("Error encoding bot from pack", err.Error())
		return nil, "", false
	}
	resp, err := r.data.client.CreateBotsFromPacksV1OrgsOrgIdBotsFromPackPostWithBodyWithResponse(
		ctx, r.data.orgID, "application/json", bytes.NewReader(body))
	if err != nil {
		diags.AddError("Error creating bot from pack", err.Error())
		return nil, "", false
	}
	if resp.JSON201 == nil || len(resp.JSON201.Bots) != 1 {
		diags.AddError("Unexpected response creating bot from pack",
			fmt.Sprintf("Create from pack returned HTTP %d: %s", resp.StatusCode(), describeAPIError(resp.Body)))
		return nil, "", false
	}
	created := resp.JSON201.Bots[0]
	return &created.Bot, created.Install.BotPackInstallId, true
}

// readBotInstalls reads GET /packs/installs for one bot and returns its
// installs. Since P5.1 (#4533) a bot can carry several; the callers pick.
func (r *BotResource) readBotInstalls(ctx context.Context, botID string, diags *diag.Diagnostics) ([]client.BotPackInstallView, bool) {
	uid, err := parseUUID(botID)
	if err != nil {
		diags.AddError("Error reading bot pack installs", err.Error())
		return nil, false
	}
	resp, err := r.data.client.ListBotPackInstallsV1OrgsOrgIdPacksInstallsGetWithResponse(ctx, r.data.orgID,
		&client.ListBotPackInstallsV1OrgsOrgIdPacksInstallsGetParams{BotIds: []openapi_types.UUID{uid}})
	if err != nil {
		diags.AddError("Error reading bot pack installs", err.Error())
		return nil, false
	}
	if resp.StatusCode() == 404 {
		return nil, true
	}
	if resp.JSON200 == nil {
		diags.AddError("Unexpected response reading bot pack installs",
			fmt.Sprintf("Reading installs returned HTTP %d: %s", resp.StatusCode(), describeAPIError(resp.Body)))
		return nil, false
	}
	for _, b := range resp.JSON200.Bots {
		if b.BotId == botID {
			return b.Installs, true
		}
	}
	return nil, true
}

// findInstall returns the install with this id, or nil. A tracked install
// that is gone is never swapped for another one on the bot.
func findInstall(installs []client.BotPackInstallView, id string) *client.BotPackInstallView {
	for i := range installs {
		if installs[i].BotPackInstallId == id {
			return &installs[i]
		}
	}
	return nil
}

// selectImportInstall picks the install an import adopts.
//
// With no selector: none for a bot without installs, the only one for a bot
// with one, and an error listing them for a bot with several, since "the
// first" would be a guess. A selector is an install id, or a pack slug that
// is unique on the bot (the same rule as P1: an ambiguous slug is an error).
func selectImportInstall(installs []client.BotPackInstallView, slug, selector string) (*client.BotPackInstallView, diag.Diagnostics) {
	var diags diag.Diagnostics
	if selector == "" {
		switch len(installs) {
		case 0:
			return nil, diags
		case 1:
			return &installs[0], diags
		}
		diags.AddError("Bot has several pack installs",
			fmt.Sprintf("Bot %q has %d pack installs, so the import must name the one this resource manages: "+
				"`terraform import <address> %s:<install_id>`, or `%s:<pack_slug>` when the slug is unique on the bot. "+
				"Installs:\n%s", slug, len(installs), slug, slug, listInstalls(installs)))
		return nil, diags
	}
	if in := findInstall(installs, selector); in != nil {
		return in, diags
	}
	var bySlug []*client.BotPackInstallView
	for i := range installs {
		if installs[i].PackSlug == selector {
			bySlug = append(bySlug, &installs[i])
		}
	}
	switch len(bySlug) {
	case 1:
		return bySlug[0], diags
	case 0:
		diags.AddError("No such pack install",
			fmt.Sprintf("Bot %q has no pack install with id or pack slug %q. Installs:\n%s", slug, selector, listInstalls(installs)))
	default:
		diags.AddError("Pack slug is ambiguous",
			fmt.Sprintf("Bot %q has %d installs of pack %q; import with the install id instead: `%s:<install_id>`. Installs:\n%s",
				slug, len(bySlug), selector, slug, listInstalls(installs)))
	}
	return nil, diags
}

func listInstalls(installs []client.BotPackInstallView) string {
	if len(installs) == 0 {
		return "  (none)"
	}
	lines := make([]string, 0, len(installs))
	for _, in := range installs {
		lines = append(lines, fmt.Sprintf("  - %s  %s v%d", in.BotPackInstallId, in.PackSlug, in.VersionNo))
	}
	return strings.Join(lines, "\n")
}

// mapPackInstall writes an install into the `pack` block, keeping the
// create-time choices already in m (they cannot be read back). A nil install
// clears the block.
func mapPackInstall(in *client.BotPackInstallView, prior *botPackModel) *botPackModel {
	if in == nil {
		return nil
	}
	m := &botPackModel{}
	if prior != nil {
		m.Settings, m.Connections = prior.Settings, prior.Connections
	}
	m.PackVersionID = types.StringValue(in.PackVersionId)
	m.BotPackInstallID = types.StringValue(in.BotPackInstallId)
	m.PackID = types.StringValue(in.PackId)
	m.PackSlug = types.StringValue(in.PackSlug)
	m.VersionNo = types.Int64Value(int64(in.VersionNo))
	pending := make([]attr.Value, 0, len(in.PendingConnections))
	for _, pc := range in.PendingConnections {
		pending = append(pending, types.ObjectValueMust(pendingConnectionAttrTypes, map[string]attr.Value{
			"pack_version_connection_id":   types.StringValue(pc.PackVersionConnectionId),
			"bot_pack_connection_setup_id": types.StringValue(pc.BotPackConnectionSetupId),
			"state":                        types.StringValue(string(pc.State)),
		}))
	}
	m.PendingConnections = types.ListValueMust(pendingConnectionObjectType, pending)
	return m
}

// uninstallPack removes the tracked install, keeping the bot.
func (r *BotResource) uninstallPack(ctx context.Context, installID string, diags *diag.Diagnostics) bool {
	uid, err := parseUUID(installID)
	if err != nil {
		diags.AddError("Error uninstalling pack", err.Error())
		return false
	}
	resp, err := r.data.client.UninstallPacksV1OrgsOrgIdPacksInstallsDeleteWithResponse(ctx, r.data.orgID,
		&client.UninstallPacksV1OrgsOrgIdPacksInstallsDeleteParams{BotPackInstallIds: []openapi_types.UUID{uid}})
	if err != nil {
		diags.AddError("Error uninstalling pack", err.Error())
		return false
	}
	switch code := resp.StatusCode(); {
	case resp.JSON200 != nil:
		return true
	case code == 404:
		// Already gone: the desired state holds.
		return true
	case code == 403:
		diags.AddAttributeError(path.Root("pack"), "Not allowed to uninstall this pack",
			"Only the bot's owner can uninstall its packs. A bot Terraform creates from a pack is owned by the "+
				"provider's API key; this bot belongs to someone else (for example, it was created in the app "+
				"and imported). Uninstall it in the app, or as its owner. API: "+describeAPIError(resp.Body))
		return false
	default:
		diags.AddError("Unexpected response uninstalling pack",
			fmt.Sprintf("Uninstall returned HTTP %d: %s", code, describeAPIError(resp.Body)))
		return false
	}
}

func parseUUID(s string) (openapi_types.UUID, error) {
	var u openapi_types.UUID
	if err := u.UnmarshalText([]byte(strings.TrimSpace(s))); err != nil {
		return u, fmt.Errorf("%q is not a UUID: %w", s, err)
	}
	return u, nil
}

// keepPackAvatar applies the avatar rule for bots with a pack: an unset
// `avatar_url` means "the pack's avatar", so the server's value is not
// written into an attribute the practitioner left null.
func keepPackAvatar(m *BotResourceModel, declared types.String) {
	if m.Pack != nil && declared.IsNull() {
		m.AvatarURL = types.StringNull()
	}
}

// ModifyPlan runs the `pack` guards (modifyPackPlan). Nothing else on
// botyard_bot needs plan modification.
func (r *BotResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // destroy
	}
	var plan BotResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var state *BotResourceModel
	if !req.State.Raw.IsNull() {
		state = &BotResourceModel{}
		resp.Diagnostics.Append(req.State.Get(ctx, state)...)
		if resp.Diagnostics.HasError() {
			return
		}
	}
	r.modifyPackPlan(ctx, &plan, state, &resp.Diagnostics)
}

// createFromPack is Create for a bot with a `pack` block: from-pack, then the
// sparse config PATCH when a config block is declared, then the install read
// that fills the computed `pack` attributes.
func (r *BotResource) createFromPack(ctx context.Context, plan BotResourceModel, resp *resource.CreateResponse) {
	bot, installID, ok := r.createBotFromPack(ctx, plan, &resp.Diagnostics)
	if !ok {
		return
	}
	// From here the bot exists: record it in state before anything else can
	// fail, so a failed config PATCH leaves a tracked (tainted) bot rather than
	// an orphan Terraform would try to create again.
	declaredAvatar := plan.AvatarURL
	mapBotResource(bot, &plan)
	plan.Pack.BotPackInstallID = types.StringValue(installID)
	if plan.Config != nil || plan.NativeConfig != nil {
		// The PATCH sends the config as declared. Mapping the from-pack
		// response into plan.Config first would overwrite declared leaves
		// with the pack's values (e.g. tool_search `off` -> the pack's
		// `auto`) and send those instead.
		final, ok := r.updateBotConfig(ctx, bot.Slug, plan, &resp.Diagnostics)
		if !ok {
			mapBotDesiredConfig(bot.Harness, &bot.DesiredConfig, plan.Config, plan.NativeConfig, &resp.Diagnostics)
			r.savePartialPackBot(ctx, plan, declaredAvatar, resp)
			return
		}
		mapBotResource(final, &plan)
		mapBotDesiredConfig(final.Harness, &final.DesiredConfig, plan.Config, plan.NativeConfig, &resp.Diagnostics)
	}
	if !r.refreshPack(ctx, &plan, &resp.Diagnostics) {
		r.savePartialPackBot(ctx, plan, declaredAvatar, resp)
		return
	}
	keepPackAvatar(&plan, declaredAvatar)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// savePartialPackBot records a bot whose create got past from-pack but failed
// later. Unknown computed `pack` values are made known-null so state is valid.
func (r *BotResource) savePartialPackBot(ctx context.Context, plan BotResourceModel, declaredAvatar types.String, resp *resource.CreateResponse) {
	p := plan.Pack
	for _, s := range []*types.String{&p.PackID, &p.PackSlug} {
		if s.IsUnknown() {
			*s = types.StringNull()
		}
	}
	if p.VersionNo.IsUnknown() {
		p.VersionNo = types.Int64Null()
	}
	if p.PendingConnections.IsUnknown() {
		p.PendingConnections = types.ListNull(pendingConnectionObjectType)
	}
	keepPackAvatar(&plan, declaredAvatar)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// refreshPack re-reads the tracked install into m.Pack. A bot without a
// tracked pack makes no call, and a pack installed in the app is never
// adopted into config. A tracked install that is gone clears the block
// (never falling back to another install); the next plan then reports it
// through modifyPackPlan.
func (r *BotResource) refreshPack(ctx context.Context, m *BotResourceModel, diags *diag.Diagnostics) bool {
	if m.Pack == nil {
		return true
	}
	installs, ok := r.readBotInstalls(ctx, m.ID.ValueString(), diags)
	if !ok {
		return false
	}
	m.Pack = mapPackInstall(findInstall(installs, m.Pack.BotPackInstallID.ValueString()), m.Pack)
	return true
}

// importPack fills m.Pack on import, by selectImportInstall.
func (r *BotResource) importPack(ctx context.Context, m *BotResourceModel, selector string, diags *diag.Diagnostics) bool {
	installs, ok := r.readBotInstalls(ctx, m.ID.ValueString(), diags)
	if !ok {
		return false
	}
	in, d := selectImportInstall(installs, m.Slug.ValueString(), selector)
	diags.Append(d...)
	if d.HasError() {
		return false
	}
	m.Pack = mapPackInstall(in, nil)
	return true
}

// packImportSelectorKey is the private-state key carrying the `:<selector>`
// part of an import id from ImportState to the Read that follows it.
const packImportSelectorKey = "pack_import_selector"

// splitBotImportID splits `<slug>` or `<slug>:<install_id|pack_slug>`.
func splitBotImportID(id string) (slug, selector string, err error) {
	slug, selector, _ = strings.Cut(id, ":")
	if slug == "" || strings.Contains(selector, ":") {
		return "", "", fmt.Errorf("import id %q: expected `<bot_slug>` or `<bot_slug>:<install_id|pack_slug>`", id)
	}
	return slug, selector, nil
}
