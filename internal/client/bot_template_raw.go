package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// BotTemplateWithRawConfig pairs the generated typed template response with
// the config value exactly as it appeared in the API response. The generated
// OpenClawConfigPatch type cannot preserve explicit nulls or future fields it
// does not model, so callers that expose config as JSON must use RawConfig.
type BotTemplateWithRawConfig struct {
	Template  BotTemplateResponse
	RawConfig json.RawMessage
}

// ListBotTemplatesWithRawConfig lists templates while preserving each config
// value as raw JSON. Non-200 responses are returned to the caller unchanged so
// it can surface the API status and body consistently with generated clients.
func (c *ClientWithResponses) ListBotTemplatesWithRawConfig(
	ctx context.Context, orgID string,
) (templates []BotTemplateWithRawConfig, status int, respBody []byte, err error) {
	resp, err := c.ListBotTemplatesV1OrgsOrgIdBotTemplatesGetWithResponse(ctx, orgID)
	if err != nil {
		return nil, 0, nil, err
	}
	if resp.StatusCode() != http.StatusOK {
		return nil, resp.StatusCode(), resp.Body, nil
	}
	if resp.JSON200 == nil {
		return nil, resp.StatusCode(), resp.Body, fmt.Errorf("decoding bot templates: HTTP 200 response had no JSON body")
	}

	var rawTemplates []map[string]json.RawMessage
	if err := json.Unmarshal(resp.Body, &rawTemplates); err != nil {
		return nil, resp.StatusCode(), resp.Body, fmt.Errorf("decoding raw bot templates: %w", err)
	}
	typedTemplates := *resp.JSON200
	if len(rawTemplates) != len(typedTemplates) {
		return nil, resp.StatusCode(), resp.Body, fmt.Errorf(
			"decoding raw bot templates: got %d raw templates for %d typed templates",
			len(rawTemplates), len(typedTemplates),
		)
	}

	templates = make([]BotTemplateWithRawConfig, 0, len(typedTemplates))
	for i, template := range typedTemplates {
		var rawID string
		if err := json.Unmarshal(rawTemplates[i]["id"], &rawID); err != nil {
			return nil, resp.StatusCode(), resp.Body, fmt.Errorf("decoding raw bot template %d id: %w", i, err)
		}
		if rawID != template.Id {
			return nil, resp.StatusCode(), resp.Body, fmt.Errorf(
				"decoding raw bot template %d: raw id %q does not match typed id %q",
				i, rawID, template.Id,
			)
		}
		templates = append(templates, BotTemplateWithRawConfig{
			Template:  template,
			RawConfig: rawTemplates[i]["config"],
		})
	}
	return templates, resp.StatusCode(), resp.Body, nil
}
