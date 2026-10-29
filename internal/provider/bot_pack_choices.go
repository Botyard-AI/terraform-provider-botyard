package provider

// The create-time choices of the `pack` block: per-setting and per-connection
// choices, and the setup of a `create` connection.
//
// Each is a union on its discriminator (`choice`, or `kind` for a setup) and
// is modelled in HCL as one payload attribute per variant that carries data:
// `custom = { value }`, `use_server = { mcp_server_id }`, `create = { ... }`,
// `setup_form = { ... }`, `credential_form = { ... }`. Variants without data
// (`pack`, `keep`, `leave_pending`, setup `none`) have no payload.
//
// The tfsdk structs below are serialization DTOs only. decodePackChoices
// turns them into sealed per-variant Go types at the boundary, refusing a
// payload that does not match its discriminator, and the wire builders
// consume only those sealed types.

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

// Discriminator values, as on the wire.
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
)

// --- serialization DTOs (tfsdk) ---------------------------------------------

type botPackSettingModel struct {
	Path   types.String               `tfsdk:"path"`
	Choice types.String               `tfsdk:"choice"`
	Custom *botPackSettingCustomModel `tfsdk:"custom"`
}

type botPackSettingCustomModel struct {
	Value types.String `tfsdk:"value"`
}

type botPackConnectionModel struct {
	PackVersionConnectionID types.String           `tfsdk:"pack_version_connection_id"`
	Choice                  types.String           `tfsdk:"choice"`
	UseServer               *botPackUseServerModel `tfsdk:"use_server"`
	Create                  *botPackCreateModel    `tfsdk:"create"`
}

type botPackUseServerModel struct {
	McpServerID types.String `tfsdk:"mcp_server_id"`
}

type botPackCreateModel struct {
	Name          types.String            `tfsdk:"name"`
	AcceptMetered types.Bool              `tfsdk:"accept_metered"`
	Setup         botPackCreateSetupModel `tfsdk:"setup"`
}

type botPackCreateSetupModel struct {
	Kind           types.String                `tfsdk:"kind"`
	SetupForm      *botPackSetupFormModel      `tfsdk:"setup_form"`
	CredentialForm *botPackCredentialFormModel `tfsdk:"credential_form"`
}

type botPackSetupFormModel struct {
	CatalogFormAnswers map[string]botPackFormAnswerModel `tfsdk:"catalog_form_answers"`
}

type botPackCredentialFormModel struct {
	VaultEntryRefs map[string]types.String `tfsdk:"vault_entry_refs"`
}

type botPackFormAnswerModel struct {
	Mode  types.String `tfsdk:"mode"`
	Value types.String `tfsdk:"value"`
}

// --- sealed domain types ----------------------------------------------------

// packSettingChoice is one decoded setting choice. Sealed: dispatch with a
// type switch on *settingPack / *settingKeep / *settingCustom.
type packSettingChoice interface{ isPackSettingChoice() }

type settingPack struct{ path string }
type settingKeep struct{ path string }
type settingCustom struct {
	path  string
	value json.RawMessage
}

func (*settingPack) isPackSettingChoice()   {}
func (*settingKeep) isPackSettingChoice()   {}
func (*settingCustom) isPackSettingChoice() {}

// packConnChoice is one decoded connection choice. Sealed: dispatch with a
// type switch on *connUseServer / *connLeavePending / *connCreate.
type packConnChoice interface{ isPackConnChoice() }

type connUseServer struct{ connectionID, mcpServerID string }
type connLeavePending struct{ connectionID string }
type connCreate struct {
	connectionID  string
	name          *string
	acceptMetered *bool
	setup         packCreateSetup
}

func (*connUseServer) isPackConnChoice()    {}
func (*connLeavePending) isPackConnChoice() {}
func (*connCreate) isPackConnChoice()       {}

// packCreateSetup is a decoded `create` setup. Sealed: dispatch with a type
// switch on *setupNone / *setupForm / *setupVaultRefs.
type packCreateSetup interface{ isPackCreateSetup() }

type setupNone struct{}
type setupForm struct{ answers map[string]formAnswer }
type setupVaultRefs struct{ refs map[string]string }
type formAnswer struct{ mode, value string }

func (*setupNone) isPackCreateSetup()      {}
func (*setupForm) isPackCreateSetup()      {}
func (*setupVaultRefs) isPackCreateSetup() {}

// --- schema -------------------------------------------------------------------

