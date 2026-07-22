package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

// credentialScopeValues is the closed set of valid credential scopes, shared by
// the schema's scope validator and the import-ID parser.
var credentialScopeValues = []string{"llm", "web_search", "image_gen", "integration"}

var (
	_ resource.Resource                = (*BotCredentialAssignmentResource)(nil)
	_ resource.ResourceWithConfigure   = (*BotCredentialAssignmentResource)(nil)
	_ resource.ResourceWithImportState = (*BotCredentialAssignmentResource)(nil)
)

// BotCredentialAssignmentResource manages the assignment of *existing* org
// credentials to a bot. Assignments are scoped (llm / web_search / image_gen /
// integration) and ordered within a scope by an explicit ordinal (priority).
//
// The API's assign endpoint is a *per-scope replace*: it wipes and repopulates
// exactly the scopes named in the request. This resource therefore takes
// *exclusive ownership of the scopes it declares* — a credential assigned to a
// managed scope outside Terraform is removed on the next apply — while leaving
// scopes it does not declare untouched. When an update drops a scope entirely,
// the resource still names that scope in the replace so its links are cleared.
//
// It manages assignment only. It does NOT create credentials: the secret-bearing
// bot-private credential endpoints (which carry a raw api_key/oauth_token) are
// deliberately excluded so secrets never land in Terraform state — credential
// creation belongs in a future write-only/ephemeral resource. The per-link
// Claude Code CLI `model` override (a separate PATCH) is likewise not managed;
// note that re-applying this resource re-issues the per-scope assignment, which
// resets any per-link model override set outside Terraform.
type BotCredentialAssignmentResource struct {
	data *providerData
}

// BotCredentialAssignmentResourceModel maps the
// botyard_bot_credential_assignment schema.
type BotCredentialAssignmentResourceModel struct {
	ID          types.String `tfsdk:"id"`
	BotSlug     types.String `tfsdk:"bot_slug"`
	Credentials types.Set    `tfsdk:"credentials"`
}

// credentialEntryModel maps one element of the `credentials` set.
type credentialEntryModel struct {
	CredentialID types.String `tfsdk:"credential_id"`
	Scope        types.String `tfsdk:"scope"`
	Ordinal      types.Int64  `tfsdk:"ordinal"`
	DefaultModel types.String `tfsdk:"default_model"`
}

// credentialEntry is the plugin-framework-free representation of one assignment,
// used for the write/read reconcile logic so it can be exercised hermetically
// against an httptest server.
type credentialEntry struct {
	CredentialID string
	Scope        string
	Ordinal      int
	DefaultModel *string
}

// credentialEntryObjectType is the object type of a `credentials` set element.
func credentialEntryObjectType() types.ObjectType {
	return types.ObjectType{AttrTypes: map[string]attr.Type{
		"credential_id": types.StringType,
		"scope":         types.StringType,
		"ordinal":       types.Int64Type,
		"default_model": types.StringType,
	}}
}

// NewBotCredentialAssignmentResource is the resource factory registered with the provider.
func NewBotCredentialAssignmentResource() resource.Resource {
	return &BotCredentialAssignmentResource{}
}

// Metadata sets the resource type name.
func (r *BotCredentialAssignmentResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_bot_credential_assignment"
}

