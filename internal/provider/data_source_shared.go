package provider

import (
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
)

// configureDataSourceProviderData extracts the shared *providerData handed to
// data sources at Configure time.
//
// It returns nil in two cases: when provider data is not yet available (an
// early Configure call the framework makes before provider configuration
// completes — the framework calls Configure again later with data), and when
// the type assertion fails (a provider bug, surfaced as a diagnostic). Callers
// assign the result to their data field; a nil result simply leaves the data
// source unconfigured until the next call.
func configureDataSourceProviderData(req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) *providerData {
	if req.ProviderData == nil {
		return nil
	}
	data, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Data Source Configure Type",
			fmt.Sprintf("Expected *providerData, got: %T. This is a bug in the provider.", req.ProviderData),
		)
		return nil
	}
	return data
}