func packSettingsSchemaAttribute() schema.ListNestedAttribute {
	return schema.ListNestedAttribute{
		Optional: true,
		MarkdownDescription: "Per pack setting: `pack` (the default for a setting left out), `keep` the bot's " +
			"own value, or `custom` with a `custom = { value }` payload. Applied at creation only.",
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
			"custom": schema.SingleNestedAttribute{
				Optional:            true,
				MarkdownDescription: "`choice = \"custom\"` only, and required for it.",
				Attributes: map[string]schema.Attribute{
					"value": schema.StringAttribute{
						Required: true,
						MarkdownDescription: "The value as JSON, in the setting's own shape, e.g. " +
							"`jsonencode({ mode = \"auto\" })` for `tool_search`.",
					},
				},
			},
		}},
	}
}

func packConnectionsSchemaAttribute() schema.ListNestedAttribute {
	return schema.ListNestedAttribute{
		Optional: true,
		MarkdownDescription: "Per MCP connection of the pack version: `use_server` binds an org MCP server, " +
			"`leave_pending` binds nothing, and `create` connects a new account with Runtime Vault references. " +
			"A connection left out binds the one visible server from its catalogue entry, as in the app. " +
			"Applied at creation only. A connection that needs a vendor sign-in (OAuth) cannot be created " +
			"here: connect it in the app first, then use `use_server` or `leave_pending`.",
		NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
			"pack_version_connection_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "A connection of the pack version.",
			},
			"choice": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "`use_server`, `leave_pending` or `create`.",
				Validators: []validator.String{stringvalidator.OneOf(
					packConnChoiceUseServer, packConnChoiceLeavePending, packConnChoiceCreate)},
			},
			"use_server": schema.SingleNestedAttribute{
				Optional:            true,
				MarkdownDescription: "`choice = \"use_server\"` only, and required for it.",
				Attributes: map[string]schema.Attribute{
					"mcp_server_id": schema.StringAttribute{
						Required:            true,
						MarkdownDescription: "The org MCP server to bind, e.g. `botyard_mcp_server.crm.id`.",
					},
				},
			},
			"create": schema.SingleNestedAttribute{
				Optional:            true,
				MarkdownDescription: "`choice = \"create\"` only, and required for it: the new account.",
				Attributes: map[string]schema.Attribute{
					"name": schema.StringAttribute{
						Optional:            true,
						MarkdownDescription: "The new server's name. Defaults to the catalogue entry's.",
					},
					"accept_metered": schema.BoolAttribute{
						Optional: true,
						MarkdownDescription: "Must be `true` when the connection is metered: the new server is " +
							"billed as a hosted MCP server.",
					},
					"setup": packCreateSetupSchemaAttribute(),
				},
			},
		}},
	}
}

func packCreateSetupSchemaAttribute() schema.SingleNestedAttribute {
	return schema.SingleNestedAttribute{
		Required:            true,
		MarkdownDescription: "What the new account needs, matching the connection's setup kind.",
		Attributes: map[string]schema.Attribute{
			"kind": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "`none`, `setup_form` or `credential_form`.",
				Validators: []validator.String{stringvalidator.OneOf(
					packCreateSetupNone, packCreateSetupForm, packCreateSetupVaultRefs)},
			},
			"setup_form": schema.SingleNestedAttribute{
				Optional:            true,
				MarkdownDescription: "`kind = \"setup_form\"` only, and required for it.",
				Attributes: map[string]schema.Attribute{
					"catalog_form_answers": schema.MapNestedAttribute{
						Required: true,
						MarkdownDescription: "Field id to answer. A secret field must be `mode = \"secret_ref\"` " +
							"with a Runtime Vault key path.",
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
				},
			},
			"credential_form": schema.SingleNestedAttribute{
				Optional:            true,
				MarkdownDescription: "`kind = \"credential_form\"` only, and required for it.",
				Attributes: map[string]schema.Attribute{
					"vault_entry_refs": schema.MapAttribute{
						Required:    true,
						ElementType: types.StringType,
						MarkdownDescription: "Credential field name to the id of an org Runtime Vault secret " +
							"holding its value. At least one.",
					},
				},
			},
		},
	}
}

// --- validation and decoding --------------------------------------------------

// validatePackConfig checks the `pack` block at plan time, without the API.
// Values not yet known are skipped; decodePackChoices re-checks at apply.
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
		requirePayload(at, "custom", s.Choice.ValueString(), packSettingChoiceCustom, s.Custom != nil, diags)
		if s.Custom != nil && !s.Custom.Value.IsUnknown() && !json.Valid([]byte(s.Custom.Value.ValueString())) {
			diags.AddAttributeError(at.AtName("custom").AtName("value"), "Setting value is not JSON",
				"`custom.value` must be JSON. Use `jsonencode(...)`.")
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
		requirePayload(at, "use_server", choice, packConnChoiceUseServer, c.UseServer != nil, diags)
		requirePayload(at, "create", choice, packConnChoiceCreate, c.Create != nil, diags)
		if c.Create != nil {
			validateCreateSetup(c.Create.Setup, at.AtName("create").AtName("setup"), diags)
		}
	}
}

