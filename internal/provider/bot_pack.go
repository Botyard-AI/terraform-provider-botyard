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
	"sort"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

// Wire values of the per-setting and per-connection choices.
const (
	packSettingChoicePack   = "pack"
	packSettingChoiceKeep   = "keep"
	packSettingChoiceCustom = "custom"

	packConnChoiceUseServer    = "use_server"
	packConnChoiceLeavePending = "leave_pending"
	packConnChoiceCreate       = "create"

	packCreateSetupNone      = "none"
	packCreateSetupForm      = "setup_form"
	packCreateSetupVaultRefs = "credential_form"

	// Connection preview setup kinds that `create` cannot satisfy.
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

type botPackSettingModel struct {
	Path   types.String `tfsdk:"path"`
	Choice types.String `tfsdk:"choice"`
	Value  types.String `tfsdk:"value"`
}

// botPackConnectionModel is one connection choice. `choice` is the
// discriminator; the other attributes belong to exactly one choice each and
// ValidateConfig refuses any that do not match it.
type botPackConnectionModel struct {
	PackVersionConnectionID types.String        `tfsdk:"pack_version_connection_id"`
	Choice                  types.String        `tfsdk:"choice"`
	McpServerID             types.String        `tfsdk:"mcp_server_id"`
	Create                  *botPackCreateModel `tfsdk:"create"`
}

type botPackCreateModel struct {
	Name          types.String            `tfsdk:"name"`
	AcceptMetered types.Bool              `tfsdk:"accept_metered"`
	Setup         botPackCreateSetupModel `tfsdk:"setup"`
}

// botPackCreateSetupModel mirrors PackCreateServerChoice.setup, a union on
// `kind`. Secrets are Runtime Vault references only, as the API requires.
type botPackCreateSetupModel struct {
	Kind               types.String                      `tfsdk:"kind"`
	CatalogFormAnswers map[string]botPackFormAnswerModel `tfsdk:"catalog_form_answers"`
	VaultEntryRefs     map[string]types.String           `tfsdk:"vault_entry_refs"`
}

type botPackFormAnswerModel struct {
	Mode  types.String `tfsdk:"mode"`
	Value types.String `tfsdk:"value"`
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
			"settings": schema.ListNestedAttribute{
				Optional: true,
				MarkdownDescription: "Per pack setting: `pack` (the default for a setting left out), `keep` " +
					"the bot's own value, or `custom` with `value`. Applied at creation only.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"path": schema.StringAttribute{
						Required:            true,
						MarkdownDescription: "The pack setting, e.g. `tool_search`.",
					},
					"choice": schema.StringAttribute{
						Required:            true,
						MarkdownDescription: "`pack`, `keep` or `custom`.",
						Validators: []validator.String{stringvalidator.OneOf(
							packSettingChoicePack, packSettingChoiceKeep, packSettingChoiceCustom)},
					},
					"value": schema.StringAttribute{
						Optional: true,
						MarkdownDescription: "`custom` only: the value as JSON, in the setting's own shape, " +
							"e.g. `jsonencode({ mode = \"auto\" })` for `tool_search`.",
					},
				}},
			},
			"connections": schema.ListNestedAttribute{
				Optional: true,
				MarkdownDescription: "Per MCP connection of the pack version: `use_server` binds an org MCP " +
					"server, `leave_pending` binds nothing, and `create` connects a new account with Runtime " +
					"Vault references. A connection left out binds the one visible server from its catalogue " +
					"entry, as in the app. Applied at creation only. A connection that needs a vendor sign-in " +
					"(OAuth) cannot be created here: connect it in the app first, then use `use_server` or " +
					"`leave_pending`.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"pack_version_connection_id": schema.StringAttribute{
						Required:            true,
						MarkdownDescription: "A connection of the pack version, from `data.botyard_pack_version`.",
					},
					"choice": schema.StringAttribute{
						Required:            true,
						MarkdownDescription: "`use_server`, `leave_pending` or `create`.",
						Validators: []validator.String{stringvalidator.OneOf(
							packConnChoiceUseServer, packConnChoiceLeavePending, packConnChoiceCreate)},
					},
					"mcp_server_id": schema.StringAttribute{
						Optional:            true,
						MarkdownDescription: "`use_server` only: the org MCP server to bind, e.g. `botyard_mcp_server.crm.id`.",
					},
					"create": schema.SingleNestedAttribute{
						Optional:            true,
						MarkdownDescription: "`create` only: the new account.",
						Attributes: map[string]schema.Attribute{
							"name": schema.StringAttribute{
								Optional:            true,
								MarkdownDescription: "The new server's name. Defaults to the catalogue entry's.",
							},
							"accept_metered": schema.BoolAttribute{
								Optional: true,
								MarkdownDescription: "Must be `true` when the connection is metered: the new server " +
									"is billed as a hosted MCP server.",
							},
							"setup": schema.SingleNestedAttribute{
								Required:            true,
								MarkdownDescription: "What the new account needs, matching the connection's setup kind.",
								Attributes: map[string]schema.Attribute{
									"kind": schema.StringAttribute{
										Required:            true,
										MarkdownDescription: "`none`, `setup_form` or `credential_form`.",
										Validators: []validator.String{stringvalidator.OneOf(
											packCreateSetupNone, packCreateSetupForm, packCreateSetupVaultRefs)},
									},
									"catalog_form_answers": schema.MapNestedAttribute{
										Optional: true,
										MarkdownDescription: "`setup_form` only: field id to answer. A secret field " +
											"must be `mode = \"secret_ref\"` with a Runtime Vault key path.",
										NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
											"mode": schema.StringAttribute{
												Required:            true,
												MarkdownDescription: "`plaintext` or `secret_ref`.",
												Validators:          []validator.String{stringvalidator.OneOf("plaintext", "secret_ref")},
											},
											"value": schema.StringAttribute{
												Required:            true,
												MarkdownDescription: "The plaintext value, or the vault key path for `secret_ref`.",
											},
										}},
									},
									"vault_entry_refs": schema.MapAttribute{
										Optional:    true,
										ElementType: types.StringType,
										MarkdownDescription: "`credential_form` only: credential field name to the id " +
											"of an org Runtime Vault secret holding its value.",
									},
								},
							},
						},
					},
				}},
			},

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

