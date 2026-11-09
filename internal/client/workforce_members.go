package client

import (
	"context"
	"fmt"
	"net/http"
)

// WorkforceIdentityHeader is the membership API's acknowledgement that it
// checked expected_workforce_id against the same board instance it authorized.
// This public HTTP contract is documented in the vendored OpenAPI response.
const WorkforceIdentityHeader = "X-Botyard-Workforce-Id"

// RemoveWorkforceMember uses the generated collection-shaped removal request
// for exactly one principal. Like the DELETE wrappers, it handles an empty 204
// with JSON Content-Type without trying to decode JSON. Returning the identity
// acknowledgement lets the resource verify the response without hiding errors.
func (c *ClientWithResponses) RemoveWorkforceMember(ctx context.Context, orgID, slug, workforceID string, actorType ActorType, actorID string) (status int, body []byte, acknowledgement string, err error) {
	writes, err := c.membershipWriteClient()
	if err != nil {
		return 0, nil, "", err
	}
	response, err := writes.RemoveWorkforceMembersV1OrgsOrgIdWorkforcesWorkforceSlugMembersDeletePost(ctx,
		orgID, slug, &RemoveWorkforceMembersV1OrgsOrgIdWorkforcesWorkforceSlugMembersDeletePostParams{ExpectedWorkforceId: &workforceID},
		WorkforceMembersRemove{ExpectedWorkforceId: &workforceID, Members: []WorkforceMemberRef{{ActorType: actorType, ActorId: actorID}}})
	if err != nil {
		return 0, nil, "", err
	}
	acknowledgement = response.Header.Get(WorkforceIdentityHeader)
	status, body, err = deleteStatus(response, nil)
	return status, body, acknowledgement, err
}

// membershipWriteClient copies the generated and HTTP client configuration so
// membership writes cannot be replayed by redirects. Reads and other resources
// retain their existing policy. Refuse opaque doers whose redirect policy cannot
// be enforced instead of silently falling back to potentially replayable writes.
func (c *ClientWithResponses) membershipWriteClient() (*ClientWithResponses, error) {
	generated, ok := c.ClientInterface.(*Client)
	if !ok {
		return nil, fmt.Errorf("membership writes require the generated HTTP client")
	}
	configured, ok := generated.Client.(*http.Client)
	if !ok {
		return nil, fmt.Errorf("membership writes require an HTTP client with enforceable redirect policy")
	}
	httpClient := *configured
	httpClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return fmt.Errorf("membership write redirect refused; no redirected request was sent")
	}
	copied := *generated
	copied.Client = &httpClient
	return &ClientWithResponses{ClientInterface: &copied}, nil
}

// SetWorkforceMember uses the generated collection write without redirect replay.
func (c *ClientWithResponses) SetWorkforceMember(ctx context.Context, orgID, slug, workforceID string, actorType ActorType, actorID, role string) (*SetWorkforceMembersV1OrgsOrgIdWorkforcesWorkforceSlugMembersPutResponse, error) {
	writes, err := c.membershipWriteClient()
	if err != nil {
		return nil, err
	}
	return writes.SetWorkforceMembersV1OrgsOrgIdWorkforcesWorkforceSlugMembersPutWithResponse(ctx,
		orgID, slug, &SetWorkforceMembersV1OrgsOrgIdWorkforcesWorkforceSlugMembersPutParams{ExpectedWorkforceId: &workforceID},
		WorkforceMembersSet{ExpectedWorkforceId: &workforceID, Members: []WorkforceMemberSet{{ActorType: actorType, ActorId: actorID, Role: &role}}})
}
