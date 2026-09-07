package provider

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

func strp(s string) *string { return &s }
func phm(s string) *client.McpPodHostMode {
	m := client.McpPodHostMode(s)
	return &m
}

// decodeObj unmarshals a JSON object into a raw-message map for key/value assertions.
func decodeObj(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return m
}

func jsonStr(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("json string: %v", err)
	}
	return s
}

// containerModel / managedModel build minimal valid resource models.
func containerModel() McpServerResourceModel {
	return McpServerResourceModel{
		RuntimeKind:      types.StringValue(client.McpRuntimeContainerImage),
		Name:             types.StringValue("My MCP"),
		Slug:             types.StringNull(),
		Description:      types.StringNull(),
		Transport:        types.StringNull(),
		Image:            types.StringValue("ghcr.io/x:1"),
		Port:             types.Int64Value(8080),
		Command:          types.ListNull(types.StringType),
		Args:             types.ListNull(types.StringType),
		EnvPlaintext:     types.MapNull(types.StringType),
		EnvSecretRefs:    types.MapNull(types.StringType),
		SecretFileMounts: types.MapNull(types.StringType),
		PodHostMode:      types.StringValue("pod_localhost"),
		EndpointURL:      types.StringNull(),
		StaticHeaders:    types.MapNull(types.StringType),
		SecretHeaders:    types.MapNull(types.StringType),

		AcknowledgedCredentialHost: types.StringNull(),
	}
}

func managedModel() McpServerResourceModel {
	return McpServerResourceModel{
		RuntimeKind:      types.StringValue(client.McpRuntimeManagedRemote),
		Name:             types.StringValue("Remote"),
		Slug:             types.StringNull(),
		Description:      types.StringNull(),
		Transport:        types.StringNull(),
		Image:            types.StringNull(),
		Port:             types.Int64Null(),
		Command:          types.ListNull(types.StringType),
		Args:             types.ListNull(types.StringType),
		EnvPlaintext:     types.MapNull(types.StringType),
		EnvSecretRefs:    types.MapNull(types.StringType),
		SecretFileMounts: types.MapNull(types.StringType),
		PodHostMode:      types.StringNull(),
		EndpointURL:      types.StringValue("https://example.com/mcp"),
		StaticHeaders:    types.MapNull(types.StringType),
		SecretHeaders:    types.MapNull(types.StringType),

		AcknowledgedCredentialHost: types.StringNull(),
	}
}

// strMap is a small helper for building a known types.Map of strings.
func strMap(t *testing.T, kv map[string]string) types.Map {
	t.Helper()
	m, d := types.MapValueFrom(context.Background(), types.StringType, kv)
	if d.HasError() {
		t.Fatalf("build map: %v", d)
	}
	return m
}

func TestValidateMcpServerConfig(t *testing.T) {
	// happy paths
	if d := validateMcpServerConfig(containerModel()); d.HasError() {
		t.Errorf("valid container config errored: %v", d)
	}
	if d := validateMcpServerConfig(managedModel()); d.HasError() {
		t.Errorf("valid managed config errored: %v", d)
	}

	// container missing image/port
	bad := containerModel()
	bad.Image = types.StringNull()
	bad.Port = types.Int64Null()
	if !validateMcpServerConfig(bad).HasError() {
		t.Error("container without image/port should error")
	}

	// container with endpoint_url (forbidden)
	bad = containerModel()
	bad.EndpointURL = types.StringValue("https://x")
	if !validateMcpServerConfig(bad).HasError() {
		t.Error("container with endpoint_url should error")
	}

	// managed missing endpoint_url
	bad = managedModel()
	bad.EndpointURL = types.StringNull()
	if !validateMcpServerConfig(bad).HasError() {
		t.Error("managed without endpoint_url should error")
	}

	// managed with container-only fields (forbidden)
	bad = managedModel()
	bad.Image = types.StringValue("ghcr.io/x:1")
	if !validateMcpServerConfig(bad).HasError() {
		t.Error("managed with image should error")
	}
	bad = managedModel()
	bad.PodHostMode = types.StringValue("natural")
	if !validateMcpServerConfig(bad).HasError() {
		t.Error("managed with pod_host_mode should error")
	}
}