// validatePackConfig checks the `pack` block's internal consistency at plan
// time, without calling the API. Unknown values are skipped.
func validatePackConfig(p *botPackModel, diags *diag.Diagnostics) {
	if p == nil {
		return
	}
	root := path.Root("pack")

	seenPaths := map[string]bool{}
	for i, s := range p.Settings {
		at := root.AtName("settings").AtListIndex(i)
		if !s.Path.IsUnknown() && !s.Path.IsNull() {
			if seenPaths[s.Path.ValueString()] {
				diags.AddAttributeError(at.AtName("path"), "Duplicate pack setting",
					fmt.Sprintf("Setting %q is listed more than once; each path may appear at most once.", s.Path.ValueString()))
			}
			seenPaths[s.Path.ValueString()] = true
		}
		if s.Choice.IsUnknown() {
			continue
		}
		isCustom := s.Choice.ValueString() == packSettingChoiceCustom
		switch {
		case isCustom && s.Value.IsNull():
			diags.AddAttributeError(at.AtName("value"), "Missing setting value",
				"`choice = \"custom\"` needs a `value` (JSON, e.g. `jsonencode({ mode = \"auto\" })`).")
		case !isCustom && !s.Value.IsNull():
			diags.AddAttributeError(at.AtName("value"), "Unexpected setting value",
				fmt.Sprintf("`value` is only for `choice = \"custom\"`, not %q.", s.Choice.ValueString()))
		case isCustom && !s.Value.IsUnknown() && !json.Valid([]byte(s.Value.ValueString())):
			diags.AddAttributeError(at.AtName("value"), "Setting value is not JSON",
				"`value` must be JSON. Use `jsonencode(...)`.")
		}
	}

	seenConns := map[string]bool{}
	for i, c := range p.Connections {
		at := root.AtName("connections").AtListIndex(i)
		if !c.PackVersionConnectionID.IsUnknown() && !c.PackVersionConnectionID.IsNull() {
			id := c.PackVersionConnectionID.ValueString()
			if seenConns[id] {
				diags.AddAttributeError(at.AtName("pack_version_connection_id"), "Duplicate pack connection",
					fmt.Sprintf("Connection %q is listed more than once; each connection may appear at most once.", id))
			}
			seenConns[id] = true
		}
		if c.Choice.IsUnknown() {
			continue
		}
		choice := c.Choice.ValueString()
		hasServer, hasCreate := !c.McpServerID.IsNull(), c.Create != nil
		if (choice == packConnChoiceUseServer) != hasServer {
			diags.AddAttributeError(at.AtName("mcp_server_id"), "Connection fields do not match its choice",
				fmt.Sprintf("`mcp_server_id` is required for `use_server` and not allowed otherwise; this connection's choice is %q.", choice))
		}
		if (choice == packConnChoiceCreate) != hasCreate {
			diags.AddAttributeError(at.AtName("create"), "Connection fields do not match its choice",
				fmt.Sprintf("`create` is required for `choice = \"create\"` and not allowed otherwise; this connection's choice is %q.", choice))
		}
		if hasCreate {
			validateCreateSetup(c.Create.Setup, at.AtName("create").AtName("setup"), diags)
		}
	}
}

