package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

// Runtime settings on botyard_bot (PlatDev #4028b): `auto_suspend_enabled`,
// `runtime_privilege_mode` and `resources`. All three are written through
// PATCH /bots/{slug} (BotUpdate) and read back from BotResponse.
//
//   - Each is sent only when the plan moves it. Privilege mode and resources
//     re-render the pod and bump config_generation, so resending an unchanged
//     value would restart the bot on every unrelated apply.
//   - auto_suspend_enabled and runtime_privilege_mode are Optional+Computed
//     with UseStateForUnknown: omitted means "leave the current value", and an
//     import re-plans clean.
//   - `resources` follows the botyard_mcp_server block (PR 32): undeclared is
//     null and never sent, declared sends only the moved line-items, removing
//     it sends null (reset to the plan defaults). Read back is complete; the
//     quantity type compares in base units.
//   - BotCreate takes `resources` but not the other two, and a pack create
//     takes none of them, so whatever the create call cannot carry is sent in
//     one PATCH right after it (applyRuntimeSettingsAfterCreate).
//
// Server gates (botyard routes/bots.py update_bot): the PATCH is owner-gated
// (require_bot_owner, D6 #4379); `privileged` needs the plan's privileged_mode
// feature and `auto_suspend_enabled = false` its auto_suspend_opt_out feature,
// both refused with 403 `upgrade_required`; storage can only grow.

const (
	autoSuspendEnabledAttr   = "auto_suspend_enabled"
	runtimePrivilegeModeAttr = "runtime_privilege_mode"
	botResourcesAttr         = "resources"
)

func autoSuspendEnabledSchemaAttribute() schema.BoolAttribute {
	return schema.BoolAttribute{
		Optional: true,
		Computed: true,
		MarkdownDescription: "Whether the platform may suspend the bot after a period without human activity. " +
			"When omitted, Terraform reads the current value and leaves it alone.\n\n" +
			"Setting it to `false` requires a plan that allows opting out of auto-suspend; on other plans the " +
			"API refuses the change. This is the owner's stored preference: a plan that enforces auto-suspend " +
			"still suspends the bot.",
		PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
	}
}

func runtimePrivilegeModeSchemaAttribute() schema.StringAttribute {
	return schema.StringAttribute{
		Optional: true,
		Computed: true,
		MarkdownDescription: "Privilege level of the bot runtime pod: `standard` or `privileged`. When omitted, " +
			"Terraform reads the current value and leaves it alone.\n\n" +
			"`privileged` requires a plan with privileged runtimes and moves the bot onto a Kata runtime " +
			"class if it is not on one already. **Changing it restarts the bot.**",
		Validators: []validator.String{stringvalidator.OneOf(
			string(client.BotRuntimePrivilegeModeStandard), string(client.BotRuntimePrivilegeModePrivileged))},
		PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
	}
}

func botResourcesSchemaAttribute() schema.SingleNestedAttribute {
	leaf := resourceQuantityLeaf
	return schema.SingleNestedAttribute{
		Optional: true,
		MarkdownDescription: "Compute for the bot's pod, as Kubernetes quantities, on top of the plan's preset.\n\n" +
			"When the block is **omitted**, Terraform does not manage compute: it never sends it, so a size set " +
			"in the Botyard app is left alone. When the block is **declared**, each quantity you set is applied " +
			"over the bot's current shape, and the ones you leave out adopt the bot's current values. " +
			"Only quantities that changed are sent, because **every change restarts the bot**. " +
			"**Removing the block** resets the bot to the plan's default shape; removing a single quantity " +
			"keeps its current value.\n\n" +
			"The plan's per-bot maximums apply, and sizing above the plan's included resources needs a " +
			"payment method on the organization. `storage` can only grow. Quantities are compared by value, " +
			"so `2` and `2000m` are the same CPU and never produce a diff.\n\n" +
			"Requests follow their limits unless you set them: when `cpu_limit` or `memory_limit` changes and " +
			"the matching request is not configured, the API re-derives the request from the new limit (CPU " +
			"10% of the limit within 250m–1000m; memory 60% with a 1536Mi floor), so the plan shows it as " +
			"known after apply. A request you do set is sent with its limit, so it is kept.",
		Attributes: map[string]schema.Attribute{
			"cpu_request": leaf(quantityCPU, "CPU reserved for the pod, e.g. `500m` or `1`. Must not exceed "+
				"`cpu_limit`."),
			"cpu_limit": leaf(quantityCPU, "CPU ceiling for the pod, e.g. `1` or `2`."),
			"memory_request": leaf(quantityBytes, "Memory reserved for the pod, e.g. `1Gi`. Must not exceed "+
				"`memory_limit`."),
			"memory_limit": leaf(quantityBytes, "Memory ceiling for the pod, e.g. `2Gi`."),
			"storage": leaf(quantityBytes, "Size of the bot's durable workspace volume, e.g. `10Gi`. Can only "+
				"grow: Kubernetes cannot shrink a volume, so the API refuses a smaller value. Removing the "+
				"`resources` block resets the recorded size to the plan's default, but the volume itself is never "+
				"shrunk."),
		},
	}
}

