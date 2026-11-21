package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"

	plugintypes "github.com/zerelio/plugin-sdk/plugin"
	"github.com/zerelio/plugin-sdk/types"

	sdk "github.com/zerelio/plugin-sdk"
)

// DiscoverIAM discovers IAM users, groups, roles, and policies. It creates
// HAS_ROLE relationships (User -> Group, User -> Role) and HAS_POLICY
// relationships (Role -> Policy).
func DiscoverIAM(ctx context.Context, clients *AWSClients, ch chan<- plugintypes.DiscoveryEvent) (int32, int32) {
	var resourcesFound, relationshipsFound int32

	// Discover roles (needed first for relationship mapping).
	roleMap := discoverIAMRoles(ctx, clients, ch, &resourcesFound, &relationshipsFound)

	// Discover users and their group/role memberships.
	discoverIAMUsers(ctx, clients, ch, &resourcesFound, &relationshipsFound, roleMap)

	// Discover groups.
	discoverIAMGroups(ctx, clients, ch, &resourcesFound, &relationshipsFound)

	// Discover policies.
	discoverIAMPolicies(ctx, clients, ch, &resourcesFound, &relationshipsFound)

	return resourcesFound, relationshipsFound
}

// discoverIAMRoles discovers all IAM roles and their attached policies.
func discoverIAMRoles(ctx context.Context, clients *AWSClients, ch chan<- plugintypes.DiscoveryEvent, resFound, relFound *int32) map[string]string {
	roleMap := make(map[string]string) // role name -> resource ID

	paginator := iam.NewListRolesPaginator(clients.IAM, &iam.ListRolesInput{})

	err := Paginate(ctx, func() (bool, error) {
		if !paginator.HasMorePages() {
			return false, nil
		}

		page, err := paginator.NextPage(ctx)
		if err != nil {
			return false, fmt.Errorf("iam:ListRoles failed: %w", err)
		}

		for _, role := range page.Roles {
			roleName := aws.ToString(role.RoleName)
			roleARN := aws.ToString(role.Arn)

			res := sdk.NewResourceWithID(roleARN, "aws", types.TypeIAMRole, "global", clients.AccountID, roleName)
			res.Name = roleName

			props := res.Properties
			props["role_name"] = roleName
			props["arn"] = roleARN

			if role.Path != nil {
				props["path"] = *role.Path
			}
			if role.CreateDate != nil {
				props["create_date"] = role.CreateDate.Format("2006-01-02T15:04:05Z")
			}
			if role.MaxSessionDuration != nil {
				props["max_session_duration"] = fmt.Sprintf("%d", *role.MaxSessionDuration)
			}
			if role.Description != nil {
				props["description"] = *role.Description
			}

			// Trust policy (assume role policy).
			if role.AssumeRolePolicyDocument != nil {
				props["trust_policy"] = *role.AssumeRolePolicyDocument
			}

			// Tags.
			if len(role.Tags) > 0 {
				tagMap := make(map[string]string, len(role.Tags))
				for _, t := range role.Tags {
					if t.Key != nil && t.Value != nil {
						tagMap[*t.Key] = *t.Value
					}
				}
				res.Tags = sdk.TagsToStrings(tagMap)
			}

			rawData, _ := json.Marshal(role)
			res.RawData = rawData

			sdk.EmitResource(ch, res)
			*resFound++

			roleMap[roleName] = res.ID

			// List attached policies for this role.
			discoverRolePolicies(ctx, clients, ch, res.ID, roleName, relFound)
		}

		return paginator.HasMorePages(), nil
	}, 5)

	if err != nil {
		ch <- sdk.SendError("IAM_ROLE_ERROR", fmt.Sprintf("IAM role discovery failed: %v", err), true)
	}

	return roleMap
}