func TestBuildCreateJSON_ContainerImage(t *testing.T) {
	body, diags := buildCreateJSON(context.Background(), containerModel(), types.StringNull())
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	m := decodeObj(t, body)
	if jsonStr(t, m["runtime_kind"]) != "container_image" || jsonStr(t, m["image"]) != "ghcr.io/x:1" {
		t.Errorf("runtime_kind/image = %s/%s", m["runtime_kind"], m["image"])
	}
	if string(m["port"]) != "8080" {
		t.Errorf("port = %s", m["port"])
	}
	if jsonStr(t, m["pod_host_mode"]) != "pod_localhost" {
		t.Errorf("pod_host_mode = %s", m["pod_host_mode"])
	}
	if _, ok := m["endpoint_url"]; ok {
		t.Error("container create must not carry endpoint_url")
	}
}

func TestBuildCreateJSON_ManagedRemote(t *testing.T) {
	body, diags := buildCreateJSON(context.Background(), managedModel(), types.StringNull())
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	m := decodeObj(t, body)
	if jsonStr(t, m["runtime_kind"]) != "managed_remote" || jsonStr(t, m["endpoint_url"]) != "https://example.com/mcp" {
		t.Errorf("runtime_kind/endpoint = %s/%s", m["runtime_kind"], m["endpoint_url"])
	}
	for _, k := range []string{"image", "port", "pod_host_mode"} {
		if _, ok := m[k]; ok {
			t.Errorf("managed create must not carry %q", k)
		}
	}
}

func TestBuildUpdateJSON_SparsePerKind(t *testing.T) {
	// container update: no endpoint_url key (would be rejected by the API)
	body, diags := buildUpdateJSON(context.Background(), containerModel(), containerModel(), client.McpRuntimeContainerImage)
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	cm := decodeObj(t, body)
	if _, ok := cm["endpoint_url"]; ok {
		t.Error("container update must omit endpoint_url")
	}
	if jsonStr(t, cm["image"]) != "ghcr.io/x:1" || jsonStr(t, cm["pod_host_mode"]) != "pod_localhost" {
		t.Errorf("container update body = %v", cm)
	}

	// managed update: no container-only keys
	body, diags = buildUpdateJSON(context.Background(), managedModel(), managedModel(), client.McpRuntimeManagedRemote)
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	mm := decodeObj(t, body)
	for _, k := range []string{"image", "port", "command", "args", "env_plaintext", "env_secret_refs", "secret_file_mounts", "pod_host_mode"} {
		if _, ok := mm[k]; ok {
			t.Errorf("managed update must omit %q", k)
		}
	}
	if jsonStr(t, mm["endpoint_url"]) != "https://example.com/mcp" {
		t.Errorf("managed update body = %v", mm)
	}
}

func TestMapDetail_ContainerImage(t *testing.T) {
	ts := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	d := &client.McpServerDetail{
		RuntimeKind: client.McpRuntimeContainerImage,
		Container: &client.ContainerImageMcpServerDetail{
			McpServerId:      "m-1",
			OrgId:            "org-1",
			Slug:             "my-mcp",
			Name:             "My MCP",
			Description:      strp("desc"),
			Transport:        client.McpServerTransportStreamableHttp,
			Image:            "ghcr.io/x:1",
			Port:             8080,
			Command:          &[]string{"run"},
			EnvSecretRefs:    &map[string]string{"TOKEN": "vault.token"},
			PodHostMode:      phm("pod_localhost"),
			ToolCount:        3,
			ConfigGeneration: 2,
			DesiredState:     client.McpServerDesiredStateRunning,
			ObservedState:    client.McpServerStateRunning,
			CreatedAt:        ts,
			UpdatedAt:        ts,
		},
	}
	var m McpServerResourceModel
	var diags diag.Diagnostics
	mapDetail(context.Background(), d, &m, &diags)
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	if m.ID.ValueString() != "m-1" || m.Image.ValueString() != "ghcr.io/x:1" || m.Port.ValueInt64() != 8080 {
		t.Errorf("id/image/port = %q/%q/%d", m.ID.ValueString(), m.Image.ValueString(), m.Port.ValueInt64())
	}
	if m.PodHostMode.ValueString() != "pod_localhost" {
		t.Error("pod_host_mode should be mapped from the detail response on container read")
	}
	if !m.EndpointURL.IsNull() {
		t.Error("endpoint_url should be null for container_image")
	}
}