// runtimeSettings is what the configuration asks for, captured before any API
// response is mapped over the plan (mapping would overwrite it).
type runtimeSettings struct {
	autoSuspend types.Bool
	privilege   types.String
	resources   *mcpServerResourcesModel
}

func plannedRuntimeSettings(plan BotResourceModel) runtimeSettings {
	s := runtimeSettings{autoSuspend: plan.AutoSuspendEnabled, privilege: plan.RuntimePrivilegeMode}
	if plan.Resources != nil {
		r := *plan.Resources
		s.resources = &r
	}
	return s
}

// putRuntimeSettingsUpdate adds the runtime settings the plan moves to a
// PATCH /bots/{slug} body. Unknown means "not configured" (UseStateForUnknown
// has already resolved it to state where there is one).
func putRuntimeSettingsUpdate(body map[string]json.RawMessage, plan, state BotResourceModel) {
	if known(plan.AutoSuspendEnabled) && !plan.AutoSuspendEnabled.Equal(state.AutoSuspendEnabled) {
		body[autoSuspendEnabledAttr] = rawBool(plan.AutoSuspendEnabled)
	}
	if known(plan.RuntimePrivilegeMode) && !plan.RuntimePrivilegeMode.Equal(state.RuntimePrivilegeMode) {
		body[runtimePrivilegeModeAttr] = rawString(plan.RuntimePrivilegeMode)
	}
	if patch, ok := botResourcesUpdatePatch(plan.Resources, state.Resources); ok {
		body[botResourcesAttr] = patch
	}
}

// Bot requests are DERIVED from limits. The API overlays a bot's resources
// patch with derive_requests=True (DesiredResources.with_overrides, called by
// bot_provisioning._apply_resource_update): a limit sent without its request
// re-derives the request from the new limit (CPU 10% of the limit within
// 250m-1000m; memory 60% with a 1536Mi floor). MCP servers keep the current
// request instead, so this is bot-only. Two consequences:
//
//   - A request the configuration sets must travel with its limit whenever
//     the limit is sent, even if the request itself did not move, or the API
//     overwrites it (botResourcesUpdatePatch).
//   - A request the configuration leaves out will change when its limit
//     moves, so the plan must not promise the old value (modifyBotResourcesPlan
//     marks it unknown; the response then fills it in).
var derivedRequestPairs = []struct {
	limit, request client.MeteredResource
	limitAttr      string
	requestAttr    string
}{
	{client.MeteredResourceCpuLimit, client.MeteredResourceCpuRequest, "cpu_limit", "cpu_request"},
	{client.MeteredResourceMemoryLimit, client.MeteredResourceMemoryRequest, "memory_limit", "memory_request"},
}

func lineItem(m *mcpServerResourcesModel, r client.MeteredResource) *quantityValue {
	for _, li := range m.lineItems() {
		if li.resource == r {
			return li.value
		}
	}
	return nil
}

// botResourcesUpdatePatch is resourcesUpdatePatch plus the derived-request
// rule: when a limit is sent, its request goes with it if the plan knows it.
// A known planned request is a configured one, because modifyBotResourcesPlan
// has made an unconfigured request unknown whenever its limit moves.
func botResourcesUpdatePatch(plan, state *mcpServerResourcesModel) (json.RawMessage, bool) {
	patch, ok := resourcesUpdatePatch(plan, state)
	if !ok || plan == nil {
		return patch, ok
	}
	var rq client.ResourceQuantities
	if err := json.Unmarshal(patch, &rq); err != nil || rq.Resources == nil {
		return patch, ok
	}
	sent := map[client.MeteredResource]bool{}
	for _, it := range *rq.Resources {
		sent[it.ResourceType] = true
	}
	items := *rq.Resources
	for _, p := range derivedRequestPairs {
		req := lineItem(plan, p.request)
		if sent[p.limit] && !sent[p.request] && known(req) {
			items = append(items, client.ResourceQuantity{ResourceType: p.request, Quantity: req.ValueString()})
		}
	}
	out, err := json.Marshal(client.ResourceQuantities{Resources: &items})
	if err != nil {
		return patch, ok
	}
	return out, true
}

