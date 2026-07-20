package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// The MCP server create/detail payloads are an OpenAPI `oneOf` discriminated on
// `runtime_kind`. oapi-codegen models these as a struct with an unexported
// `union json.RawMessage` field and does not generate As*/From* helpers, so we
// orchestrate the union here using the generated concrete variant types
// (ContainerImage*/ManagedRemote*): callers marshal a concrete variant and pass
// the bytes; responses are decoded by their `runtime_kind` discriminator.

// McpServerRuntimeContainerImage / ...ManagedRemote are the discriminator values.
const (
	McpRuntimeContainerImage = "container_image"
	McpRuntimeManagedRemote  = "managed_remote"
)

// McpServerDetail is the decoded, kind-tagged detail response. Exactly one of
// Container / Managed is non-nil, per RuntimeKind.
type McpServerDetail struct {
	RuntimeKind string
	Container   *ContainerImageMcpServerDetail
	Managed     *ManagedRemoteMcpServerDetail
}

// DecodeMcpServerDetail unmarshals a raw MCP-server detail body into the
// variant selected by its `runtime_kind` discriminator.
func DecodeMcpServerDetail(body []byte) (*McpServerDetail, error) {
	var disc struct {
		RuntimeKind string `json:"runtime_kind"`
	}
	if err := json.Unmarshal(body, &disc); err != nil {
		return nil, fmt.Errorf("decoding mcp server runtime_kind: %w", err)
	}
	out := &McpServerDetail{RuntimeKind: disc.RuntimeKind}
	switch disc.RuntimeKind {
	case McpRuntimeContainerImage:
		var d ContainerImageMcpServerDetail
		if err := json.Unmarshal(body, &d); err != nil {
			return nil, fmt.Errorf("decoding container_image mcp server: %w", err)
		}
		out.Container = &d
	case McpRuntimeManagedRemote:
		var d ManagedRemoteMcpServerDetail
		if err := json.Unmarshal(body, &d); err != nil {
			return nil, fmt.Errorf("decoding managed_remote mcp server: %w", err)
		}
		out.Managed = &d
	default:
		return nil, fmt.Errorf("unknown mcp server runtime_kind %q", disc.RuntimeKind)
	}
	return out, nil
}

// CreateMcpServer POSTs a pre-marshaled create body (a concrete
// ContainerImageMcpServerCreate or ManagedRemoteMcpServerCreate) and decodes the
// created server on 201. A non-201 status returns a nil detail with the status
// and raw body so callers can surface the error.
func (c *ClientWithResponses) CreateMcpServer(
	ctx context.Context, orgID string, body []byte,
) (detail *McpServerDetail, status int, respBody []byte, err error) {
	resp, err := c.CreateMcpServerV1OrgsOrgIdMcpServersPostWithBodyWithResponse(
		ctx, orgID, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, 0, nil, err
	}
	if resp.StatusCode() != http.StatusCreated {
		return nil, resp.StatusCode(), resp.Body, nil
	}
	detail, err = DecodeMcpServerDetail(resp.Body)
	return detail, resp.StatusCode(), resp.Body, err
}

// UpdateMcpServer PATCHes a pre-marshaled sparse update body and decodes the
// updated server on 200.
func (c *ClientWithResponses) UpdateMcpServer(
	ctx context.Context, orgID, mcpServerID string, body []byte,
) (detail *McpServerDetail, status int, respBody []byte, err error) {
	resp, err := c.UpdateMcpServerV1OrgsOrgIdMcpServersMcpServerIdPatchWithBodyWithResponse(
		ctx, orgID, mcpServerID, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, 0, nil, err
	}
	if resp.StatusCode() != http.StatusOK {
		return nil, resp.StatusCode(), resp.Body, nil
	}
	detail, err = DecodeMcpServerDetail(resp.Body)
	return detail, resp.StatusCode(), resp.Body, err
}

// GetMcpServerTyped fetches a server by id and decodes the variant. A nil detail
// with status 404 signals the server no longer exists.
func (c *ClientWithResponses) GetMcpServerTyped(
	ctx context.Context, orgID, mcpServerID string,
) (detail *McpServerDetail, status int, respBody []byte, err error) {
	resp, err := c.GetMcpServerV1OrgsOrgIdMcpServersMcpServerIdGetWithResponse(ctx, orgID, mcpServerID)
	if err != nil {
		return nil, 0, nil, err
	}
	if resp.StatusCode() != http.StatusOK {
		return nil, resp.StatusCode(), resp.Body, nil
	}
	detail, err = DecodeMcpServerDetail(resp.Body)
	return detail, resp.StatusCode(), resp.Body, err
}