func TestMapDetail_ManagedRemote(t *testing.T) {
	ts := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	d := &client.McpServerDetail{
		RuntimeKind: client.McpRuntimeManagedRemote,
		Managed: &client.ManagedRemoteMcpServerDetail{
			McpServerId:      "m-2",
			OrgId:            "org-1",
			Slug:             "remote",
			Name:             "Remote",
			Transport:        client.McpServerTransportStreamableHttp,
			EndpointUrl:      "https://example.com/mcp",
			ToolCount:        0,
			ConfigGeneration: 1,
			DesiredState:     client.McpServerDesiredStateRunning,
			ObservedState:    client.McpServerStateRunning,
			CreatedAt:        ts,
			UpdatedAt:        ts,
		},
	}
	var m McpServerResourceModel
	var diags diag.Diagnostics
	mapDetail(context.Background(), d, &m, &diags)
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	if m.RuntimeKind.ValueString() != "managed_remote" || m.EndpointURL.ValueString() != "https://example.com/mcp" {
		t.Errorf("kind/endpoint = %q/%q", m.RuntimeKind.ValueString(), m.EndpointURL.ValueString())
	}
	if !m.Image.IsNull() || !m.Port.IsNull() || !m.PodHostMode.IsNull() {
		t.Error("container-only fields (incl pod_host_mode) should be null for managed_remote")
	}
}

// --- managed-remote request headers ------------------------------------------

// TestValidateMcpServerConfig_HeadersAreManagedRemoteOnly proves the provider
// refuses header configuration on a container_image server at plan time, which
// is the same rule the API enforces on create and update.
func TestValidateMcpServerConfig_HeadersAreManagedRemoteOnly(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(*McpServerResourceModel)
	}{
		{"static_headers", func(m *McpServerResourceModel) {
			m.StaticHeaders = strMap(t, map[string]string{"X-Trace": "on"})
		}},
		{"secret_headers", func(m *McpServerResourceModel) {
			m.SecretHeaders = strMap(t, map[string]string{"Authorization": "mcp.posthog.authorization_header"})
		}},
		{"acknowledged_credential_host", func(m *McpServerResourceModel) {
			m.AcknowledgedCredentialHost = types.StringValue("mcp.posthog.com")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := containerModel()
			tc.apply(&bad)
			if !validateMcpServerConfig(bad).HasError() {
				t.Errorf("container_image with %s should error", tc.name)
			}
		})
	}
}

// TestValidateMcpServerConfig_HeadersAllowedOnManagedRemote proves the same
// three arguments are accepted on a managed_remote server.
func TestValidateMcpServerConfig_HeadersAllowedOnManagedRemote(t *testing.T) {
	ok := managedModel()
	ok.StaticHeaders = strMap(t, map[string]string{"X-Trace": "on"})
	ok.SecretHeaders = strMap(t, map[string]string{"Authorization": "mcp.posthog.authorization_header"})
	ok.AcknowledgedCredentialHost = types.StringValue("mcp.posthog.com")
	if d := validateMcpServerConfig(ok); d.HasError() {
		t.Errorf("valid managed_remote header config errored: %v", d)
	}
}

// TestValidateMcpServerConfig_RejectsPlaintextAuthorization mirrors the server's
// `validate_managed_remote_headers` rule: an Authorization value belongs in
// secret_headers as a vault key path, never in the plaintext map. The match is
// case-insensitive, as it is server-side.
func TestValidateMcpServerConfig_RejectsPlaintextAuthorization(t *testing.T) {
	for _, name := range []string{"Authorization", "authorization", "AUTHORIZATION"} {
		bad := managedModel()
		bad.StaticHeaders = strMap(t, map[string]string{name: "Bearer hunter2"})
		if !validateMcpServerConfig(bad).HasError() {
			t.Errorf("static_headers %q should be rejected", name)
		}
	}
	// A vault-backed Authorization in secret_headers is the supported shape.
	ok := managedModel()
	ok.SecretHeaders = strMap(t, map[string]string{"Authorization": "mcp.posthog.authorization_header"})
	if d := validateMcpServerConfig(ok); d.HasError() {
		t.Errorf("vault-backed Authorization errored: %v", d)
	}
	// Any other static header name is none of the provider's business.
	ok = managedModel()
	ok.StaticHeaders = strMap(t, map[string]string{"X-Api-Version": "2026-09-01"})
	if d := validateMcpServerConfig(ok); d.HasError() {
		t.Errorf("ordinary static header errored: %v", d)
	}
}