// modifyBotResourcesPlan marks an unconfigured request unknown when its limit
// moves, since the API re-derives it (see derivedRequestPairs). Only an
// existing block needs this: in a newly declared block every unconfigured
// line-item is unknown already.
func modifyBotResourcesPlan(ctx context.Context, plan BotResourceModel, cfg BotResourceModel, state *BotResourceModel, resp *resource.ModifyPlanResponse) {
	if plan.Resources == nil || state == nil || state.Resources == nil {
		return
	}
	for _, p := range derivedRequestPairs {
		limit, prior := lineItem(plan.Resources, p.limit), lineItem(state.Resources, p.limit)
		moved := limit.IsUnknown() || !limit.sameQuantity(*prior)
		configured := cfg.Resources != nil && !lineItem(cfg.Resources, p.request).IsNull()
		if !moved || configured {
			continue
		}
		unknown := quantityValue{StringValue: types.StringUnknown(), kind: kindOf(p.request)}
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root(botResourcesAttr).AtName(p.requestAttr), unknown)...)
	}
}

// runtimeSettingsAfterCreateBody renders the PATCH that settles what the
// create call could not carry: the settings the configuration declares and the
// created bot does not already hold. withResources is set when the create call
// could not take `resources` (a pack create). Nil when nothing is needed.
func runtimeSettingsAfterCreateBody(want runtimeSettings, created *client.BotResponse, withResources bool) ([]byte, error) {
	body := map[string]json.RawMessage{}
	if known(want.autoSuspend) && want.autoSuspend.ValueBool() != created.AutoSuspendEnabled {
		body[autoSuspendEnabledAttr] = rawBool(want.autoSuspend)
	}
	if known(want.privilege) && want.privilege.ValueString() != string(created.RuntimePrivilegeMode) {
		body[runtimePrivilegeModeAttr] = rawString(want.privilege)
	}
	if withResources {
		if rq := resourcesCreateBody(want.resources); rq != nil {
			raw, err := json.Marshal(rq)
			if err != nil {
				return nil, err
			}
			body[botResourcesAttr] = raw
		}
	}
	if len(body) == 0 {
		return nil, nil
	}
	return json.Marshal(body)
}

// applyRuntimeSettingsAfterCreate sends runtimeSettingsAfterCreateBody, if
// any, and returns the bot to map into state: the PATCH response, or the
// created bot when nothing was sent. ok=false means the PATCH failed and a
// diagnostic was recorded; the caller still records the (existing) bot.
func (r *BotResource) applyRuntimeSettingsAfterCreate(ctx context.Context, want runtimeSettings, created *client.BotResponse, withResources bool, diags *diag.Diagnostics) (*client.BotResponse, bool) {
	body, err := runtimeSettingsAfterCreateBody(want, created, withResources)
	if err != nil {
		diags.AddError("Error encoding bot runtime settings", err.Error())
		return created, false
	}
	if body == nil {
		return created, true
	}
	got, ok := r.patchBot(ctx, created.Slug, body, diags)
	if !ok {
		return created, false
	}
	return got, true
}

// patchBot sends PATCH /bots/{slug}. A 403 from the owner gate gets the D6
// diagnostic; anything else non-200 a generic one.
func (r *BotResource) patchBot(ctx context.Context, slug string, body []byte, diags *diag.Diagnostics) (*client.BotResponse, bool) {
	apiResp, err := r.data.client.UpdateBotV1OrgsOrgIdBotsBotSlugPatchWithBodyWithResponse(
		ctx, r.data.orgID, slug, "application/json", bytes.NewReader(body))
	if err != nil {
		diags.AddError("Error updating bot", err.Error())
		return nil, false
	}
	if apiResp.JSON200 == nil {
		if isBotOwnerForbidden(apiResp.StatusCode(), apiResp.Body) {
			addBotOwnerWriteForbiddenError(diags, "update", slug, apiResp.Body)
		} else {
			diags.AddError("Unexpected response updating bot",
				fmt.Sprintf("Update returned HTTP %d: %s", apiResp.StatusCode(), describeAPIError(apiResp.Body)))
		}
		return nil, false
	}
	return apiResp.JSON200, true
}

// checkpointRuntimeSettings records, on a copy of cp, the runtime settings an
// applied PATCH now holds. Update calls it right after the PATCH so that a
// later failure (the config PATCH) does not leave state claiming the old
// values: the next apply would then resend them and restart the bot again.
func checkpointRuntimeSettings(cp BotResourceModel, plan BotResourceModel, b *client.BotResponse) BotResourceModel {
	cp.AutoSuspendEnabled = types.BoolValue(b.AutoSuspendEnabled)
	cp.RuntimePrivilegeMode = types.StringValue(string(b.RuntimePrivilegeMode))
	cp.RuntimeClass = types.StringValue(string(b.RuntimeClass))
	cp.Resources = nil
	if plan.Resources != nil {
		res := *plan.Resources
		cp.Resources = &res
		mapBotResources(b, cp.Resources)
	}
	return cp
}

