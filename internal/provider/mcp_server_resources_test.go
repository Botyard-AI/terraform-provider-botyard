package provider

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

func cpuQ(s string) quantityValue {
	return quantityValue{StringValue: basetypes.NewStringValue(s), kind: quantityCPU}
}

func bytesQ(s string) quantityValue {
	return quantityValue{StringValue: basetypes.NewStringValue(s), kind: quantityBytes}
}

func unknownQ(kind quantityKind) quantityValue {
	return quantityValue{StringValue: basetypes.NewStringUnknown(), kind: kind}
}

// The platform default shape, as the API reads it back.
func defaultShape() *mcpServerResourcesModel {
	return &mcpServerResourcesModel{
		CPURequest:    cpuQ("100m"),
		CPULimit:      cpuQ("500m"),
		MemoryRequest: bytesQ("128Mi"),
		MemoryLimit:   bytesQ("512Mi"),
		Storage:       bytesQ("1Gi"),
	}
}

func effective(pairs ...string) client.ResourceQuantities {
	items := []client.ResourceQuantity{}
	for i := 0; i+1 < len(pairs); i += 2 {
		items = append(items, client.ResourceQuantity{
			ResourceType: client.MeteredResource(pairs[i]), Quantity: pairs[i+1],
		})
	}
	return client.ResourceQuantities{Resources: &items}
}

func decodeItems(t *testing.T, raw json.RawMessage) map[string]string {
	t.Helper()
	var rq client.ResourceQuantities
	if err := json.Unmarshal(raw, &rq); err != nil {
		t.Fatalf("decode resources: %v (%s)", err, raw)
	}
	out := map[string]string{}
	if rq.Resources != nil {
		for _, it := range *rq.Resources {
			out[string(it.ResourceType)] = it.Quantity
		}
	}
	return out
}

// --- quantity parsing mirrors the API ---------------------------------------

func TestParseQuantity_MirrorsAPI(t *testing.T) {
	cases := []struct {
		kind quantityKind
		in   string
		want int64
	}{
		{quantityCPU, "500m", 500},
		{quantityCPU, "2", 2000},
		{quantityCPU, "0.5", 500},
		{quantityCPU, "2000m", 2000},
		{quantityCPU, "1.5m", 1}, // Python int() truncates
		{quantityBytes, "512Mi", 512 << 20},
		{quantityBytes, "2Gi", 2 << 30},
		{quantityBytes, "2048Mi", 2 << 30},
		{quantityBytes, "1G", 1_000_000_000},
		{quantityBytes, "1024", 1024},
		{quantityBytes, "0.5Gi", 512 << 20},
	}
	for _, c := range cases {
		got, ok := parseQuantity(c.kind, c.in)
		if !ok || got != c.want {
			t.Errorf("parseQuantity(%v, %q) = %d, %v; want %d", c.kind, c.in, got, ok, c.want)
		}
	}
}

// "500m" is half a core, but the API's byte parser refuses it; "2Gi" is not a
// CPU quantity. Both must be refused rather than silently misread.
func TestParseQuantity_RefusesCrossKindSpellings(t *testing.T) {
	for _, c := range []struct {
		kind quantityKind
		in   string
	}{
		{quantityBytes, "500m"},
		{quantityCPU, "2Gi"},
		{quantityCPU, "lots"},
		{quantityBytes, ""},
	} {
		if _, ok := parseQuantity(c.kind, c.in); ok {
			t.Errorf("parseQuantity(%v, %q) should fail", c.kind, c.in)
		}
	}
}