func validateCreateSetup(s botPackCreateSetupModel, at path.Path, diags *diag.Diagnostics) {
	if s.Kind.IsUnknown() {
		return
	}
	kind := s.Kind.ValueString()
	requirePayload(at, "setup_form", kind, packCreateSetupForm, s.SetupForm != nil, diags)
	requirePayload(at, "credential_form", kind, packCreateSetupVaultRefs, s.CredentialForm != nil, diags)
	if s.CredentialForm != nil && len(s.CredentialForm.VaultEntryRefs) == 0 {
		diags.AddAttributeError(at.AtName("credential_form").AtName("vault_entry_refs"), "Missing vault references",
			"`credential_form.vault_entry_refs` needs at least one entry.")
	}
}

// requirePayload enforces that the payload attribute `name`, belonging to
// discriminator value `owner`, is set exactly when the discriminator is it.
func requirePayload(at path.Path, name, disc, owner string, present bool, diags *diag.Diagnostics) {
	switch {
	case disc == owner && !present:
		diags.AddAttributeError(at.AtName(name), "Missing variant payload",
			fmt.Sprintf("%q needs a `%s = { ... }` block.", owner, name))
	case disc != owner && present:
		diags.AddAttributeError(at.AtName(name), "Payload does not match its discriminator",
			fmt.Sprintf("`%s` belongs to %q, not %q.", name, owner, disc))
	}
}

// decodePackChoices turns the DTOs into sealed variants. Called at apply,
// when every value is known; any mismatch is an error, never a guess.
func decodePackChoices(p *botPackModel) ([]packSettingChoice, []packConnChoice, error) {
	settings := make([]packSettingChoice, 0, len(p.Settings))
	for _, s := range p.Settings {
		c, err := decodeSettingChoice(s)
		if err != nil {
			return nil, nil, err
		}
		settings = append(settings, c)
	}
	conns := make([]packConnChoice, 0, len(p.Connections))
	for _, c := range p.Connections {
		d, err := decodeConnChoice(c)
		if err != nil {
			return nil, nil, err
		}
		conns = append(conns, d)
	}
	return settings, conns, nil
}

func decodeSettingChoice(s botPackSettingModel) (packSettingChoice, error) {
	settingPath := s.Path.ValueString()
	switch choice := s.Choice.ValueString(); {
	case choice == packSettingChoicePack && s.Custom == nil:
		return &settingPack{path: settingPath}, nil
	case choice == packSettingChoiceKeep && s.Custom == nil:
		return &settingKeep{path: settingPath}, nil
	case choice == packSettingChoiceCustom && s.Custom != nil:
		v := json.RawMessage(s.Custom.Value.ValueString())
		if !json.Valid(v) {
			return nil, fmt.Errorf("setting %q: custom.value is not JSON", settingPath)
		}
		return &settingCustom{path: settingPath, value: v}, nil
	default:
		return nil, fmt.Errorf("setting %q: choice %q does not match its payload", settingPath, choice)
	}
}

func decodeConnChoice(c botPackConnectionModel) (packConnChoice, error) {
	id := c.PackVersionConnectionID.ValueString()
	switch choice := c.Choice.ValueString(); {
	case choice == packConnChoiceUseServer && c.UseServer != nil && c.Create == nil:
		return &connUseServer{connectionID: id, mcpServerID: c.UseServer.McpServerID.ValueString()}, nil
	case choice == packConnChoiceLeavePending && c.UseServer == nil && c.Create == nil:
		return &connLeavePending{connectionID: id}, nil
	case choice == packConnChoiceCreate && c.Create != nil && c.UseServer == nil:
		setup, err := decodeCreateSetup(c.Create.Setup)
		if err != nil {
			return nil, fmt.Errorf("connection %q: %w", id, err)
		}
		return &connCreate{
			connectionID:  id,
			name:          c.Create.Name.ValueStringPointer(),
			acceptMetered: c.Create.AcceptMetered.ValueBoolPointer(),
			setup:         setup,
		}, nil
	default:
		return nil, fmt.Errorf("connection %q: choice %q does not match its payload", id, choice)
	}
}