func validateCreateSetup(s botPackCreateSetupModel, at path.Path, diags *diag.Diagnostics) {
	if s.Kind.IsUnknown() {
		return
	}
	kind := s.Kind.ValueString()
	if kind != packCreateSetupForm && s.CatalogFormAnswers != nil {
		diags.AddAttributeError(at.AtName("catalog_form_answers"), "Setup fields do not match its kind",
			fmt.Sprintf("`catalog_form_answers` is only for `kind = \"setup_form\"`, not %q.", kind))
	}
	switch {
	case kind == packCreateSetupVaultRefs && len(s.VaultEntryRefs) == 0:
		diags.AddAttributeError(at.AtName("vault_entry_refs"), "Missing vault references",
			"`kind = \"credential_form\"` needs at least one `vault_entry_refs` entry.")
	case kind != packCreateSetupVaultRefs && s.VaultEntryRefs != nil:
		diags.AddAttributeError(at.AtName("vault_entry_refs"), "Setup fields do not match its kind",
			fmt.Sprintf("`vault_entry_refs` is only for `kind = \"credential_form\"`, not %q.", kind))
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

// packChoicesEqual reports whether two blocks declare the same create-time
// choices. Only used to decide whether to warn.
func packChoicesEqual(a, b *botPackModel) bool {
	if len(a.Settings) != len(b.Settings) || len(a.Connections) != len(b.Connections) {
		return false
	}
	for i := range a.Settings {
		x, y := a.Settings[i], b.Settings[i]
		if !x.Path.Equal(y.Path) || !x.Choice.Equal(y.Choice) || !x.Value.Equal(y.Value) {
			return false
		}
	}
	for i := range a.Connections {
		if !connectionChoiceEqual(a.Connections[i], b.Connections[i]) {
			return false
		}
	}
	return true
}

func connectionChoiceEqual(x, y botPackConnectionModel) bool {
	if !x.PackVersionConnectionID.Equal(y.PackVersionConnectionID) || !x.Choice.Equal(y.Choice) ||
		!x.McpServerID.Equal(y.McpServerID) || (x.Create == nil) != (y.Create == nil) {
		return false
	}
	if x.Create == nil {
		return true
	}
	xc, yc := x.Create, y.Create
	if !xc.Name.Equal(yc.Name) || !xc.AcceptMetered.Equal(yc.AcceptMetered) || !xc.Setup.Kind.Equal(yc.Setup.Kind) ||
		len(xc.Setup.CatalogFormAnswers) != len(yc.Setup.CatalogFormAnswers) ||
		len(xc.Setup.VaultEntryRefs) != len(yc.Setup.VaultEntryRefs) {
		return false
	}
	for k, xa := range xc.Setup.CatalogFormAnswers {
		ya, ok := yc.Setup.CatalogFormAnswers[k]
		if !ok || !xa.Mode.Equal(ya.Mode) || !xa.Value.Equal(ya.Value) {
			return false
		}
	}
	for k, xv := range xc.Setup.VaultEntryRefs {
		if yv, ok := yc.Setup.VaultEntryRefs[k]; !ok || !xv.Equal(yv) {
			return false
		}
	}
	return true
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
	if len(p.Settings) > 0 {
		settings := make([]client.PackBotRequest_Settings_Item, 0, len(p.Settings))
		for _, s := range p.Settings {
			item, err := buildSettingChoice(s)
			if err != nil {
				return nil, err
			}
			settings = append(settings, item)
		}
		req.Settings = &settings
	}
	if len(p.Connections) > 0 {
		conns := make([]client.PackBotRequest_Connections_Item, 0, len(p.Connections))
		for _, c := range p.Connections {
			item, err := buildConnectionChoice(c)
			if err != nil {
				return nil, err
			}
			conns = append(conns, item)
		}
		req.Connections = &conns
	}
	return json.Marshal(client.BotsFromPacksBody{Bots: []client.PackBotRequest{req}})
}

func buildSettingChoice(s botPackSettingModel) (client.PackBotRequest_Settings_Item, error) {
	var item client.PackBotRequest_Settings_Item
	settingPath := client.PackSettingPath(s.Path.ValueString())
	switch choice := s.Choice.ValueString(); choice {
	case packSettingChoicePack:
		return item, item.FromPackSettingPackChoice(client.PackSettingPackChoice{
			Choice: client.PackSettingPackChoiceChoicePack, Path: settingPath,
		})
	case packSettingChoiceKeep:
		return item, item.FromPackSettingKeepChoice(client.PackSettingKeepChoice{
			Choice: client.PackSettingKeepChoiceChoiceKeep, Path: settingPath,
		})
	case packSettingChoiceCustom:
		var v client.JsonValueInput
		if err := json.Unmarshal([]byte(s.Value.ValueString()), &v); err != nil {
			return item, fmt.Errorf("setting %q: value is not JSON: %w", s.Path.ValueString(), err)
		}
		return item, item.FromPackSettingCustomChoice(client.PackSettingCustomChoice{
			Choice: client.PackSettingCustomChoiceChoiceCustom, Path: settingPath, Value: &v,
		})
	default:
		return item, fmt.Errorf("unknown setting choice %q", choice)
	}
}

// buildConnectionChoice renders one connection choice, dispatched on `choice`.
func buildConnectionChoice(c botPackConnectionModel) (client.PackBotRequest_Connections_Item, error) {
	var item client.PackBotRequest_Connections_Item
	id := c.PackVersionConnectionID.ValueString()
	switch choice := c.Choice.ValueString(); choice {
	case packConnChoiceUseServer:
		return item, item.FromPackUseServerChoice(client.PackUseServerChoice{
			Choice: client.PackUseServerChoiceChoiceUseServer, PackVersionConnectionId: id, McpServerId: c.McpServerID.ValueString(),
		})
	case packConnChoiceLeavePending:
		return item, item.FromPackLeavePendingChoice(client.PackLeavePendingChoice{
			Choice: client.PackLeavePendingChoiceChoiceLeavePending, PackVersionConnectionId: id,
		})
	case packConnChoiceCreate:
		setup, err := buildCreateSetup(c.Create.Setup)
		if err != nil {
			return item, err
		}
		return item, item.FromPackCreateServerChoice(client.PackCreateServerChoice{
			Choice:                  client.PackCreateServerChoiceChoiceCreate,
			PackVersionConnectionId: id,
			Name:                    c.Create.Name.ValueStringPointer(),
			AcceptMetered:           c.Create.AcceptMetered.ValueBoolPointer(),
			Setup:                   setup,
		})
	default:
		return item, fmt.Errorf("unknown connection choice %q", choice)
	}
}

func buildCreateSetup(s botPackCreateSetupModel) (client.PackCreateServerChoice_Setup, error) {
	var setup client.PackCreateServerChoice_Setup
	switch kind := s.Kind.ValueString(); kind {
	case packCreateSetupNone:
		return setup, setup.FromPackCreateNone(client.PackCreateNone{Kind: client.PackCreateNoneKindNone})
	case packCreateSetupForm:
		form := client.PackCreateSetupForm{Kind: client.PackCreateSetupFormKindSetupForm}
		if s.CatalogFormAnswers != nil {
			answers := make(map[string]client.McpCatalogFormFieldValueInput, len(s.CatalogFormAnswers))
			for id, a := range s.CatalogFormAnswers {
				mode := client.McpCatalogFormValueMode(a.Mode.ValueString())
				answers[id] = client.McpCatalogFormFieldValueInput{Mode: &mode, Value: a.Value.ValueStringPointer()}
			}
			form.CatalogFormAnswers = &answers
		}
		return setup, setup.FromPackCreateSetupForm(form)
	case packCreateSetupVaultRefs:
		refs := make(map[string]string, len(s.VaultEntryRefs))
		for k, v := range s.VaultEntryRefs {
			refs[k] = v.ValueString()
		}
		return setup, setup.FromPackCreateCredentialForm(client.PackCreateCredentialForm{
			Kind: client.PackCreateCredentialFormKindCredentialForm, VaultEntryRefs: refs,
		})
	default:
		return setup, fmt.Errorf("unknown create setup kind %q", kind)
	}
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

// packReadResult is what reading a bot's installs found for the `pack` block.
type packReadResult struct {
	install *client.BotPackInstallView
}

// readPackInstall reads GET /packs/installs for one bot and picks the install
// the `pack` block tracks: the one with wantInstallID, or on import (no id
// yet) the bot's first install, which is the one from-pack created. Since
// P5.1 a bot can carry other packs installed in the app; those are left
// alone. Returns a nil install when the bot has none to track.
func (r *BotResource) readPackInstall(ctx context.Context, botID, wantInstallID string, diags *diag.Diagnostics) (packReadResult, bool) {
	uid, err := parseUUID(botID)
	if err != nil {
		diags.AddError("Error reading bot pack installs", err.Error())
		return packReadResult{}, false
	}
	resp, err := r.data.client.ListBotPackInstallsV1OrgsOrgIdPacksInstallsGetWithResponse(ctx, r.data.orgID,
		&client.ListBotPackInstallsV1OrgsOrgIdPacksInstallsGetParams{BotIds: []openapi_types.UUID{uid}})
	if err != nil {
		diags.AddError("Error reading bot pack installs", err.Error())
		return packReadResult{}, false
	}
	if resp.StatusCode() == 404 {
		return packReadResult{}, true
	}
	if resp.JSON200 == nil {
		diags.AddError("Unexpected response reading bot pack installs",
			fmt.Sprintf("Reading installs returned HTTP %d: %s", resp.StatusCode(), describeAPIError(resp.Body)))
		return packReadResult{}, false
	}
	return packReadResult{install: selectPackInstall(resp.JSON200, botID, wantInstallID)}, true
}

func selectPackInstall(view *client.OrgBotPackInstalls, botID, wantInstallID string) *client.BotPackInstallView {
	for _, b := range view.Bots {
		if b.BotId != botID || len(b.Installs) == 0 {
			continue
		}
		if wantInstallID != "" {
			for i := range b.Installs {
				if b.Installs[i].BotPackInstallId == wantInstallID {
					return &b.Installs[i]
				}
			}
			return nil
		}
		installs := append([]client.BotPackInstallView(nil), b.Installs...)
		sort.SliceStable(installs, func(i, j int) bool { return installs[i].InstalledAt.Before(installs[j].InstalledAt) })
		return &installs[0]
	}
	return nil
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
	mapBotDesiredConfig(bot.Harness, &bot.DesiredConfig, plan.Config, plan.NativeConfig, &resp.Diagnostics)
	plan.Pack.BotPackInstallID = types.StringValue(installID)
	if plan.Config != nil || plan.NativeConfig != nil {
		final, ok := r.updateBotConfig(ctx, bot.Slug, plan, &resp.Diagnostics)
		if !ok {
			r.savePartialPackBot(ctx, plan, declaredAvatar, resp)
			return
		}
		mapBotResource(final, &plan)
		mapBotDesiredConfig(final.Harness, &final.DesiredConfig, plan.Config, plan.NativeConfig, &resp.Diagnostics)
	}
	if !r.refreshPack(ctx, &plan, false, &resp.Diagnostics) {
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

// refreshPack re-reads the tracked install into m.Pack. On import (no pack
// in state, no install id) it adopts the bot's first install, so importing a
// bot made from a pack fills in `pack`. A tracked bot without a pack stays
// without one: a pack installed in the app is not adopted into config. An install gone remotely clears the
// block, which the next plan reports through modifyPackPlan.
func (r *BotResource) refreshPack(ctx context.Context, m *BotResourceModel, importing bool, diags *diag.Diagnostics) bool {
	if m.Pack == nil && !importing {
		return true // no pack to track: no extra API call per refresh
	}
	want := ""
	if m.Pack != nil {
		want = m.Pack.BotPackInstallID.ValueString()
	}
	got, ok := r.readPackInstall(ctx, m.ID.ValueString(), want, diags)
	if !ok {
		return false
	}
	m.Pack = mapPackInstall(got.install, m.Pack)
	return true
}
