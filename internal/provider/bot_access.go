package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

// setBotAccess PUTs a bot's access mode and returns the updated bot. A refusal
// is reported on the `access` attribute; ok=false means nothing changed.
func (r *BotResource) setBotAccess(ctx context.Context, slug, access string, diags *diag.Diagnostics) (*client.BotResponse, bool) {
	apiResp, err := r.data.client.SetBotAccessV1OrgsOrgIdBotsBotSlugAccessPutWithResponse(
		ctx, r.data.orgID, slug, client.BotAccessUpdate{Access: client.BotAccess(access)})
	if err != nil {
		diags.AddAttributeError(path.Root("access"), "Error setting bot access", err.Error())
		return nil, false
	}
	if apiResp.JSON200 != nil {
		return apiResp.JSON200, true
	}
	if isBotOwnerForbidden(apiResp.StatusCode(), apiResp.Body) {
		diags.AddAttributeError(path.Root("access"), "Not the bot's owner",
			fmt.Sprintf("Botyard refused to change the access mode of bot %q (HTTP 403: %s).\n\n"+
				"Only a bot owner can change its access mode. An API key owns only the bots it created itself, "+
				"so it cannot change bots created in the Botyard app or with another key, even if the key's "+
				"organization role is owner. Make the change in the Botyard app, or manage the bot with the key "+
				"that created it.", slug, describeAPIError(apiResp.Body)))
		return nil, false
	}
	diags.AddAttributeError(path.Root("access"), "Unexpected response setting bot access",
		fmt.Sprintf("PUT /access on bot %q returned HTTP %d: %s", slug, apiResp.StatusCode(),
			describeAPIError(apiResp.Body)))
	return nil, false
}

// applyAccessAfterCreate applies a declared `access` to a bot that was just
// created with the API's default mode. It runs only when the create succeeded
// and recorded the bot in state; on failure the bot stays in state with the
// access mode the server actually holds, and Terraform taints it.
func (r *BotResource) applyAccessAfterCreate(ctx context.Context, desired types.String, resp *resource.CreateResponse) {
	if resp.Diagnostics.HasError() || resp.State.Raw.IsNull() {
		return
	}
	var state BotResourceModel
	resp.Diagnostics.Append(resp.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() || !accessNeedsPut(desired, state.Access) {
		return
	}
	bot, ok := r.setBotAccess(ctx, state.Slug.ValueString(), desired.ValueString(), &resp.Diagnostics)
	if !ok {
		return
	}
	state.Access = types.StringValue(string(bot.Access))
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