func decodeCreateSetup(s botPackCreateSetupModel) (packCreateSetup, error) {
	switch kind := s.Kind.ValueString(); {
	case kind == packCreateSetupNone && s.SetupForm == nil && s.CredentialForm == nil:
		return &setupNone{}, nil
	case kind == packCreateSetupForm && s.SetupForm != nil && s.CredentialForm == nil:
		answers := make(map[string]formAnswer, len(s.SetupForm.CatalogFormAnswers))
		for k, a := range s.SetupForm.CatalogFormAnswers {
			answers[k] = formAnswer{mode: a.Mode.ValueString(), value: a.Value.ValueString()}
		}
		return &setupForm{answers: answers}, nil
	case kind == packCreateSetupVaultRefs && s.CredentialForm != nil && s.SetupForm == nil:
		refs := make(map[string]string, len(s.CredentialForm.VaultEntryRefs))
		for k, v := range s.CredentialForm.VaultEntryRefs {
			refs[k] = v.ValueString()
		}
		return &setupVaultRefs{refs: refs}, nil
	default:
		return nil, fmt.Errorf("setup kind %q does not match its payload", kind)
	}
}

// packChoicesEqual reports whether two blocks declare the same create-time
// choices. Only used to decide whether to warn; the DTOs hold framework
// values, which compare by value and state under DeepEqual.
func packChoicesEqual(a, b *botPackModel) bool {
	return reflect.DeepEqual(a.Settings, b.Settings) && reflect.DeepEqual(a.Connections, b.Connections)
}

// --- wire builders (sealed types → generated request types) ------------------

func buildSettingChoice(s packSettingChoice) (client.PackBotRequest_Settings_Item, error) {
	var item client.PackBotRequest_Settings_Item
	switch v := s.(type) {
	case *settingPack:
		return item, item.FromPackSettingPackChoice(client.PackSettingPackChoice{
			Choice: client.PackSettingPackChoiceChoicePack, Path: client.PackSettingPath(v.path),
		})
	case *settingKeep:
		return item, item.FromPackSettingKeepChoice(client.PackSettingKeepChoice{
			Choice: client.PackSettingKeepChoiceChoiceKeep, Path: client.PackSettingPath(v.path),
		})
	case *settingCustom:
		var val client.JsonValueInput
		if err := json.Unmarshal(v.value, &val); err != nil {
			return item, fmt.Errorf("setting %q: %w", v.path, err)
		}
		return item, item.FromPackSettingCustomChoice(client.PackSettingCustomChoice{
			Choice: client.PackSettingCustomChoiceChoiceCustom, Path: client.PackSettingPath(v.path), Value: &val,
		})
	default:
		return item, fmt.Errorf("unhandled setting choice %T", s)
	}
}

func buildConnectionChoice(c packConnChoice) (client.PackBotRequest_Connections_Item, error) {
	var item client.PackBotRequest_Connections_Item
	switch v := c.(type) {
	case *connUseServer:
		return item, item.FromPackUseServerChoice(client.PackUseServerChoice{
			Choice: client.PackUseServerChoiceChoiceUseServer, PackVersionConnectionId: v.connectionID, McpServerId: v.mcpServerID,
		})
	case *connLeavePending:
		return item, item.FromPackLeavePendingChoice(client.PackLeavePendingChoice{
			Choice: client.PackLeavePendingChoiceChoiceLeavePending, PackVersionConnectionId: v.connectionID,
		})
	case *connCreate:
		setup, err := buildCreateSetup(v.setup)
		if err != nil {
			return item, err
		}
		return item, item.FromPackCreateServerChoice(client.PackCreateServerChoice{
			Choice:                  client.PackCreateServerChoiceChoiceCreate,
			PackVersionConnectionId: v.connectionID,
			Name:                    v.name,
			AcceptMetered:           v.acceptMetered,
			Setup:                   setup,
		})
	default:
		return item, fmt.Errorf("unhandled connection choice %T", c)
	}
}

func buildCreateSetup(s packCreateSetup) (client.PackCreateServerChoice_Setup, error) {
	var setup client.PackCreateServerChoice_Setup
	switch v := s.(type) {
	case *setupNone:
		return setup, setup.FromPackCreateNone(client.PackCreateNone{Kind: client.PackCreateNoneKindNone})
	case *setupForm:
		answers := make(map[string]client.McpCatalogFormFieldValueInput, len(v.answers))
		for id, a := range v.answers {
			mode, value := client.McpCatalogFormValueMode(a.mode), a.value
			answers[id] = client.McpCatalogFormFieldValueInput{Mode: &mode, Value: &value}
		}
		return setup, setup.FromPackCreateSetupForm(client.PackCreateSetupForm{
			Kind: client.PackCreateSetupFormKindSetupForm, CatalogFormAnswers: &answers,
		})
	case *setupVaultRefs:
		return setup, setup.FromPackCreateCredentialForm(client.PackCreateCredentialForm{
			Kind: client.PackCreateCredentialFormKindCredentialForm, VaultEntryRefs: v.refs,
		})
	default:
		return setup, fmt.Errorf("unhandled create setup %T", s)
	}
}