// TestBuildCreateJSON_ManagedRemoteHeaders proves headers and the write-only
// acknowledgement reach the create body, and that the acknowledgement is taken
// from the config argument rather than from the (always-null) plan field.
func TestBuildCreateJSON_ManagedRemoteHeaders(t *testing.T) {
	plan := managedModel()
	plan.StaticHeaders = strMap(t, map[string]string{"X-Api-Version": "2026-09-01"})
	plan.SecretHeaders = strMap(t, map[string]string{"Authorization": "mcp.posthog.authorization_header"})

	body, diags := buildCreateJSON(context.Background(), plan, types.StringValue("mcp.posthog.com"))
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	m := decodeObj(t, body)
	static := decodeObj(t, m["static_headers"])
	if jsonStr(t, static["X-Api-Version"]) != "2026-09-01" {
		t.Errorf("static_headers = %s", m["static_headers"])
	}
	secret := decodeObj(t, m["secret_headers"])
	if jsonStr(t, secret["Authorization"]) != "mcp.posthog.authorization_header" {
		t.Errorf("secret_headers = %s", m["secret_headers"])
	}
	if jsonStr(t, m["acknowledged_credential_host"]) != "mcp.posthog.com" {
		t.Errorf("acknowledged_credential_host = %s", m["acknowledged_credential_host"])
	}
}

// TestBuildCreateJSON_ManagedRemoteOmitsUndeclaredHeaders proves an undeclared
// header map is left out of the create body entirely so the API applies its own
// default, rather than being sent as an explicit null.
func TestBuildCreateJSON_ManagedRemoteOmitsUndeclaredHeaders(t *testing.T) {
	body, diags := buildCreateJSON(context.Background(), managedModel(), types.StringNull())
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	m := decodeObj(t, body)
	for _, k := range []string{"static_headers", "secret_headers"} {
		if _, ok := m[k]; ok {
			t.Errorf("undeclared %q must be omitted from the create body, got %s", k, m[k])
		}
	}
}

// TestBuildUpdateJSON_OmitsUndeclaredHeaders is the anti-clobber test: a
// practitioner who never declared headers must not have the server's headers
// replaced by an unrelated apply. The API treats an omitted field as UNSET and
// an included one as a full replacement, so the key must be absent — not null,
// and not an empty object.
func TestBuildUpdateJSON_OmitsUndeclaredHeaders(t *testing.T) {
	plan, cfg := managedModel(), managedModel()
	// The plan carries values refreshed from the server (Optional+Computed +
	// UseStateForUnknown); the config does not, because nothing was declared.
	plan.StaticHeaders = strMap(t, map[string]string{"X-Set-In-The-App": "1"})
	plan.SecretHeaders = strMap(t, map[string]string{"Authorization": "mcp.posthog.authorization_header"})

	body, diags := buildUpdateJSON(context.Background(), plan, cfg, client.McpRuntimeManagedRemote)
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	m := decodeObj(t, body)
	for _, k := range []string{"static_headers", "secret_headers", "acknowledged_credential_host"} {
		if _, ok := m[k]; ok {
			t.Errorf("undeclared %q must not be sent in a PATCH, got %s", k, m[k])
		}
	}
	// The rest of the sparse PATCH is unaffected.
	if jsonStr(t, m["endpoint_url"]) != "https://example.com/mcp" {
		t.Errorf("endpoint_url = %s", m["endpoint_url"])
	}
}

// TestBuildUpdateJSON_SendsDeclaredHeaders proves declared headers are sent as a
// full replacement, that a declared-but-empty map is sent as {} (the documented
// way to clear headers), and that the write-only acknowledgement is read from
// the config.
func TestBuildUpdateJSON_SendsDeclaredHeaders(t *testing.T) {
	plan, cfg := managedModel(), managedModel()
	headers := strMap(t, map[string]string{"Authorization": "mcp.posthog.authorization_header"})
	empty := strMap(t, map[string]string{})
	plan.SecretHeaders, cfg.SecretHeaders = headers, headers
	plan.StaticHeaders, cfg.StaticHeaders = empty, empty
	cfg.AcknowledgedCredentialHost = types.StringValue("mcp.posthog.com")

	body, diags := buildUpdateJSON(context.Background(), plan, cfg, client.McpRuntimeManagedRemote)
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	m := decodeObj(t, body)
	secret := decodeObj(t, m["secret_headers"])
	if jsonStr(t, secret["Authorization"]) != "mcp.posthog.authorization_header" {
		t.Errorf("secret_headers = %s", m["secret_headers"])
	}
	if string(m["static_headers"]) != "{}" {
		t.Errorf("declared-empty static_headers should clear via {}, got %s", m["static_headers"])
	}
	if jsonStr(t, m["acknowledged_credential_host"]) != "mcp.posthog.com" {
		t.Errorf("acknowledged_credential_host = %s", m["acknowledged_credential_host"])
	}
}