// discoverRolePolicies lists both managed and inline policies attached to a role.
func discoverRolePolicies(ctx context.Context, clients *AWSClients, ch chan<- plugintypes.DiscoveryEvent, roleResID, roleName string, relFound *int32) {
	// Attached managed policies.
	policyPaginator := iam.NewListAttachedRolePoliciesPaginator(clients.IAM, &iam.ListAttachedRolePoliciesInput{
		RoleName: aws.String(roleName),
	})

	err := Paginate(ctx, func() (bool, error) {
		if !policyPaginator.HasMorePages() {
			return false, nil
		}

		page, err := policyPaginator.NextPage(ctx)
		if err != nil {
			return false, fmt.Errorf("iam:ListAttachedRolePolicies for %s failed: %w", roleName, err)
		}

		for _, policy := range page.AttachedPolicies {
			policyARN := aws.ToString(policy.PolicyArn)
			policyName := aws.ToString(policy.PolicyName)

			policyResID := fmt.Sprintf("%s:%s:global:%s:%s", "aws", types.TypeIAMPolicy, clients.AccountID, policyARN)

			rel := sdk.NewRelationship(roleResID, policyResID, types.RelHasPolicy)
			rel.Properties["policy_name"] = policyName
			rel.Properties["policy_arn"] = policyARN
			sdk.EmitRelationship(ch, rel)
			*relFound++
		}

		return policyPaginator.HasMorePages(), nil
	}, 5)

	if err != nil {
		ch <- sdk.SendError("IAM_ROLE_POLICY_ERROR", fmt.Sprintf("Failed to list policies for role %s: %v", roleName, err), true)
	}

	// Inline policies.
	inlineResult, err := clients.IAM.ListRolePolicies(ctx, &iam.ListRolePoliciesInput{
		RoleName: aws.String(roleName),
	})
	if err != nil {
		return
	}

	for _, policyName := range inlineResult.PolicyNames {
		policyResID := fmt.Sprintf("%s:%s:global:%s:inline:%s:%s", "aws", types.TypeIAMPolicy, clients.AccountID, roleName, policyName)

		rel := sdk.NewRelationship(roleResID, policyResID, types.RelHasPolicy)
		rel.Properties["policy_name"] = policyName
		rel.Properties["attachment_type"] = "inline"
		sdk.EmitRelationship(ch, rel)
		*relFound++
	}
}

// discoverIAMUsers discovers all IAM users and their group memberships.
func discoverIAMUsers(ctx context.Context, clients *AWSClients, ch chan<- plugintypes.DiscoveryEvent, resFound, relFound *int32, roleMap map[string]string) {
	paginator := iam.NewListUsersPaginator(clients.IAM, &iam.ListUsersInput{})

	err := Paginate(ctx, func() (bool, error) {
		if !paginator.HasMorePages() {
			return false, nil
		}

		page, err := paginator.NextPage(ctx)
		if err != nil {
			return false, fmt.Errorf("iam:ListUsers failed: %w", err)
		}

		for _, user := range page.Users {
			userName := aws.ToString(user.UserName)
			userARN := aws.ToString(user.Arn)

			res := sdk.NewResourceWithID(userARN, "aws", types.TypeIAMUser, "global", clients.AccountID, userName)
			res.Name = userName

			props := res.Properties
			props["user_name"] = userName
			props["arn"] = userARN

			if user.Path != nil {
				props["path"] = *user.Path
			}
			if user.CreateDate != nil {
				props["create_date"] = user.CreateDate.Format("2006-01-02T15:04:05Z")
			}
			if user.PasswordLastUsed != nil {
				props["password_last_used"] = user.PasswordLastUsed.Format("2006-01-02T15:04:05Z")
			}
			// Permissions boundary.
			if user.PermissionsBoundary != nil {
				if user.PermissionsBoundary.PermissionsBoundaryArn != nil {
					props["permissions_boundary_arn"] = *user.PermissionsBoundary.PermissionsBoundaryArn
				}
				if user.PermissionsBoundary.PermissionsBoundaryType != "" {
					props["permissions_boundary_type"] = string(user.PermissionsBoundary.PermissionsBoundaryType)
				}
			}

			// Tags.
			if len(user.Tags) > 0 {
				tagMap := make(map[string]string, len(user.Tags))
				for _, t := range user.Tags {
					if t.Key != nil && t.Value != nil {
						tagMap[*t.Key] = *t.Value
					}
				}
				res.Tags = sdk.TagsToStrings(tagMap)
			}

			rawData, _ := json.Marshal(user)
			res.RawData = rawData

			sdk.EmitResource(ch, res)
			*resFound++

			// List group memberships for this user.
			discoverUserGroups(ctx, clients, ch, res.ID, userName, relFound)

			// List attached policies for this user.
			discoverUserPolicies(ctx, clients, ch, res.ID, userName, relFound)
		}

		return paginator.HasMorePages(), nil
	}, 5)

	if err != nil {
		ch <- sdk.SendError("IAM_USER_ERROR", fmt.Sprintf("IAM user discovery failed: %v", err), true)
	}
}

