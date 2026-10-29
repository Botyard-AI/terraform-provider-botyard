package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

// The `resources` block on botyard_mcp_server: pod compute for a
// container_image server.
//
// The API's two halves are asymmetric, and the block is shaped around that:
//
//   - The read side returns the server's COMPLETE effective resources (the
//     platform default shape until someone customises it).
//   - The write side takes a SPARSE patch (only the line-items being changed),
//     or null to reset the server to the platform default shape. Every change
//     restarts the pod, so an unrelated apply must never resend resources.
//
// So:
//
//   - An undeclared block is null in state and never sent. A server resized in
//     the Botyard app is not clobbered by an unrelated `terraform apply`.
//   - A declared block is managed. Each line-item is Optional+Computed: a set
//     one is sent, and an unset one adopts the server's current value.
//   - Update sends only the line-items whose value moved. If none moved, the
//     PATCH carries no `resources` key and the pod is left alone.
//   - Removing the block resets the server to the platform default (sends
//     null).
//
// The API normalises quantities on the way back: CPU always comes back as
// millicores ("2" -> "2000m"), and memory/storage as the largest binary suffix
// that divides evenly ("2048Mi" -> "2Gi"). The quantity custom type compares in
// base units, so a configured "2" stays "2" in state and never diffs.

// mcpServerResourcesModel maps the `resources` block.
type mcpServerResourcesModel struct {
	CPURequest    quantityValue `tfsdk:"cpu_request"`
	CPULimit      quantityValue `tfsdk:"cpu_limit"`
	MemoryRequest quantityValue `tfsdk:"memory_request"`
	MemoryLimit   quantityValue `tfsdk:"memory_limit"`
	Storage       quantityValue `tfsdk:"storage"`
}

// lineItems returns the block's line-items in the API's canonical order.
func (m *mcpServerResourcesModel) lineItems() []struct {
	resource client.MeteredResource
	value    *quantityValue
} {
	return []struct {
		resource client.MeteredResource
		value    *quantityValue
	}{
		{client.MeteredResourceCpuRequest, &m.CPURequest},
		{client.MeteredResourceCpuLimit, &m.CPULimit},
		{client.MeteredResourceMemoryRequest, &m.MemoryRequest},
		{client.MeteredResourceMemoryLimit, &m.MemoryLimit},
		{client.MeteredResourceStorage, &m.Storage},
	}
}

// resourceQuantityLeaf is one Optional+Computed line-item of a `resources`
// block: set means managed, unset adopts the current value.
//
// The modifier must be the nested variant: with the stock UseStateForUnknown,
// declaring the block on an existing resource planned every unset line-item
// as null (its absent parent's null), and the apply then failed with
// "Provider produced inconsistent result after apply" once the API filled it.
func resourceQuantityLeaf(kind quantityKind, desc string) schema.StringAttribute {
	return schema.StringAttribute{
		CustomType:          quantityType{kind: kind},
		Optional:            true,
		Computed:            true,
		MarkdownDescription: desc,
		Validators:          []validator.String{quantityFormatValidator(kind)},
		PlanModifiers:       []planmodifier.String{nestedUseStateForUnknownString()},
	}
}