// liveResources reads the bot's current effective resources as a complete
// block, or nil when the bot has none materialised yet.
func (r *BotResource) liveResources(ctx context.Context, slug string, diags *diag.Diagnostics) (*mcpServerResourcesModel, bool) {
	apiResp, err := r.data.client.GetBotV1OrgsOrgIdBotsBotSlugGetWithResponse(ctx, r.data.orgID, slug)
	if err != nil {
		diags.AddError("Error reading bot", err.Error())
		return nil, false
	}
	if apiResp.JSON200 == nil {
		diags.AddError("Unexpected response reading bot",
			fmt.Sprintf("Read returned HTTP %d: %s", apiResp.StatusCode(), describeAPIError(apiResp.Body)))
		return nil, false
	}
	if apiResp.JSON200.Resources == nil {
		return nil, true
	}
	m := &mcpServerResourcesModel{}
	mapResources(*apiResp.JSON200.Resources, m)
	return m, true
}

// settleUnsentResources records, after a create that failed before
// `resources` was sent, what the bot actually holds: its effective resources
// when the response carried them, otherwise null. A configured quantity that
// never reached the API must not be saved as if it had.
func settleUnsentResources(b *client.BotResponse, m *mcpServerResourcesModel) {
	if m == nil {
		return
	}
	if b.Resources != nil {
		mapResources(*b.Resources, m)
		return
	}
	for _, li := range m.lineItems() {
		*li.value = quantityValue{StringValue: types.StringNull(), kind: kindOf(li.resource)}
	}
}

// mapBotResources refreshes a declared `resources` block from the bot's
// effective resources. A nil response value (a transient, not yet
// materialised bot) leaves the block as it is.
func mapBotResources(b *client.BotResponse, m *mcpServerResourcesModel) {
	if m == nil || b.Resources == nil {
		return
	}
	mapResources(*b.Resources, m)
}

// runtimeClassMayChange reports whether this plan can move runtime_class,
// whose UseStateForUnknown otherwise promises it will not: switching to
// `privileged` puts a non-Kata bot on Kata (bot_provisioning.update_bot).
func runtimeClassMayChange(plan BotResourceModel, state *BotResourceModel) bool {
	return state != nil && known(plan.RuntimePrivilegeMode) &&
		!plan.RuntimePrivilegeMode.Equal(state.RuntimePrivilegeMode)
}

// nullUnknownResources makes a partial create's state valid: a line-item the
// configuration left unset is still unknown when the response carried no
// resources to fill it from.
func nullUnknownResources(m *mcpServerResourcesModel) {
	if m == nil {
		return
	}
	for _, li := range m.lineItems() {
		if li.value.IsUnknown() {
			*li.value = quantityValue{StringValue: types.StringNull(), kind: kindOf(li.resource)}
		}
	}
}

// isBotOwnerForbidden reports whether a response is the bot-owner gate's
// refusal: 403 with the generic `forbidden` problem code. A plan-feature 403
// (`upgrade_required`, e.g. privileged mode) is a different problem with its
// own message and must not be reported as an ownership one.
func isBotOwnerForbidden(status int, body []byte) bool {
	if status != http.StatusForbidden {
		return false
	}
	var pd client.ProblemDetails
	if err := json.Unmarshal(body, &pd); err != nil {
		return false
	}
	return pd.ErrorCode == "forbidden"
}

// addBotOwnerWriteForbiddenError is the D6 (#4379) diagnostic for the
// owner-gated bot writes: PATCH /bots/{slug}, PATCH /config and DELETE.
func addBotOwnerWriteForbiddenError(diags *diag.Diagnostics, action, slug string, body []byte) {
	diags.AddError("Not the bot's owner",
		fmt.Sprintf("Botyard refused to %s bot %q (HTTP 403: %s).\n\n"+
			"Only a bot owner can update or delete a bot. An API key owns only the bots it created itself, "+
			"so it cannot change bots created in the Botyard app or with another key, even if the key's "+
			"organization role is owner. Make the change in the Botyard app, or manage the bot with the key "+
			"that created it.",
			action, slug, describeAPIError(body)))
}

func known(v interface {
	IsNull() bool
	IsUnknown() bool
}) bool {
	return !v.IsNull() && !v.IsUnknown()
}