// discoverUserGroups lists the groups a user belongs to and creates HAS_ROLE relationships.
func discoverUserGroups(ctx context.Context, clients *AWSClients, ch chan<- plugintypes.DiscoveryEvent, userResID, userName string, relFound *int32) {
	result, err := clients.IAM.ListGroupsForUser(ctx, &iam.ListGroupsForUserInput{
		UserName: aws.String(userName),
	})
	if err != nil {
		return
	}

	for _, group := range result.Groups {
		groupName := aws.ToString(group.GroupName)
		groupResID := fmt.Sprintf("%s:%s:global:%s:%s", "aws", types.TypeIAMUser, clients.AccountID, fmt.Sprintf("group:%s", groupName))

		rel := sdk.NewRelationship(userResID, groupResID, types.RelHasRole)
		rel.Properties["group_name"] = groupName
		if group.Arn != nil {
			rel.Properties["group_arn"] = *group.Arn
		}
		sdk.EmitRelationship(ch, rel)
		*relFound++
	}
}

// discoverUserPolicies lists policies attached to a user.
func discoverUserPolicies(ctx context.Context, clients *AWSClients, ch chan<- plugintypes.DiscoveryEvent, userResID, userName string, relFound *int32) {
	// Attached managed policies.
	paginator := iam.NewListAttachedUserPoliciesPaginator(clients.IAM, &iam.ListAttachedUserPoliciesInput{
		UserName: aws.String(userName),
	})

	err := Paginate(ctx, func() (bool, error) {
		if !paginator.HasMorePages() {
			return false, nil
		}

		page, err := paginator.NextPage(ctx)
		if err != nil {
			return false, err
		}

		for _, policy := range page.AttachedPolicies {
			policyARN := aws.ToString(policy.PolicyArn)
			policyResID := fmt.Sprintf("%s:%s:global:%s:%s", "aws", types.TypeIAMPolicy, clients.AccountID, policyARN)

			rel := sdk.NewRelationship(userResID, policyResID, types.RelHasPolicy)
			rel.Properties["policy_name"] = aws.ToString(policy.PolicyName)
			rel.Properties["policy_arn"] = policyARN
			sdk.EmitRelationship(ch, rel)
			*relFound++
		}

		return paginator.HasMorePages(), nil
	}, 5)

	if err != nil {
		// Non-fatal.
		return
	}
}