func mcpServerResourcesSchemaAttribute() schema.SingleNestedAttribute {
	leaf := resourceQuantityLeaf
	return schema.SingleNestedAttribute{
		Optional: true,
		MarkdownDescription: "Pod compute for a `container_image` server, as Kubernetes quantities. Only valid " +
			"when `runtime_kind = \"container_image\"`.\n\n" +
			"When the block is **omitted**, Terraform does not manage compute: it never sends it, so a size set " +
			"in the Botyard app is left alone. When the block is **declared**, each quantity you set is applied " +
			"over the server's current shape, and the ones you leave out adopt the server's current values. " +
			"Only quantities that changed are sent, because **every change restarts the server's pod**. " +
			"**Removing the block** resets the server to the platform default shape (100m/500m CPU, " +
			"128Mi/512Mi memory, 1Gi scratch); removing a single quantity keeps its current value.\n\n" +
			"Compute is billed like bot compute. The plan's per-MCP-server maximums apply, and sizing above " +
			"the plan's included compute needs a payment method on the organization. Quantities are compared " +
			"by value, so `2` and `2000m` are the same CPU and never produce a diff.",
		Attributes: map[string]schema.Attribute{
			"cpu_request": leaf(quantityCPU, "CPU reserved for the pod, e.g. `100m` or `0.5`. Must not exceed "+
				"`cpu_limit`."),
			"cpu_limit": leaf(quantityCPU, "CPU ceiling for the pod, e.g. `500m` or `2`."),
			"memory_request": leaf(quantityBytes, "Memory reserved for the pod, e.g. `128Mi`. Must not exceed "+
				"`memory_limit`."),
			"memory_limit": leaf(quantityBytes, "Memory ceiling for the pod, e.g. `512Mi` or `2Gi`. At least "+
				"`128Mi`."),
			"storage": leaf(quantityBytes, "Size of the writable scratch volume (`/tmp`, `/var/tmp`, "+
				"`/home/mcp`), e.g. `1Gi` or `10Gi`. Cleared when the pod restarts."),
		},
	}
}

// validateMcpServerResources checks what the API would otherwise refuse, or
// (for request <= limit) accept and fail to schedule. Only compares values that
// are both configured and known; everything else is left to the API.
func validateMcpServerResources(r *mcpServerResourcesModel) diag.Diagnostics {
	var diags diag.Diagnostics
	if r == nil {
		return diags
	}
	pairs := []struct {
		request, limit     quantityValue
		requestAttr, label string
		limitAttr          string
	}{
		{r.CPURequest, r.CPULimit, "cpu_request", "CPU", "cpu_limit"},
		{r.MemoryRequest, r.MemoryLimit, "memory_request", "Memory", "memory_limit"},
	}
	for _, p := range pairs {
		req, okReq := p.request.baseUnits()
		lim, okLim := p.limit.baseUnits()
		if okReq && okLim && req > lim {
			diags.AddAttributeError(path.Root("resources").AtName(p.requestAttr), "Request above limit",
				fmt.Sprintf("`resources.%s` (%s) must not exceed `resources.%s` (%s).",
					p.requestAttr, p.request.ValueString(), p.limitAttr, p.limit.ValueString()))
		}
	}
	return diags
}

// resourcesCreateBody renders the create-time `resources`: every line-item the
// configuration set. Nil (send nothing, take the platform default) when the
// block is undeclared or sets nothing.
func resourcesCreateBody(r *mcpServerResourcesModel) *client.ResourceQuantities {
	if r == nil {
		return nil
	}
	var items []client.ResourceQuantity
	for _, li := range r.lineItems() {
		if li.value.IsNull() || li.value.IsUnknown() {
			continue
		}
		items = append(items, client.ResourceQuantity{ResourceType: li.resource, Quantity: li.value.ValueString()})
	}
	if len(items) == 0 {
		return nil
	}
	return &client.ResourceQuantities{Resources: &items}
}

// resourcesUpdatePatch renders the update-time `resources` value, and whether
// the key belongs in the PATCH at all.
//
//   - Block removed (declared before, not now): explicit null, which resets the
//     server to the platform default shape.
//   - Block declared: the line-items whose planned value differs, by value,
//     from state. An unset line-item plans to its state value
//     (UseStateForUnknown), so it is never sent. Nothing moved means no key,
//     so the pod is not restarted.
//   - Never declared: no key.
func resourcesUpdatePatch(plan, state *mcpServerResourcesModel) (json.RawMessage, bool) {
	if plan == nil {
		if state == nil {
			return nil, false
		}
		return json.RawMessage("null"), true
	}
	var items []client.ResourceQuantity
	planItems := plan.lineItems()
	var stateItems []struct {
		resource client.MeteredResource
		value    *quantityValue
	}
	if state != nil {
		stateItems = state.lineItems()
	}
	for i, li := range planItems {
		if li.value.IsNull() || li.value.IsUnknown() {
			continue
		}
		if stateItems != nil && li.value.sameQuantity(*stateItems[i].value) {
			continue
		}
		items = append(items, client.ResourceQuantity{ResourceType: li.resource, Quantity: li.value.ValueString()})
	}
	if len(items) == 0 {
		return nil, false
	}
	out, err := json.Marshal(client.ResourceQuantities{Resources: &items})
	if err != nil {
		return nil, false
	}
	return out, true
}

