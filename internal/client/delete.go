package client

import (
	"context"
	"io"
	"net/http"
)

// The generated ...DeleteWithResponse parsers json-unmarshal the response body
// whenever the Content-Type contains "json" (their only case decodes
// ProblemDetails). But these DELETE endpoints return 204 No Content on success,
// and the API serves that empty body with a JSON content-type — so the parser
// unmarshals an empty body, fails with "unexpected end of JSON input", and turns
// a successful delete into a spurious transport error. A delete only needs the
// HTTP status, so these wrappers call the raw generated method and read the
// status directly, never parsing the body. The body is still returned so callers
// can surface error detail on a genuinely unexpected status.

// deleteStatus drains and returns the raw DELETE response's status and body
// without parsing it. err is non-nil only for transport-level failures.
func deleteStatus(resp *http.Response, callErr error) (status int, body []byte, err error) {
	if callErr != nil {
		return 0, nil, callErr
	}
	defer func() { _ = resp.Body.Close() }()
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

// DeleteMcpServer deletes an MCP server by ID, returning the HTTP status and raw
// body without body parsing (see the package note above on the empty-204 bug).
func (c *ClientWithResponses) DeleteMcpServer(ctx context.Context, orgID, mcpServerID string) (int, []byte, error) {
	return deleteStatus(c.DeleteMcpServerV1OrgsOrgIdMcpServersMcpServerIdDelete(ctx, orgID, mcpServerID))
}

// DeleteSecretPolicy deletes a secret policy (vault secret) by ID, returning the
// HTTP status and raw body without body parsing (see the package note above).
func (c *ClientWithResponses) DeleteSecretPolicy(ctx context.Context, orgID, policyID string) (int, []byte, error) {
	return deleteStatus(c.DeleteSecretPolicyV1OrgsOrgIdSecretPoliciesPolicyIdDelete(ctx, orgID, policyID))
}

// UnassignBotSkills removes the given skill assignments from a bot in one batch
// request, returning the HTTP status and raw body without body parsing (see the
// package note above). This DELETE takes a BotSkillIds body and returns 204 No
// Content on success — served with a JSON content-type, so the generated
// ...DeleteWithResponse parser would choke on the empty body. Unknown IDs are
// ignored server-side; the endpoint returns 404 only when none of the given
// skills were assigned, which a caller treats as already-gone.
func (c *ClientWithResponses) UnassignBotSkills(ctx context.Context, orgID, botSlug string, skillIDs []string) (int, []byte, error) {
	return deleteStatus(c.UnassignSkillsV1OrgsOrgIdBotsBotSlugSkillsDelete(
		ctx, orgID, botSlug, BotSkillIds{SkillIds: skillIDs}))
}

// UnassignBotTools removes the given tool assignments from a bot in one batch
// request, returning the HTTP status and raw body without body parsing (see the
// package note above). Like the skills DELETE, this takes a BotToolIds body and
// returns 204 No Content with a JSON content-type, which the generated
// ...DeleteWithResponse parser would choke on.
func (c *ClientWithResponses) UnassignBotTools(ctx context.Context, orgID, botSlug string, toolIDs []string) (int, []byte, error) {
	return deleteStatus(c.UnassignToolsV1OrgsOrgIdBotsBotSlugToolsDelete(
		ctx, orgID, botSlug, BotToolIds{ToolIds: toolIDs}))
}

// UnassignBotCredential removes a single credential assignment from a bot,
// returning the HTTP status and raw body without body parsing (see the package
// note above). This DELETE takes the credential ID in the path and returns 204
// No Content on success — served with a JSON content-type, which the generated
// ...DeleteWithResponse parser would choke on. It removes every link for the
// (bot, credential) pair (a credential is single-scope, so this clears it from
// its scope). A 404 means the credential was not assigned, which a caller
// treats as already-gone.
func (c *ClientWithResponses) UnassignBotCredential(ctx context.Context, orgID, botSlug, credentialID string) (int, []byte, error) {
	return deleteStatus(c.UnassignCredentialV1OrgsOrgIdBotsBotSlugCredentialsCredentialIdDelete(
		ctx, orgID, botSlug, credentialID))
}