// The API returns CPU as millicores and memory in its largest even binary
// suffix. A configured "2" must equal the API's "2000m", or every apply ends in
// "Provider produced inconsistent result".
func TestQuantityValue_SemanticEquals(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		a, b quantityValue
		want bool
	}{
		{cpuQ("2"), cpuQ("2000m"), true},
		{cpuQ("0.5"), cpuQ("500m"), true},
		{cpuQ("1"), cpuQ("500m"), false},
		{bytesQ("2048Mi"), bytesQ("2Gi"), true},
		{bytesQ("1Gi"), bytesQ("1G"), false},
	} {
		got, diags := c.a.StringSemanticEquals(ctx, c.b)
		if diags.HasError() || got != c.want {
			t.Errorf("%q ~ %q = %v, want %v", c.a.ValueString(), c.b.ValueString(), got, c.want)
		}
	}
}

// --- create ------------------------------------------------------------------

func TestResourcesCreateBody_SendsOnlyConfigured(t *testing.T) {
	body := resourcesCreateBody(&mcpServerResourcesModel{
		CPURequest:    unknownQ(quantityCPU),
		CPULimit:      cpuQ("2"),
		MemoryRequest: unknownQ(quantityBytes),
		MemoryLimit:   bytesQ("2Gi"),
		Storage:       bytesQ("10Gi"),
	})
	if body == nil || body.Resources == nil {
		t.Fatal("expected a resources body")
	}
	raw, _ := json.Marshal(body)
	got := decodeItems(t, raw)
	want := map[string]string{"cpu_limit": "2", "memory_limit": "2Gi", "storage": "10Gi"}
	if len(got) != len(want) {
		t.Fatalf("items = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestResourcesCreateBody_UndeclaredOrEmptyIsNil(t *testing.T) {
	if resourcesCreateBody(nil) != nil {
		t.Error("undeclared block must send nothing")
	}
	empty := &mcpServerResourcesModel{
		CPURequest: unknownQ(quantityCPU), CPULimit: unknownQ(quantityCPU),
		MemoryRequest: unknownQ(quantityBytes), MemoryLimit: unknownQ(quantityBytes),
		Storage: unknownQ(quantityBytes),
	}
	if resourcesCreateBody(empty) != nil {
		t.Error("a declared block that sets nothing must send nothing")
	}
}

// The full create body carries the block; an undeclared block keeps the API's
// default (null).
func TestBuildCreateJSON_CarriesResources(t *testing.T) {
	m := containerModel()
	m.Resources = &mcpServerResourcesModel{
		CPURequest: unknownQ(quantityCPU), CPULimit: unknownQ(quantityCPU),
		MemoryRequest: unknownQ(quantityBytes), MemoryLimit: unknownQ(quantityBytes),
		Storage: bytesQ("10Gi"),
	}
	body, diags := buildCreateJSON(context.Background(), m, basetypes.NewStringNull())
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	if got := decodeItems(t, decodeObj(t, body)["resources"]); got["storage"] != "10Gi" || len(got) != 1 {
		t.Errorf("create resources = %v", got)
	}

	body, _ = buildCreateJSON(context.Background(), containerModel(), basetypes.NewStringNull())
	if string(decodeObj(t, body)["resources"]) != "null" {
		t.Errorf("undeclared create resources = %s, want null", decodeObj(t, body)["resources"])
	}
}

// --- update ------------------------------------------------------------------

// REGRESSION GUARD: every resources change restarts the pod, so an apply that
// only renames the server must not resend resources.
func TestResourcesUpdatePatch_UnchangedSendsNothing(t *testing.T) {
	if _, ok := resourcesUpdatePatch(defaultShape(), defaultShape()); ok {
		t.Error("an unchanged block must not put resources in the PATCH")
	}
	// The same quantities spelled differently are not a change either.
	respelled := defaultShape()
	respelled.CPULimit = cpuQ("0.5")
	respelled.Storage = bytesQ("1024Mi")
	if _, ok := resourcesUpdatePatch(respelled, defaultShape()); ok {
		t.Error("a re-spelled but equal quantity must not put resources in the PATCH")
	}
}

func TestResourcesUpdatePatch_SendsOnlyMovedLineItems(t *testing.T) {
	plan := defaultShape()
	plan.MemoryLimit = bytesQ("2Gi")
	plan.Storage = bytesQ("10Gi")
	raw, ok := resourcesUpdatePatch(plan, defaultShape())
	if !ok {
		t.Fatal("moved line-items must be sent")
	}
	got := decodeItems(t, raw)
	if len(got) != 2 || got["memory_limit"] != "2Gi" || got["storage"] != "10Gi" {
		t.Errorf("patch = %v, want only memory_limit and storage", got)
	}
}

func TestResourcesUpdatePatch_RemovedBlockResetsToDefault(t *testing.T) {
	raw, ok := resourcesUpdatePatch(nil, defaultShape())
	if !ok || string(raw) != "null" {
		t.Fatalf("removed block = %s, %v; want explicit null", raw, ok)
	}
	if _, ok := resourcesUpdatePatch(nil, nil); ok {
		t.Error("a never-declared block must not appear in the PATCH")
	}
}

// Declaring the block on a server that had none sends what the config set; the
// unset (unknown) line-items adopt the server's values.
func TestResourcesUpdatePatch_NewlyDeclaredBlock(t *testing.T) {
	plan := &mcpServerResourcesModel{
		CPURequest: unknownQ(quantityCPU), CPULimit: cpuQ("2"),
		MemoryRequest: unknownQ(quantityBytes), MemoryLimit: unknownQ(quantityBytes),
		Storage: unknownQ(quantityBytes),
	}
	raw, ok := resourcesUpdatePatch(plan, nil)
	if !ok {
		t.Fatal("a newly declared block with a set value must be sent")
	}
	if got := decodeItems(t, raw); len(got) != 1 || got["cpu_limit"] != "2" {
		t.Errorf("patch = %v", got)
	}
}

func TestBuildUpdateJSON_ResourcesKeyedOffState(t *testing.T) {
	plan := containerModel()
	plan.Resources = defaultShape()
	plan.Resources.Storage = bytesQ("10Gi")
	body, diags := buildUpdateJSON(context.Background(), plan, plan, defaultShape(), client.McpRuntimeContainerImage)
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	if got := decodeItems(t, decodeObj(t, body)["resources"]); len(got) != 1 || got["storage"] != "10Gi" {
		t.Errorf("update resources = %v", got)
	}

	plan.Resources = defaultShape()
	body, _ = buildUpdateJSON(context.Background(), plan, plan, defaultShape(), client.McpRuntimeContainerImage)
	if _, present := decodeObj(t, body)["resources"]; present {
		t.Error("an unchanged block must not appear in the PATCH")
	}
}

// --- read --------------------------------------------------------------------

func TestMapResources_UndeclaredStaysNil(_ *testing.T) {
	mapResources(effective("cpu_limit", "500m"), nil) // must not panic or allocate
}

func TestMapResources_KeepsConfiguredSpelling(t *testing.T) {
	m := &mcpServerResourcesModel{
		CPURequest: unknownQ(quantityCPU), CPULimit: cpuQ("2"),
		MemoryRequest: unknownQ(quantityBytes), MemoryLimit: bytesQ("2048Mi"),
		Storage: unknownQ(quantityBytes),
	}
	mapResources(effective(
		"cpu_request", "100m", "cpu_limit", "2000m",
		"memory_request", "128Mi", "memory_limit", "2Gi", "storage", "1Gi",
	), m)
	if m.CPULimit.ValueString() != "2" || m.MemoryLimit.ValueString() != "2048Mi" {
		t.Errorf("configured spelling lost: cpu_limit=%q memory_limit=%q",
			m.CPULimit.ValueString(), m.MemoryLimit.ValueString())
	}
	if m.CPURequest.ValueString() != "100m" || m.Storage.ValueString() != "1Gi" {
		t.Errorf("unset line-items must adopt the server's: cpu_request=%q storage=%q",
			m.CPURequest.ValueString(), m.Storage.ValueString())
	}
}

// Drift: someone resized the server in the app. A declared block must surface
// the new value so the plan shows the change.
func TestMapResources_SurfacesDrift(t *testing.T) {
	m := defaultShape()
	mapResources(effective(
		"cpu_request", "100m", "cpu_limit", "1000m",
		"memory_request", "128Mi", "memory_limit", "512Mi", "storage", "1Gi",
	), m)
	if m.CPULimit.ValueString() != "1000m" {
		t.Errorf("cpu_limit = %q, want the drifted 1000m", m.CPULimit.ValueString())
	}
}

func TestMapDetail_ContainerImageMapsDeclaredResources(t *testing.T) {
	d := &client.McpServerDetail{
		RuntimeKind: client.McpRuntimeContainerImage,
		Container: &client.ContainerImageMcpServerDetail{
			Resources: effective("cpu_limit", "2000m", "storage", "10Gi"),
		},
	}
	m := McpServerResourceModel{Resources: &mcpServerResourcesModel{
		CPURequest: unknownQ(quantityCPU), CPULimit: cpuQ("2"),
		MemoryRequest: unknownQ(quantityBytes), MemoryLimit: unknownQ(quantityBytes),
		Storage: unknownQ(quantityBytes),
	}}
	var diags diag.Diagnostics
	mapDetail(context.Background(), d, &m, &diags)
	if m.Resources.CPULimit.ValueString() != "2" || m.Resources.Storage.ValueString() != "10Gi" {
		t.Errorf("resources = %+v", m.Resources)
	}
	// A line-item the API did not return resolves to null, never unknown.
	if !m.Resources.MemoryLimit.IsNull() {
		t.Errorf("memory_limit = %v, want null", m.Resources.MemoryLimit)
	}
}

// --- validation --------------------------------------------------------------

func TestValidate_ResourcesForbiddenOnManagedRemote(t *testing.T) {
	m := managedModel()
	m.Resources = defaultShape()
	if !validateMcpServerConfig(m).HasError() {
		t.Error("resources on a managed_remote server must be refused at plan time")
	}
}

// The API does not yet refuse a request above its limit (PlatDev #4227); the
// pod would then fail to schedule. Refuse it at plan time.
func TestValidate_RequestAboveLimit(t *testing.T) {
	m := containerModel()
	m.Resources = defaultShape()
	m.Resources.CPURequest = cpuQ("1")
	m.Resources.MemoryRequest = bytesQ("1Gi")
	diags := validateMcpServerConfig(m)
	if diags.ErrorsCount() != 2 {
		t.Fatalf("errors = %d (%v), want 2", diags.ErrorsCount(), diags)
	}

	m.Resources = defaultShape()
	m.Resources.CPURequest = cpuQ("0.5")
	if validateMcpServerConfig(m).HasError() {
		t.Error("request equal to limit (0.5 == 500m) must be accepted")
	}
}

// --- schema --------------------------------------------------------------------

func TestMcpServerSchema_HasResourcesBlock(t *testing.T) {
	var resp resource.SchemaResponse
	(&McpServerResource{}).Schema(context.Background(), resource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diags: %v", resp.Diagnostics)
	}
	if _, ok := resp.Schema.Attributes["resources"]; !ok {
		t.Fatal("botyard_mcp_server must expose `resources`")
	}
	if d := resp.Schema.ValidateImplementation(context.Background()); d.HasError() {
		t.Fatalf("schema implementation: %v", d)
	}
}

// onlyAccessChanged gates skipping the PATCH; a resources change must defeat it.
func TestOnlyAccessChanged_ResourcesCount(t *testing.T) {
	plan, state := containerModel(), containerModel()
	plan.Resources, state.Resources = defaultShape(), defaultShape()
	if !onlyAccessChanged(plan, state) {
		t.Fatal("equal resources must not count as a change")
	}
	plan.Resources.Storage = bytesQ("10Gi")
	if onlyAccessChanged(plan, state) {
		t.Error("a resources change must force the PATCH")
	}
}