// Schema defines the botyard_bot_credential_assignment resource schema.
func (r *BotCredentialAssignmentResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Assigns existing organization credentials to a Botyard bot. Assignments are " +
			"scoped (`llm`, `web_search`, `image_gen`, `integration`) and ordered within a scope by an " +
			"explicit `ordinal` (0 = highest priority, tried first).\n\n" +
			"This resource takes **exclusive ownership of the scopes it declares**: for every scope present " +
			"in `credentials`, any credential assigned to that scope outside Terraform is removed on apply so " +
			"the scope converges on the declared entries. Scopes not present in `credentials` are left " +
			"untouched. Use at most one `botyard_bot_credential_assignment` per bot.\n\n" +
			"It manages assignment of **existing** credentials only. It does **not** create credentials — the " +
			"secret-bearing bot-private credential endpoints are intentionally not modeled so raw API keys and " +
			"OAuth tokens never enter Terraform state. The per-link Claude Code CLI `model` override is also " +
			"not managed; re-applying this resource re-issues the per-scope assignment and resets any per-link " +
			"model override set outside Terraform.\n\n" +
			"Because ownership is per-scope, import IDs carry the scopes to manage: `<bot_slug>` imports every " +
			"scope the bot currently has assignments in, and `<bot_slug>:<scope>[,<scope>...]` imports only the " +
			"listed scopes (each listed scope must currently have at least one assignment — an empty scope " +
			"cannot be owned).",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Resource identifier — the bot slug this assignment manages.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"bot_slug": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Slug of the bot whose credential assignments this resource manages. Changing " +
					"it forces replacement.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"credentials": schema.SetNestedAttribute{
				Required: true,
				MarkdownDescription: "The complete set of credential assignments this resource manages, across one " +
					"or more scopes. Each scope present here is owned exclusively (see the resource description). An " +
					"empty set removes all assignments this resource previously created.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"credential_id": schema.StringAttribute{
							Required:            true,
							MarkdownDescription: "ID of an existing organization credential to assign.",
						},
						"scope": schema.StringAttribute{
							Required: true,
							MarkdownDescription: "What the credential is used for. One of `llm`, `web_search`, " +
								"`image_gen`, `integration`. Must match the credential's own scope.",
							Validators: []validator.String{
								stringvalidator.OneOf(credentialScopeValues...),
							},
						},
						"ordinal": schema.Int64Attribute{
							Required: true,
							MarkdownDescription: "Priority of this credential within its scope (0 = tried first). " +
								"Each (scope, ordinal) pair must be unique.",
							Validators: []validator.Int64{
								int64validator.AtLeast(0),
							},
						},
						"default_model": schema.StringAttribute{
							Optional: true,
							MarkdownDescription: "Preferred model ID for this credential (optional; recommended for " +
								"the `llm` scope).",
						},
					},
				},
			},
		},
	}
}