// discoverIAMGroups discovers IAM groups.
func discoverIAMGroups(ctx context.Context, clients *AWSClients, ch chan<- plugintypes.DiscoveryEvent, resFound, relFound *int32) {
	paginator := iam.NewListGroupsPaginator(clients.IAM, &iam.ListGroupsInput{})

	err := Paginate(ctx, func() (bool, error) {
		if !paginator.HasMorePages() {
			return false, nil
		}

		page, err := paginator.NextPage(ctx)
		if err != nil {
			return false, fmt.Errorf("iam:ListGroups failed: %w", err)
		}

		for _, group := range page.Groups {
			groupName := aws.ToString(group.GroupName)
			groupARN := aws.ToString(group.Arn)

			res := sdk.NewResourceWithID(groupARN, "aws", types.TypeIAMUser, "global", clients.AccountID, fmt.Sprintf("group:%s", groupName))
			res.Name = groupName

			props := res.Properties
			props["group_name"] = groupName
			props["arn"] = groupARN
			props["type"] = "group"

			if group.Path != nil {
				props["path"] = *group.Path
			}
			if group.CreateDate != nil {
				props["create_date"] = group.CreateDate.Format("2006-01-02T15:04:05Z")
			}

			// Group policies.
			policyResult, err := clients.IAM.ListAttachedGroupPolicies(ctx, &iam.ListAttachedGroupPoliciesInput{
				GroupName: aws.String(groupName),
			})
			if err == nil && len(policyResult.AttachedPolicies) > 0 {
				var policies []string
				for _, p := range policyResult.AttachedPolicies {
					if p.PolicyName != nil {
						policies = append(policies, *p.PolicyName)
					}
				}
				if len(policies) > 0 {
					props["attached_policies"] = strings.Join(policies, ",")
				}
			}

			rawData, _ := json.Marshal(group)
			res.RawData = rawData

			sdk.EmitResource(ch, res)
			*resFound++
		}

		return paginator.HasMorePages(), nil
	}, 5)

	if err != nil {
		ch <- sdk.SendError("IAM_GROUP_ERROR", fmt.Sprintf("IAM group discovery failed: %v", err), true)
	}
}

// discoverIAMPolicies discovers IAM policies (customer-managed).
func discoverIAMPolicies(ctx context.Context, clients *AWSClients, ch chan<- plugintypes.DiscoveryEvent, resFound, relFound *int32) {
	paginator := iam.NewListPoliciesPaginator(clients.IAM, &iam.ListPoliciesInput{
		Scope: iamtypes.PolicyScopeTypeLocal, // Customer-managed policies only.
	})

	err := Paginate(ctx, func() (bool, error) {
		if !paginator.HasMorePages() {
			return false, nil
		}

		page, err := paginator.NextPage(ctx)
		if err != nil {
			return false, fmt.Errorf("iam:ListPolicies failed: %w", err)
		}

		for _, policy := range page.Policies {
			policyName := aws.ToString(policy.PolicyName)
			policyARN := aws.ToString(policy.Arn)

			res := sdk.NewResourceWithID(policyARN, "aws", types.TypeIAMPolicy, "global", clients.AccountID, policyARN)
			res.Name = policyName

			props := res.Properties
			props["policy_name"] = policyName
			props["arn"] = policyARN

			if policy.Path != nil {
				props["path"] = *policy.Path
			}
			if policy.CreateDate != nil {
				props["create_date"] = policy.CreateDate.Format("2006-01-02T15:04:05Z")
			}
			if policy.UpdateDate != nil {
				props["update_date"] = policy.UpdateDate.Format("2006-01-02T15:04:05Z")
			}
			if policy.DefaultVersionId != nil {
				props["default_version"] = *policy.DefaultVersionId
			}
			if policy.AttachmentCount != nil {
				props["attachment_count"] = fmt.Sprintf("%d", *policy.AttachmentCount)
			}
			// is_attachable is always set
				props["is_attachable"] = fmt.Sprintf("%t", policy.IsAttachable)
			if policy.Description != nil {
				props["description"] = *policy.Description
			}

			// Tags.
			if len(policy.Tags) > 0 {
				tagMap := make(map[string]string, len(policy.Tags))
				for _, t := range policy.Tags {
					if t.Key != nil && t.Value != nil {
						tagMap[*t.Key] = *t.Value
					}
				}
				res.Tags = sdk.TagsToStrings(tagMap)
			}

			rawData, _ := json.Marshal(policy)
			res.RawData = rawData

			sdk.EmitResource(ch, res)
			*resFound++
		}

		return paginator.HasMorePages(), nil
	}, 5)

	if err != nil {
		ch <- sdk.SendError("IAM_POLICY_ERROR", fmt.Sprintf("IAM policy discovery failed: %v", err), true)
	}
}
