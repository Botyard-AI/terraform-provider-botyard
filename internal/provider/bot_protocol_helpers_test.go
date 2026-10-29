package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// Helpers for driving botyard_bot through the real tfprotov6 server, the way
// Terraform core does, without a terraform binary.

const botTypeName = "botyard_bot"

func botProtoServer(t *testing.T) tfprotov6.ProviderServer {
	t.Helper()
	srv, err := providerserver.NewProtocol6WithError(New("test")())()
	if err != nil {
		t.Fatalf("provider server: %v", err)
	}
	return srv
}

func botSchema(t *testing.T) schema.Schema {
	t.Helper()
	schemaResp := &resource.SchemaResponse{}
	(&BotResource{}).Schema(context.Background(), resource.SchemaRequest{}, schemaResp)
	if schemaResp.Diagnostics.HasError() {
		t.Fatalf("schema: %v", schemaResp.Diagnostics.Errors())
	}
	return schemaResp.Schema
}

func botObjectType(t *testing.T) tftypes.Object {
	t.Helper()
	obj, ok := botSchema(t).Type().TerraformType(context.Background()).(tftypes.Object)
	if !ok {
		t.Fatal("bot schema is not an object type")
	}
	return obj
}

// objectFromJSON decodes JSON into a value of typ the way Terraform state is
// decoded: absent attributes become null. Unlike the framework's upgrade path
// it does NOT ignore undefined attributes, so a test fixture that names an
// attribute the schema lacks fails loudly instead of being silently dropped.
func objectFromJSON(t *testing.T, typ tftypes.Type, raw string) tftypes.Value {
	t.Helper()
	v, err := tftypes.ValueFromJSONWithOpts([]byte(raw), typ, tftypes.ValueFromJSONOpts{})
	if err != nil {
		t.Fatalf("decoding fixture: %v", err)
	}
	return v
}

// proposedNewState mirrors Terraform core's objchange.ProposedNew for the
// attribute kinds this resource uses: a value set in config wins; a null config
// value inherits the prior value only for a Computed attribute; a declared
// single-nested object is merged attribute by attribute. Building it from
// config and prior state, rather than passing prior state through, is what lets
// the no-op assertion below fail when config and state disagree.
func proposedNewState(t *testing.T, attrs map[string]schema.Attribute, config, prior tftypes.Value) tftypes.Value {
	t.Helper()
	if config.IsNull() {
		return config
	}
	var cfg, old map[string]tftypes.Value
	if err := config.As(&cfg); err != nil {
		t.Fatalf("proposed: config: %v", err)
	}
	if !prior.IsNull() {
		if err := prior.As(&old); err != nil {
			t.Fatalf("proposed: prior: %v", err)
		}
	}
	out := make(map[string]tftypes.Value, len(cfg))
	for name, cv := range cfg {
		pv, hasPrior := old[name]
		if !hasPrior {
			pv = tftypes.NewValue(cv.Type(), nil)
		}
		switch a := attrs[name].(type) {
		case schema.SingleNestedAttribute:
			if cv.IsNull() && a.IsComputed() {
				out[name] = pv
			} else {
				out[name] = proposedNewState(t, a.Attributes, cv, pv)
			}
		default:
			if cv.IsNull() && attrs[name].IsComputed() {
				out[name] = pv
			} else {
				out[name] = cv
			}
		}
	}
	return tftypes.NewValue(config.Type(), out)
}

func dynamicValue(t *testing.T, typ tftypes.Type, v tftypes.Value) *tfprotov6.DynamicValue {
	t.Helper()
	dv, err := tfprotov6.NewDynamicValue(typ, v)
	if err != nil {
		t.Fatalf("dynamic value: %v", err)
	}
	return &dv
}

func requireNoErrorDiags(t *testing.T, step string, diags []*tfprotov6.Diagnostic) {
	t.Helper()
	for _, d := range diags {
		if d.Severity == tfprotov6.DiagnosticSeverityError {
			t.Fatalf("%s: unexpected error diagnostic: %s: %s", step, d.Summary, d.Detail)
		}
	}
}