// Configure receives the shared provider data.
func (r *BotCredentialAssignmentResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *BotCredentialAssignmentResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan BotCredentialAssignmentResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	desired := entriesFromSet(ctx, plan.Credentials, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	// Create replaces exactly the scopes present in config.
	final := applyCredentialAssignment(ctx, r.data.client, r.data.orgID, plan.BotSlug.ValueString(),
		desired, distinctScopes(desired), &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = types.StringValue(plan.BotSlug.ValueString())
	plan.Credentials = entriesToSet(ctx, final, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *BotCredentialAssignmentResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state BotCredentialAssignmentResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	prior := entriesFromSet(ctx, state.Credentials, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	slug := state.BotSlug.ValueString()
	all, status, raw, err := listBotCredentials(ctx, r.data.client, r.data.orgID, slug)
	if err != nil {
		resp.Diagnostics.AddError("Error reading bot credential assignments", err.Error())
		return
	}
	if status == 404 {
		// The bot no longer exists — drop the assignment from state.
		resp.State.RemoveResource(ctx)
		return
	}
	if status != 200 {
		resp.Diagnostics.AddError("Unexpected response reading bot credential assignments",
			fmt.Sprintf("List returned HTTP %d: %s", status, describeAPIError(raw)))
		return
	}
	// Refresh only the scopes this resource manages so credentials in other,
	// unmanaged scopes never appear as drift.
	state.ID = types.StringValue(slug)
	state.Credentials = entriesToSet(ctx, filterByScopes(all, distinctScopes(prior)), &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *BotCredentialAssignmentResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state BotCredentialAssignmentResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	desired := entriesFromSet(ctx, plan.Credentials, &resp.Diagnostics)
	prior := entriesFromSet(ctx, state.Credentials, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	// Replace the union of the newly-declared scopes and the previously-managed
	// scopes, so a scope dropped from config gets cleared (not left orphaned).
	scopes := unionScopes(distinctScopes(desired), distinctScopes(prior))
	final := applyCredentialAssignment(ctx, r.data.client, r.data.orgID, plan.BotSlug.ValueString(),
		desired, scopes, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = types.StringValue(plan.BotSlug.ValueString())
	plan.Credentials = entriesToSet(ctx, final, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *BotCredentialAssignmentResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state BotCredentialAssignmentResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	entries := entriesFromSet(ctx, state.Credentials, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	slug := state.BotSlug.ValueString()
	// Unassign each distinct credential; a credential is single-scope, so one
	// DELETE clears it from the scope it was assigned to.
	for _, cid := range distinctCredentialIDs(entries) {
		status, body, err := r.data.client.UnassignBotCredential(ctx, r.data.orgID, slug, cid)
		if err != nil {
			resp.Diagnostics.AddError("Error deleting bot credential assignment", err.Error())
			return
		}
		switch status {
		case 200, 202, 204, 404:
			// removed, or already gone
		default:
			resp.Diagnostics.AddError("Unexpected response deleting bot credential assignment",
				fmt.Sprintf("Delete of credential %s returned HTTP %d: %s", cid, status, describeAPIError(body)))
			return
		}
	}
}

// ImportState imports an assignment by an ID that carries which scopes the
// resource should own — because ownership is per-scope, the bot slug alone is
// ambiguous. The ID is either "<bot_slug>" (manage every scope the bot currently
// has assignments in) or "<bot_slug>:<scope>[,<scope>...]" (manage only the
// listed scopes). It reads the bot's live assignments, filters to the requested
// scopes, and seeds full state so the first post-import Read is a no-op.
//
// Ownership is represented solely by the `credentials` entries, so a scope with
// zero assignments cannot be held in state — Read would derive no ownership for
// it. An explicit scope that is currently empty is therefore rejected rather
// than silently dropped, keeping import consistent with what configuration can
// express (you own a scope by declaring its credentials).
func (r *BotCredentialAssignmentResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	slug, scopes, err := parseCredentialImportID(req.ID)
	if err != nil {
		resp.Diagnostics.AddError("Invalid import ID", err.Error())
		return
	}
	all, status, raw, lerr := listBotCredentials(ctx, r.data.client, r.data.orgID, slug)
	if lerr != nil {
		resp.Diagnostics.AddError("Error reading bot credential assignments", lerr.Error())
		return
	}
	if status == 404 {
		resp.Diagnostics.AddError("Bot not found",
			fmt.Sprintf("No bot with slug %q exists in this organization.", slug))
		return
	}
	if status != 200 {
		resp.Diagnostics.AddError("Unexpected response reading bot credential assignments",
			fmt.Sprintf("List returned HTTP %d: %s", status, describeAPIError(raw)))
		return
	}
	// A bare slug imports every scope that currently has assignments. An explicit
	// scope list imports only those — but every listed scope must currently have
	// at least one assignment, because an empty scope cannot be represented in
	// state (ownership is carried by the credential entries).
	managed := all
	if scopes != nil {
		if empty := emptyRequestedScopes(all, scopes); len(empty) > 0 {
			resp.Diagnostics.AddError("Cannot import empty credential scope(s)",
				fmt.Sprintf("Scope(s) %s have no assignments on bot %q, so they cannot be represented in "+
					"Terraform state — an empty scope is not owned by this resource. Import only scopes that "+
					"currently have assignments, then manage a scope by declaring its credentials in configuration.",
					strings.Join(empty, ", "), slug))
			return
		}
		managed = filterByScopes(all, scopes)
	}
	model := BotCredentialAssignmentResourceModel{
		ID:          types.StringValue(slug),
		BotSlug:     types.StringValue(slug),
		Credentials: entriesToSet(ctx, managed, &resp.Diagnostics),
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}

// parseCredentialImportID splits an import ID into a bot slug and, optionally,
// the explicit scope list to manage. "<bot_slug>" yields (slug, nil, nil) —
// manage all scopes; "<bot_slug>:llm,web_search" yields the slug plus the listed
// scopes. Scopes are validated against the closed enum so a typo fails the
// import instead of silently importing an empty set.
func parseCredentialImportID(id string) (string, []string, error) {
	format := "import ID must be \"<bot_slug>\" or \"<bot_slug>:<scope>[,<scope>...]\""
	slugPart, scopePart, hasScopes := strings.Cut(id, ":")
	slug := strings.TrimSpace(slugPart)
	if slug == "" {
		return "", nil, fmt.Errorf("%s", format)
	}
	if !hasScopes {
		return slug, nil, nil
	}
	var scopes []string
	for _, s := range strings.Split(scopePart, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !validCredentialScope(s) {
			return "", nil, fmt.Errorf("unknown scope %q in import ID; valid scopes: %s",
				s, strings.Join(credentialScopeValues, ", "))
		}
		scopes = append(scopes, s)
	}
	if len(scopes) == 0 {
		return "", nil, fmt.Errorf("import ID lists no scopes after ':'; %s", format)
	}
	return slug, scopes, nil
}

// validCredentialScope reports whether s is a known credential scope.
func validCredentialScope(s string) bool {
	for _, v := range credentialScopeValues {
		if s == v {
			return true
		}
	}
	return false
}

// applyCredentialAssignment performs the per-scope replace and reads back the
// authoritative assignments for the managed scopes. It issues the PUT only when
// there is a scope to replace, then GETs and returns the entries whose scope is
// one of `desired`'s scopes (the scopes this resource will own after the write).
// It is client-driven (no plugin-framework types) so it can be exercised
// hermetically. Shared by Create and Update.
func applyCredentialAssignment(
	ctx context.Context,
	c *client.ClientWithResponses,
	orgID, botSlug string,
	desired []credentialEntry,
	scopesToReplace []string,
	diags *diag.Diagnostics,
) []credentialEntry {
	if len(scopesToReplace) > 0 {
		entries := make([]client.BotCredentialAssignEntry, 0, len(desired))
		for _, e := range desired {
			entries = append(entries, client.BotCredentialAssignEntry{
				CredentialId: e.CredentialID,
				Scope:        client.CredentialScope(e.Scope),
				Ordinal:      e.Ordinal,
				DefaultModel: copyStrPtr(e.DefaultModel),
			})
		}
		scopes := make([]client.CredentialScope, 0, len(scopesToReplace))
		for _, s := range scopesToReplace {
			scopes = append(scopes, client.CredentialScope(s))
		}
		body, err := json.Marshal(client.BotCredentialAssign{Credentials: entries, Scopes: &scopes})
		if err != nil {
			diags.AddError("Error encoding bot credential assignment", err.Error())
			return nil
		}
		apiResp, err := c.AssignCredentialsV1OrgsOrgIdBotsBotSlugCredentialsPutWithBodyWithResponse(
			ctx, orgID, botSlug, "application/json", bytes.NewReader(body))
		if err != nil {
			diags.AddError("Error assigning bot credentials", err.Error())
			return nil
		}
		if apiResp.JSON201 == nil {
			diags.AddError("Unexpected response assigning bot credentials",
				fmt.Sprintf("Assign returned HTTP %d: %s", apiResp.StatusCode(), describeAPIError(apiResp.Body)))
			return nil
		}
	}

	all, status, raw, err := listBotCredentials(ctx, c, orgID, botSlug)
	if err != nil {
		diags.AddError("Error reading bot credential assignments", err.Error())
		return nil
	}
	if status != 200 {
		diags.AddError("Unexpected response reading bot credential assignments",
			fmt.Sprintf("List returned HTTP %d: %s", status, describeAPIError(raw)))
		return nil
	}
	return filterByScopes(all, distinctScopes(desired))
}

// listBotCredentials GETs the bot's credential assignments (all scopes) and maps
// them to credentialEntry. A non-200 status is returned for the caller to
// interpret (404 = bot gone); err is non-nil only for transport-level failures.
func listBotCredentials(ctx context.Context, c *client.ClientWithResponses, orgID, botSlug string) ([]credentialEntry, int, []byte, error) {
	apiResp, err := c.ListBotCredentialsV1OrgsOrgIdBotsBotSlugCredentialsGetWithResponse(ctx, orgID, botSlug, nil)
	if err != nil {
		return nil, 0, nil, err
	}
	if apiResp.JSON200 == nil {
		return nil, apiResp.StatusCode(), apiResp.Body, nil
	}
	out := make([]credentialEntry, 0, len(*apiResp.JSON200))
	for _, l := range *apiResp.JSON200 {
		out = append(out, credentialEntry{
			CredentialID: l.CredentialId,
			Scope:        string(l.Scope),
			Ordinal:      l.Ordinal,
			DefaultModel: copyStrPtr(l.DefaultModel),
		})
	}
	return out, apiResp.StatusCode(), apiResp.Body, nil
}

// entriesFromSet decodes the `credentials` set into []credentialEntry.
func entriesFromSet(ctx context.Context, set types.Set, diags *diag.Diagnostics) []credentialEntry {
	if set.IsNull() || set.IsUnknown() {
		return nil
	}
	var objs []credentialEntryModel
	diags.Append(set.ElementsAs(ctx, &objs, false)...)
	if diags.HasError() {
		return nil
	}
	out := make([]credentialEntry, 0, len(objs))
	for _, o := range objs {
		e := credentialEntry{
			CredentialID: o.CredentialID.ValueString(),
			Scope:        o.Scope.ValueString(),
			Ordinal:      int(o.Ordinal.ValueInt64()),
		}
		if !o.DefaultModel.IsNull() && !o.DefaultModel.IsUnknown() {
			v := o.DefaultModel.ValueString()
			e.DefaultModel = &v
		}
		out = append(out, e)
	}
	return out
}

// entriesToSet encodes []credentialEntry into the `credentials` set value.
func entriesToSet(ctx context.Context, entries []credentialEntry, diags *diag.Diagnostics) types.Set {
	objs := make([]credentialEntryModel, 0, len(entries))
	for _, e := range entries {
		m := credentialEntryModel{
			CredentialID: types.StringValue(e.CredentialID),
			Scope:        types.StringValue(e.Scope),
			Ordinal:      types.Int64Value(int64(e.Ordinal)),
			DefaultModel: types.StringNull(),
		}
		if e.DefaultModel != nil {
			m.DefaultModel = types.StringValue(*e.DefaultModel)
		}
		objs = append(objs, m)
	}
	set, d := types.SetValueFrom(ctx, credentialEntryObjectType(), objs)
	diags.Append(d...)
	return set
}

// distinctScopes returns the sorted unique scopes present in entries.
func distinctScopes(entries []credentialEntry) []string {
	seen := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		seen[e.Scope] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// unionScopes returns the sorted union of two scope slices.
func unionScopes(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	for _, s := range a {
		seen[s] = struct{}{}
	}
	for _, s := range b {
		seen[s] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// filterByScopes keeps only entries whose scope is in scopes, sorted by
// (scope, ordinal, credential_id) for determinism.
func filterByScopes(entries []credentialEntry, scopes []string) []credentialEntry {
	keep := make(map[string]struct{}, len(scopes))
	for _, s := range scopes {
		keep[s] = struct{}{}
	}
	out := make([]credentialEntry, 0, len(entries))
	for _, e := range entries {
		if _, ok := keep[e.Scope]; ok {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Scope != out[j].Scope {
			return out[i].Scope < out[j].Scope
		}
		if out[i].Ordinal != out[j].Ordinal {
			return out[i].Ordinal < out[j].Ordinal
		}
		return out[i].CredentialID < out[j].CredentialID
	})
	return out
}

// emptyRequestedScopes returns the requested scopes (sorted) that have no
// assignment in entries — i.e. scopes that cannot be represented in state.
func emptyRequestedScopes(entries []credentialEntry, scopes []string) []string {
	present := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		present[e.Scope] = struct{}{}
	}
	var empty []string
	for _, s := range scopes {
		if _, ok := present[s]; !ok {
			empty = append(empty, s)
		}
	}
	sort.Strings(empty)
	return empty
}

// distinctCredentialIDs returns the sorted unique credential IDs in entries.
func distinctCredentialIDs(entries []credentialEntry) []string {
	seen := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		seen[e.CredentialID] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// copyStrPtr returns a copy of a *string so callers never alias a pointer into
// a decoded response/loop variable.
func copyStrPtr(s *string) *string {
	if s == nil {
		return nil
	}
	v := *s
	return &v
}
