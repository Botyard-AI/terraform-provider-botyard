package client

import "context"

// WorkforceIdentityHeader is the membership API's acknowledgement that it
// checked expected_workforce_id against the same board instance it authorized.
// This public HTTP contract is documented in the vendored OpenAPI response.
const WorkforceIdentityHeader = "X-Botyard-Workforce-Id"

// RemoveWorkforceMember uses the generated collection-shaped removal request
// for exactly one principal. Like the DELETE wrappers, it handles an empty 204
// with JSON Content-Type without trying to decode JSON. Returning the identity
// acknowledgement lets the resource verify the response without hiding errors.
func (c *ClientWithResponses) RemoveWorkforceMember(ctx context.Context, orgID, slug, workforceID string, actorType ActorType, actorID string) (status int, body []byte, acknowledgement string, err error) {
	response, err := c.RemoveWorkforceMembersV1OrgsOrgIdWorkforcesWorkforceSlugMembersDeletePost(ctx,
		orgID, slug, &RemoveWorkforceMembersV1OrgsOrgIdWorkforcesWorkforceSlugMembersDeletePostParams{ExpectedWorkforceId: &workforceID},
		WorkforceMembersRemove{ExpectedWorkforceId: &workforceID, Members: []WorkforceMemberRef{{ActorType: actorType, ActorId: actorID}}})
	if err != nil {
		return 0, nil, "", err
	}
	acknowledgement = response.Header.Get(WorkforceIdentityHeader)
	status, body, err = deleteStatus(response, nil)
	return status, body, acknowledgement, err
}