// resourcesEqual reports whether two blocks hold the same quantities by value.
// Unknown counts as different, matching onlyAccessChanged's conservatism.
func resourcesEqual(a, b *mcpServerResourcesModel) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	bItems := b.lineItems()
	for i, li := range a.lineItems() {
		x, y := *li.value, *bItems[i].value
		if x.IsUnknown() || y.IsUnknown() {
			return false
		}
		if x.IsNull() != y.IsNull() || (!x.IsNull() && !x.sameQuantity(y)) {
			return false
		}
	}
	return true
}

// mapResources refreshes a declared block from the API's effective resources.
// A nil block is left nil: an undeclared block never enters state, so a server
// sized elsewhere produces no diff.
//
// A line-item already holding the same quantity in another spelling keeps its
// spelling, so a configured "2" is not rewritten to the API's "2000m". The
// framework's semantic equality would do the same for the apply result, but
// doing it here keeps the stored state stable on every path.
func mapResources(effective client.ResourceQuantities, m *mcpServerResourcesModel) {
	if m == nil {
		return
	}
	byType := map[client.MeteredResource]string{}
	if effective.Resources != nil {
		for _, it := range *effective.Resources {
			byType[it.ResourceType] = it.Quantity
		}
	}
	for _, li := range m.lineItems() {
		q, ok := byType[li.resource]
		if !ok {
			*li.value = quantityValue{StringValue: basetypes.NewStringNull(), kind: kindOf(li.resource)}
			continue
		}
		fresh := quantityValue{StringValue: basetypes.NewStringValue(q), kind: kindOf(li.resource)}
		if !li.value.IsNull() && !li.value.IsUnknown() && li.value.sameQuantity(fresh) {
			continue
		}
		*li.value = fresh
	}
}

func kindOf(r client.MeteredResource) quantityKind {
	if r == client.MeteredResourceCpuRequest || r == client.MeteredResourceCpuLimit {
		return quantityCPU
	}
	return quantityBytes
}

// --- quantity custom type ----------------------------------------------------

// quantityKind says how a quantity string parses: CPU (cores or millicores) or
// bytes (binary or decimal suffix). The two must not be conflated: "500m" is
// half a core, not half a byte.
type quantityKind int

const (
	quantityCPU quantityKind = iota
	quantityBytes
)

// Accepted spellings, mirroring what the API's parsers accept for each kind
// (botyard_core.models.bot_resources: parse_cpu_to_millicores,
// parse_k8s_quantity). The API's shared pattern is looser, but a CPU "2Gi" or a
// memory "500m" fails its parser and is refused, so refuse it at plan time.
var (
	cpuQuantityPattern   = regexp.MustCompile(`^\d+(\.\d+)?m?$`)
	bytesQuantityPattern = regexp.MustCompile(`^\d+(\.\d+)?(Ki|Mi|Gi|Ti|Pi|k|M|G|T|P)?$`)
)

func quantityFormatValidator(kind quantityKind) validator.String {
	if kind == quantityCPU {
		return stringvalidator.RegexMatches(cpuQuantityPattern,
			"must be a CPU quantity: cores (`2`, `0.5`) or millicores (`500m`)")
	}
	return stringvalidator.RegexMatches(bytesQuantityPattern,
		"must be a byte quantity with an optional binary (`Ki`..`Pi`) or decimal (`k`..`P`) suffix, e.g. `512Mi`")
}

var byteSuffixes = []struct {
	suffix string
	mult   float64
}{
	// Checked in this order, as the API does: the two-letter binary suffixes
	// first, so "Mi" is never read as "M" followed by junk.
	{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40}, {"Pi", 1 << 50},
	{"k", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12}, {"P", 1e15},
}

