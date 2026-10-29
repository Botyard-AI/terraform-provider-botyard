package provider

import (
	"encoding/json"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

// Tool Search is the one tool setting both config shapes carry: the API has
// the same ToolSearchConfigPatch on OpenClawConfigPatch and NativeConfigPatch,
// and the same ToolSearchConfig on both responses. `config.tool_search` and
// `native_config.tool_search` therefore share one schema, one patch builder
// and one mapper, so the two blocks cannot drift apart.

// botToolSearchModel mirrors ToolSearchConfigPatch.
type botToolSearchModel struct {
	Mode                 types.String `tfsdk:"mode"`
	AutoThresholdPercent types.Int64  `tfsdk:"auto_threshold_percent"`
}

// toolSearchSchemaAttribute builds the `tool_search` nested attribute.
//
// The leaves are Optional+Computed with UseStateForUnknown, unlike
// tool_images: the API's target fields are NON-nullable with defaults, so
// there is no "reset to default" a null could express, and the server's
// merged value must be able to land in state.
func toolSearchSchemaAttribute() schema.SingleNestedAttribute {
	return schema.SingleNestedAttribute{
		Optional: true,
		MarkdownDescription: "Tool Search: defer tool schemas out of the prompt behind a search surface instead " +
			"of inlining every schema on every turn. Removing this block does NOT reset the setting: the " +
			"API's target field is non-nullable with a default, so a patch that omits it means \"no change\" " +
			"and the last applied values persist. Set the fields explicitly to change them.",
		Attributes: map[string]schema.Attribute{
			"mode": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "`always` (defer every turn), `auto` (defer above the threshold), or " +
					"`off` (never defer). The platform default is `auto`.",
				Validators:    []validator.String{stringvalidator.OneOf("always", "auto", "off")},
				PlanModifiers: []planmodifier.String{nestedUseStateForUnknownString()},
			},
			"auto_threshold_percent": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Percentage (0 to 100) of the context window the tool schemas must exceed " +
					"before `auto` defers them. Ignored unless `mode` is `auto`, and read only by the " +
					"Claude Code runtime; OpenClaw's embedded Tool Search has no threshold, so `auto` " +
					"behaves like `always` there.",
				Validators:    []validator.Int64{int64validator.Between(0, 100)},
				PlanModifiers: []planmodifier.Int64{nestedUseStateForUnknownInt64()},
			},
		},
	}
}

// buildToolSearchPatch renders a declared tool_search block sparsely: only
// known, non-null leaves are sent, because a null target field is not
// representable and an omitted one is left unchanged.
func buildToolSearchPatch(ts *botToolSearchModel) (json.RawMessage, bool) {
	if ts == nil {
		return nil, false
	}
	inner := map[string]json.RawMessage{}
	putStr(inner, "mode", ts.Mode)
	putInt64(inner, "auto_threshold_percent", ts.AutoThresholdPercent)
	if len(inner) == 0 {
		return nil, false
	}
	return marshalObj(inner), true
}

// mapToolSearch refreshes a declared tool_search block. An undeclared block
// stays nil rather than materializing server defaults, and a response without
// tool_search (an older API) leaves the planned values in place.
func mapToolSearch(server *client.ToolSearchConfig, ts *botToolSearchModel) {
	if ts == nil || server == nil {
		return
	}
	ts.Mode = enumPtrToStr(server.Mode)
	ts.AutoThresholdPercent = intPtrToInt64(server.AutoThresholdPercent)
}

// putStrOrNull / putInt64OrNull / putBoolOrNull add a key for every known
// leaf, sending an explicit JSON null for a null leaf. Use them only for API
// fields where null means "reset to the platform default"; elsewhere use the
// sparse putStr / putInt64 / putBool, where an omitted key means "no change".
// An unknown leaf is omitted either way.
func putStrOrNull(m map[string]json.RawMessage, key string, v types.String) {
	if v.IsNull() {
		m[key] = json.RawMessage("null")
		return
	}
	putStr(m, key, v)
}

func putInt64OrNull(m map[string]json.RawMessage, key string, v types.Int64) {
	if v.IsNull() {
		m[key] = json.RawMessage("null")
		return
	}
	putInt64(m, key, v)
}

func putBoolOrNull(m map[string]json.RawMessage, key string, v types.Bool) {
	if v.IsNull() {
		m[key] = json.RawMessage("null")
		return
	}
	putBool(m, key, v)
}
