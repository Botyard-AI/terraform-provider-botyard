package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// runStringPlanModifiers replays the framework's own modifier chain for one
// string attribute: modifiers run in slice order, and each receives the
// previous one's PlanValue (see fwserver.AttributeModifyPlan, which does
// `planModifyReq.PlanValue = planModifyResp.PlanValue`). Replaying it here lets
// us assert the ORDERING of the real modifiers on the real schema, which is a
// property no amount of build/vet/lint can catch.
func runStringPlanModifiers(
	t *testing.T,
	attrName string,
	configValue, planValue, stateValue types.String,
) bool {
	t.Helper()

	schemaResp := &resource.SchemaResponse{}
	(&BotResource{}).Schema(context.Background(), resource.SchemaRequest{}, schemaResp)
	if schemaResp.Diagnostics.HasError() {
		t.Fatalf("schema: %v", schemaResp.Diagnostics.Errors())
	}
	attr, ok := schemaResp.Schema.Attributes[attrName].(schema.StringAttribute)
	if !ok {
		t.Fatalf("%s is not a StringAttribute", attrName)
	}

	// RequiresReplace short-circuits on a null State (create) or a null Plan
	// (destroy), so both raws must be non-null: this case is an UPDATE.
	objType := schemaResp.Schema.Type().TerraformType(context.Background())
	req := planmodifier.StringRequest{
		ConfigValue: configValue,
		PlanValue:   planValue,
		StateValue:  stateValue,
		Plan:        tfsdk.Plan{Raw: nonNullObject(objType), Schema: schemaResp.Schema},
		State:       tfsdk.State{Raw: nonNullObject(objType), Schema: schemaResp.Schema},
	}

	requiresReplace := false
	for _, m := range attr.PlanModifiers {
		resp := &planmodifier.StringResponse{PlanValue: req.PlanValue}
		m.PlanModifyString(context.Background(), req, resp)
		req.PlanValue = resp.PlanValue
		// The framework appends and never unsets, so replacement latches.
		requiresReplace = requiresReplace || resp.RequiresReplace
	}
	return requiresReplace
}

// nonNullObject builds a non-null raw object value of the given type, standing
// in for "this is an update, not a create or destroy".
func nonNullObject(objType tftypes.Type) tftypes.Value {
	obj, ok := objType.(tftypes.Object)
	if !ok {
		return tftypes.Value{}
	}
	attrs := make(map[string]tftypes.Value, len(obj.AttributeTypes))
	for name, at := range obj.AttributeTypes {
		attrs[name] = tftypes.NewValue(at, nil)
	}
	return tftypes.NewValue(obj, attrs)
}

// REGRESSION: `hosting_type` and `harness` are Optional+Computed AND immutable.
// For every bot that predates these attributes — i.e. every bot currently under
// management — the practitioner has written no value, so Terraform sends the
// plan value as unknown while state holds the refreshed server value.
// RequiresReplace has no unknown guard: it replaces whenever plan != state. If
// it runs before UseStateForUnknown resolves that unknown, an ordinary `plan`
// silently proposes destroying and recreating every bot in the workspace —
// along with its conversations and durable storage.
func TestBotSchema_ImmutableAttrsDoNotReplaceWhenUnconfigured(t *testing.T) {
	for _, attrName := range []string{"hosting_type", "harness"} {
		t.Run(attrName, func(t *testing.T) {
			got := runStringPlanModifiers(t, attrName,
				types.StringNull(),            // practitioner wrote nothing
				types.StringUnknown(),         // so the planned value is unknown
				types.StringValue("openclaw"), // refreshed from the API
			)
			if got {
				t.Fatal("an unconfigured immutable attribute must NOT force replacement; " +
					"UseStateForUnknown must precede RequiresReplace in PlanModifiers")
			}
		})
	}
}

// The flip side: an actual change to a declared value MUST still force replace,
// or the ordering fix would have silently disarmed immutability.
func TestBotSchema_ImmutableAttrsReplaceOnRealChange(t *testing.T) {
	for _, attrName := range []string{"hosting_type", "harness"} {
		t.Run(attrName, func(t *testing.T) {
			got := runStringPlanModifiers(t, attrName,
				types.StringValue("botyard_native"),
				types.StringValue("botyard_native"),
				types.StringValue("openclaw"),
			)
			if !got {
				t.Fatal("changing a declared immutable attribute must force replacement")
			}
		})
	}
}