// parseQuantity converts a quantity to base units (millicores or bytes) the
// same way the API does, so "equal here" means "equal there". Python's int()
// truncates and round() rounds half to even; both are mirrored.
func parseQuantity(kind quantityKind, s string) (int64, bool) {
	text := strings.TrimSpace(s)
	if text == "" {
		return 0, false
	}
	if kind == quantityCPU {
		if !cpuQuantityPattern.MatchString(text) {
			return 0, false
		}
		if num, ok := strings.CutSuffix(text, "m"); ok {
			f, err := strconv.ParseFloat(num, 64)
			if err != nil {
				return 0, false
			}
			return int64(math.Trunc(f)), true
		}
		f, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return 0, false
		}
		return int64(math.RoundToEven(f * 1000)), true
	}
	if !bytesQuantityPattern.MatchString(text) {
		return 0, false
	}
	for _, bs := range byteSuffixes {
		if num, ok := strings.CutSuffix(text, bs.suffix); ok {
			f, err := strconv.ParseFloat(num, 64)
			if err != nil {
				return 0, false
			}
			return int64(math.Trunc(f * bs.mult)), true
		}
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0, false
	}
	return int64(math.Trunc(f)), true
}

var (
	_ basetypes.StringTypable                    = quantityType{}
	_ basetypes.StringValuableWithSemanticEquals = quantityValue{}
)

// quantityType is a string type whose values compare by quantity.
type quantityType struct {
	basetypes.StringType
	kind quantityKind
}

func (t quantityType) String() string { return "provider.quantityType" }

func (t quantityType) Equal(o attr.Type) bool {
	other, ok := o.(quantityType)
	return ok && other.kind == t.kind
}

func (t quantityType) ValueType(_ context.Context) attr.Value {
	return quantityValue{kind: t.kind}
}

func (t quantityType) ValueFromString(_ context.Context, in basetypes.StringValue) (basetypes.StringValuable, diag.Diagnostics) {
	return quantityValue{StringValue: in, kind: t.kind}, nil
}

func (t quantityType) ValueFromTerraform(ctx context.Context, in tftypes.Value) (attr.Value, error) {
	v, err := t.StringType.ValueFromTerraform(ctx, in)
	if err != nil {
		return nil, err
	}
	sv, ok := v.(basetypes.StringValue)
	if !ok {
		return nil, fmt.Errorf("unexpected value type %T", v)
	}
	out, diags := t.ValueFromString(ctx, sv)
	if diags.HasError() {
		return nil, fmt.Errorf("converting quantity: %v", diags)
	}
	return out, nil
}

// quantityValue is a quantity string that remembers how it parses.
type quantityValue struct {
	basetypes.StringValue
	kind quantityKind
}

func (v quantityValue) Type(_ context.Context) attr.Type { return quantityType{kind: v.kind} }

func (v quantityValue) Equal(o attr.Value) bool {
	other, ok := o.(quantityValue)
	return ok && other.kind == v.kind && v.StringValue.Equal(other.StringValue)
}

// StringSemanticEquals lets the framework keep the configured spelling when the
// API returns the same quantity normalised.
func (v quantityValue) StringSemanticEquals(_ context.Context, newValuable basetypes.StringValuable) (bool, diag.Diagnostics) {
	other, ok := newValuable.(quantityValue)
	if !ok {
		return false, nil
	}
	return v.sameQuantity(other), nil
}

func (v quantityValue) baseUnits() (int64, bool) {
	if v.IsNull() || v.IsUnknown() {
		return 0, false
	}
	return parseQuantity(v.kind, v.ValueString())
}

// sameQuantity reports value equality. Two unparseable strings are equal only
// if they are the same string.
func (v quantityValue) sameQuantity(o quantityValue) bool {
	a, okA := v.baseUnits()
	b, okB := o.baseUnits()
	if okA && okB {
		return a == b
	}
	return v.StringValue.Equal(o.StringValue)
}
