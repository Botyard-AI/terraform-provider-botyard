package provider

import (
	"encoding/json"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

// The `tool_pinning` leaf of `native_config` models ToolPinningConfigPatch: which
// tools a native bot keeps loaded instead of deferring them behind tool search.
// Only native bots carry it, so it lives under `native_config` alone.

// defaultPinnedToolsLimit is the API's default `limit` (ToolPinningConfig).
// A response that omits `limit` means the default, not "unknown".
const defaultPinnedToolsLimit = 20

// botToolPinningModel mirrors ToolPinningConfigPatch.
type botToolPinningModel struct {
	Limit       types.Int64 `tfsdk:"limit"`
	NeverPin    types.List  `tfsdk:"never_pin"`
	OwnerPins   types.List  `tfsdk:"owner_pins"`
	TokenBudget types.Int64 `tfsdk:"token_budget"`
}

// toolPinningSchemaAttribute builds the `tool_pinning` nested attribute.
//
// Every leaf is Optional+Computed with the nested UseStateForUnknown, the same
// as tool_search: the API's target fields are non-nullable with defaults, so a
// null means "no change" on the wire, and the server's values must be able to
// land in state. Removing the block does NOT reset anything.
func toolPinningSchemaAttribute() schema.SingleNestedAttribute {
	return schema.SingleNestedAttribute{
		Optional: true,
		MarkdownDescription: "Which tools this bot keeps loaded in every conversation instead of deferring them " +
			"behind tool search. Native bots only (`harness = \"botyard_native\"`). Removing this block does NOT " +
			"reset the setting: a patch that omits a field means \"no change\", so the last applied values " +
			"persist. Set the fields explicitly to change them; `token_budget = 0` clears the budget and " +
			"`never_pin = []` / `owner_pins = []` clear the lists.",
		Attributes: map[string]schema.Attribute{
			"limit": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "How many tools each conversation keeps loaded (0 to 50). `0` turns pinning " +
					"off for this bot. Owner pins count toward it. The platform default is 20.",
				Validators:    []validator.Int64{int64validator.Between(0, 50)},
				PlanModifiers: []planmodifier.Int64{nestedUseStateForUnknownInt64()},
			},
			"owner_pins": schema.ListAttribute{
				Optional:    true,
				Computed:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Tools always kept loaded, ahead of the usage ranking (at most `limit`, and at " +
					"most 50). A tool name this bot cannot call is ignored until that tool is assigned. Duplicates " +
					"are removed by the platform.",
				Validators: []validator.List{
					listvalidator.SizeAtMost(50),
					listvalidator.ValueStringsAre(stringvalidator.LengthBetween(1, 100)),
				},
				PlanModifiers: []planmodifier.List{nestedUseStateForUnknownList()},
			},
			"never_pin": schema.ListAttribute{
				Optional:    true,
				Computed:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Tools never kept loaded, whatever their usage (at most 200). They stay reachable " +
					"through tool search. A name may not also be an owner pin.",
				Validators: []validator.List{
					listvalidator.SizeAtMost(200),
					listvalidator.ValueStringsAre(stringvalidator.LengthBetween(1, 100)),
				},
				PlanModifiers: []planmodifier.List{nestedUseStateForUnknownList()},
			},
			"token_budget": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Most tokens the kept-loaded tools' schemas may take together: `1000` to " +
					"`100000`, or `0` for no budget (the default, count only). Owner pins are always kept and use " +
					"the budget first; usage-ranked tools fill the rest, best first, skipping any that do not fit. " +
					"Sizes are an estimate (o200k_base).",
				Validators: []validator.Int64{
					int64validator.Any(
						int64validator.OneOf(0),
						int64validator.Between(1000, 100000),
					),
				},
				PlanModifiers: []planmodifier.Int64{nestedUseStateForUnknownInt64()},
			},
		},
	}
}

// buildToolPinningPatch renders a declared tool_pinning block sparsely: only
// known, non-null leaves are sent, so an unset leaf is left unchanged by the
// server merge. An explicit `token_budget = 0` is sent, because it clears the
// budget.
func buildToolPinningPatch(tp *botToolPinningModel) (json.RawMessage, bool) {
	if tp == nil {
		return nil, false
	}
	inner := map[string]json.RawMessage{}
	putInt64(inner, "limit", tp.Limit)
	putInt64(inner, "token_budget", tp.TokenBudget)
	putStrList(inner, "never_pin", tp.NeverPin)
	putStrList(inner, "owner_pins", tp.OwnerPins)
	if len(inner) == 0 {
		return nil, false
	}
	return marshalObj(inner), true
}

// putStrList adds a list leaf to a sparse patch map. A null or unknown list is
// omitted (no change); a known empty list is sent as [], which clears it.
func putStrList(m map[string]json.RawMessage, key string, v types.List) {
	if v.IsNull() || v.IsUnknown() {
		return
	}
	out := make([]string, 0, len(v.Elements()))
	for _, e := range v.Elements() {
		s, ok := e.(types.String)
		if !ok || s.IsNull() || s.IsUnknown() {
			// Config values are known at apply time; never send a guessed name.
			return
		}
		out = append(out, s.ValueString())
	}
	if b, err := json.Marshal(out); err == nil {
		m[key] = b
	}
}

// mapToolPinning refreshes a declared tool_pinning block. An undeclared block
// stays nil rather than materializing server defaults. Inside a declared block,
// an absent response field means the API's default: `limit` 20, `token_budget`
// 0 (no budget), and an empty list for each list. That keeps a re-plan clean
// whether the server omits a default or sends it.
func mapToolPinning(server *client.ToolPinningConfig, tp *botToolPinningModel) {
	if tp == nil {
		return
	}
	var sv client.ToolPinningConfig
	if server != nil {
		sv = *server
	}
	limit := int64(defaultPinnedToolsLimit)
	if sv.Limit != nil {
		limit = int64(*sv.Limit)
	}
	budget := int64(0)
	if sv.TokenBudget != nil {
		budget = int64(*sv.TokenBudget)
	}
	tp.Limit = types.Int64Value(limit)
	tp.TokenBudget = types.Int64Value(budget)
	tp.NeverPin = stringsToList(derefStrings(sv.NeverPin))
	tp.OwnerPins = stringsToList(derefStrings(sv.OwnerPins))
}

// derefStrings returns the list behind a pointer, or an empty list for nil.
func derefStrings(p *[]string) []string {
	if p == nil {
		return []string{}
	}
	return *p
}

// stringsToList builds a known, non-null list of strings. An empty input is an
// empty list, not null, so a declared `never_pin = []` reads back unchanged.
func stringsToList(in []string) types.List {
	elems := make([]attr.Value, 0, len(in))
	for _, s := range in {
		elems = append(elems, types.StringValue(s))
	}
	return types.ListValueMust(types.StringType, elems)
}