// TestBuildUpdateJSON_ContainerImageNeverSendsHeaders proves the container
// branch cannot emit managed-remote fields even if a model somehow carries them.
func TestBuildUpdateJSON_ContainerImageNeverSendsHeaders(t *testing.T) {
	plan, cfg := containerModel(), containerModel()
	headers := strMap(t, map[string]string{"X-Trace": "on"})
	plan.StaticHeaders, cfg.StaticHeaders = headers, headers
	plan.SecretHeaders, cfg.SecretHeaders = headers, headers
	cfg.AcknowledgedCredentialHost = types.StringValue("example.com")

	body, diags := buildUpdateJSON(context.Background(), plan, cfg, client.McpRuntimeContainerImage)
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	m := decodeObj(t, body)
	for _, k := range []string{"static_headers", "secret_headers", "acknowledged_credential_host", "endpoint_url"} {
		if _, ok := m[k]; ok {
			t.Errorf("container_image update must omit %q, got %s", k, m[k])
		}
	}
}

// TestMapDetail_ManagedRemoteRoundTripsHeaders proves read/import populates both
// header maps, which is what makes a plan clean immediately after import.
func TestMapDetail_ManagedRemoteRoundTripsHeaders(t *testing.T) {
	ts := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	d := &client.McpServerDetail{
		RuntimeKind: client.McpRuntimeManagedRemote,
		Managed: &client.ManagedRemoteMcpServerDetail{
			McpServerId:      "m-3",
			OrgId:            "org-1",
			Slug:             "posthog",
			Name:             "PostHog",
			Transport:        client.McpServerTransportStreamableHttp,
			EndpointUrl:      "https://mcp.posthog.com/mcp",
			StaticHeaders:    &map[string]string{"X-Api-Version": "2026-09-01"},
			SecretHeaders:    &map[string]string{"Authorization": "mcp.posthog.authorization_header"},
			DesiredState:     client.McpServerDesiredStateRunning,
			ObservedState:    client.McpServerStateRunning,
			CreatedAt:        ts,
			UpdatedAt:        ts,
			ConfigGeneration: 1,
		},
	}
	var m McpServerResourceModel
	var diags diag.Diagnostics
	mapDetail(context.Background(), d, &m, &diags)
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	if got := m.StaticHeaders.Elements()["X-Api-Version"]; got.String() != `"2026-09-01"` {
		t.Errorf("static_headers not round-tripped: %v", m.StaticHeaders)
	}
	// The vault key path is read back verbatim: it is a pointer, not a secret,
	// and drift detection depends on it being in state.
	if got := m.SecretHeaders.Elements()["Authorization"]; got.String() != `"mcp.posthog.authorization_header"` {
		t.Errorf("secret_headers not round-tripped: %v", m.SecretHeaders)
	}
}

// TestMapDetail_ContainerImageNullsHeaders proves the container variant nulls
// the managed-remote header maps rather than leaving a stale value behind.
func TestMapDetail_ContainerImageNullsHeaders(t *testing.T) {
	ts := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	d := &client.McpServerDetail{
		RuntimeKind: client.McpRuntimeContainerImage,
		Container: &client.ContainerImageMcpServerDetail{
			McpServerId:   "m-4",
			OrgId:         "org-1",
			Slug:          "local",
			Name:          "Local",
			Transport:     client.McpServerTransportStreamableHttp,
			Image:         "ghcr.io/x:1",
			Port:          8080,
			DesiredState:  client.McpServerDesiredStateRunning,
			ObservedState: client.McpServerStateRunning,
			CreatedAt:     ts,
			UpdatedAt:     ts,
		},
	}
	m := McpServerResourceModel{
		StaticHeaders: strMap(t, map[string]string{"stale": "value"}),
		SecretHeaders: strMap(t, map[string]string{"stale": "value"}),
	}
	var diags diag.Diagnostics
	mapDetail(context.Background(), d, &m, &diags)
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	if !m.StaticHeaders.IsNull() || !m.SecretHeaders.IsNull() {
		t.Error("header maps should be null for container_image")
	}
}
