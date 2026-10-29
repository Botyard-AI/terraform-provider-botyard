package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// The framework's UseStateForUnknown only asks whether the RESOURCE has prior
// state. For a leaf inside a nested object that is newly declared on an
// existing resource, the leaf's prior value is the null of its absent parent,
// so the stock modifier plans null for it. The server then fills in its
// default and Terraform core rejects the apply with "Provider produced
// inconsistent result after apply: ... was null, but now ...". Released
// v0.6.0 does this when `native_config.tool_search`, or a `config` sub-block
// such as `compaction`, is added to a bot that already exists. Creating a bot
// with the block was never affected, because there is no prior state.
//
// nestedUseStateForUnknown behaves exactly like UseStateForUnknown, except
// that it leaves the value unknown when the leaf's parent object is null in
// prior state. A leaf directly on the resource root is unaffected.

type nestedUseStateForUnknown struct{}

func nestedUseStateForUnknownString() planmodifier.String { return nestedUseStateForUnknown{} }
func nestedUseStateForUnknownInt64() planmodifier.Int64   { return nestedUseStateForUnknown{} }
func nestedUseStateForUnknownBool() planmodifier.Bool     { return nestedUseStateForUnknown{} }

func (nestedUseStateForUnknown) Description(context.Context) string {
	return "Once set, the value of this attribute in state will not change, unless its enclosing object is new."
}

func (m nestedUseStateForUnknown) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

// keepPrior reports whether the planned unknown should be replaced with the
// prior state value.
func (nestedUseStateForUnknown) keepPrior(ctx context.Context, p path.Path, state tfsdk.State, planUnknown, configUnknown bool) bool {
	if state.Raw.IsNull() || !planUnknown || configUnknown {
		return false
	}
	parent := p.ParentPath()
	if len(parent.Steps()) == 0 {
		return true
	}
	var obj types.Object
	if diags := state.GetAttribute(ctx, parent, &obj); diags.HasError() {
		// Unreadable parent: stay unknown. An extra "known after apply" is
		// harmless; a wrong known value fails the apply.
		return false
	}
	return !obj.IsNull() && !obj.IsUnknown()
}

func (m nestedUseStateForUnknown) PlanModifyString(ctx context.Context, req planmodifier.StringRequest, resp *planmodifier.StringResponse) {
	if m.keepPrior(ctx, req.Path, req.State, req.PlanValue.IsUnknown(), req.ConfigValue.IsUnknown()) {
		resp.PlanValue = req.StateValue
	}
}

func (m nestedUseStateForUnknown) PlanModifyInt64(ctx context.Context, req planmodifier.Int64Request, resp *planmodifier.Int64Response) {
	if m.keepPrior(ctx, req.Path, req.State, req.PlanValue.IsUnknown(), req.ConfigValue.IsUnknown()) {
		resp.PlanValue = req.StateValue
	}
}

func (m nestedUseStateForUnknown) PlanModifyBool(ctx context.Context, req planmodifier.BoolRequest, resp *planmodifier.BoolResponse) {
	if m.keepPrior(ctx, req.Path, req.State, req.PlanValue.IsUnknown(), req.ConfigValue.IsUnknown()) {
		resp.PlanValue = req.StateValue
	}
}
