package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// Direct tests of nestedUseStateForUnknown, covering the root-level branch
// that no current botyard_bot attribute exercises.
func TestNestedUseStateForUnknown(t *testing.T) {
	ctx := context.Background()
	s := schema.Schema{Attributes: map[string]schema.Attribute{
		"top": schema.StringAttribute{Optional: true, Computed: true},
		"blk": schema.SingleNestedAttribute{Optional: true, Attributes: map[string]schema.Attribute{
			"leaf": schema.StringAttribute{Optional: true, Computed: true},
		}},
	}}
	typ := s.Type().TerraformType(ctx).(tftypes.Object)
	blkTyp := typ.AttributeTypes["blk"]
	state := func(top string, blk tftypes.Value) tfsdk.State {
		return tfsdk.State{Schema: s, Raw: tftypes.NewValue(typ, map[string]tftypes.Value{
			"top": tftypes.NewValue(tftypes.String, top), "blk": blk,
		})}
	}
	withLeaf := tftypes.NewValue(blkTyp, map[string]tftypes.Value{"leaf": tftypes.NewValue(tftypes.String, "prior")})
	noBlk := tftypes.NewValue(blkTyp, nil)

	cases := []struct {
		name  string
		p     path.Path
		state tfsdk.State
		prior types.String
		want  types.String
	}{
		{"root-level leaf keeps prior", path.Root("top"), state("t", noBlk), types.StringValue("t"), types.StringValue("t")},
		{"leaf in existing block keeps prior", path.Root("blk").AtName("leaf"), state("t", withLeaf), types.StringValue("prior"), types.StringValue("prior")},
		{"leaf in new block stays unknown", path.Root("blk").AtName("leaf"), state("t", noBlk), types.StringNull(), types.StringUnknown()},
		{"create stays unknown", path.Root("top"), tfsdk.State{Schema: s, Raw: tftypes.NewValue(typ, nil)}, types.StringNull(), types.StringUnknown()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := planmodifier.StringRequest{Path: tc.p, State: tc.state, StateValue: tc.prior,
				PlanValue: types.StringUnknown(), ConfigValue: types.StringNull()}
			resp := &planmodifier.StringResponse{PlanValue: req.PlanValue}
			nestedUseStateForUnknownString().PlanModifyString(ctx, req, resp)
			if !resp.PlanValue.Equal(tc.want) {
				t.Errorf("planned %s, want %s", resp.PlanValue, tc.want)
			}
		})
	}
}
