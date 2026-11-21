package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"

	plugintypes "github.com/zerelio/plugin-sdk/plugin"
	"github.com/zerelio/plugin-sdk/types"

	sdk "github.com/zerelio/plugin-sdk"
)

// DiscoverLambda discovers all Lambda functions in the region. It extracts
// function metadata (runtime, handler, memory, timeout, architecture, URL config)
// and creates a CONTAINS relationship from the account to each function.
func DiscoverLambda(ctx context.Context, clients *AWSClients, ch chan<- plugintypes.DiscoveryEvent) (int32, int32) {
	var resourcesFound, relationshipsFound int32

	paginator := lambda.NewListFunctionsPaginator(clients.Lambda, &lambda.ListFunctionsInput{})

	err := Paginate(ctx, func() (bool, error) {
		if !paginator.HasMorePages() {
			return false, nil
		}

		page, err := paginator.NextPage(ctx)
		if err != nil {
			return false, fmt.Errorf("lambda:ListFunctions failed in %s: %w", clients.Region, err)
		}

		for _, fn := range page.Functions {
			funcName := aws.ToString(fn.FunctionName)
			funcARN := aws.ToString(fn.FunctionArn)

			res := sdk.NewResourceWithID(funcARN, "aws", types.TypeLambdaFunction, clients.Region, clients.AccountID, funcName)
			res.Name = funcName

			props := res.Properties
			props["function_name"] = funcName
			props["arn"] = funcARN

			if fn.Runtime != "" {
				props["runtime"] = string(fn.Runtime)
			}
			if fn.Handler != nil {
				props["handler"] = *fn.Handler
			}
			if fn.MemorySize != nil {
				props["memory_size"] = fmt.Sprintf("%d", *fn.MemorySize)
			}
			if fn.Timeout != nil {
				props["timeout"] = fmt.Sprintf("%d", *fn.Timeout)
			}
			if len(fn.Architectures) > 0 {
				props["architecture"] = string(fn.Architectures[0])
			}
			if fn.LastModified != nil {
				props["last_modified"] = *fn.LastModified
			}
			if fn.Description != nil {
				props["description"] = *fn.Description
			}
			if fn.CodeSize > 0 {
				props["code_size"] = fmt.Sprintf("%d", fn.CodeSize)
			}
			if fn.PackageType != "" {
				props["package_type"] = string(fn.PackageType)
			}
			if fn.Role != nil {
				props["execution_role_arn"] = *fn.Role
			}
			if fn.CodeSha256 != nil {
				props["code_sha256"] = *fn.CodeSha256
			}
			if fn.Version != nil {
				props["version"] = *fn.Version
			}

			// Get function URL config (non-fatal if not configured).
			urlConfig, err := clients.Lambda.GetFunctionUrlConfig(ctx, &lambda.GetFunctionUrlConfigInput{
				FunctionName: &funcName,
			})
			if err == nil && urlConfig != nil {
				props["function_url"] = *urlConfig.FunctionUrl
				props["function_url_auth_type"] = string(urlConfig.AuthType)
			}

			// Tags via ListTags.
			tagResp, err := clients.Lambda.ListTags(ctx, &lambda.ListTagsInput{
				Resource: &funcARN,
			})
			if err == nil && len(tagResp.Tags) > 0 {
				res.Tags = sdk.TagsToStrings(tagResp.Tags)
			}

			rawData, _ := json.Marshal(fn)
			res.RawData = rawData

			sdk.EmitResource(ch, res)
			resourcesFound++

			// CONTAINS: Account -> Lambda Function.
			accountID := fmt.Sprintf("aws:account:%s:%s", "global", clients.AccountID)
			rel := sdk.NewRelationship(accountID, res.ID, types.RelContains)
			rel.Properties["service"] = "lambda"
			sdk.EmitRelationship(ch, rel)
			relationshipsFound++

			// DEPENDS_ON: Lambda Function -> IAM Role (execution role).
			if fn.Role != nil {
				// Extract role name from the execution role ARN.
				// ARN format: arn:aws:iam::account:role/role-name
				roleName := extractLambdaRoleName(*fn.Role)
				if roleName != "" {
					roleID := fmt.Sprintf("%s:%s:global:%s:%s", "aws", types.TypeIAMRole, clients.AccountID, roleName)
					rel := sdk.NewRelationship(res.ID, roleID, types.RelDependsOn)
					rel.Properties["iam_role_arn"] = *fn.Role
					sdk.EmitRelationship(ch, rel)
					relationshipsFound++
				}
			}
		}

		return paginator.HasMorePages(), nil
	}, 5)

	if err != nil {
		ch <- sdk.SendError("LAMBDA_ERROR", fmt.Sprintf("Lambda discovery failed in %s: %v", clients.Region, err), true)
	}

	return resourcesFound, relationshipsFound
}

// extractLambdaRoleName extracts the role name from a Lambda execution role ARN.
// ARN format: arn:aws:iam::account:role/role-name
func extractLambdaRoleName(arn string) string {
	// Parse the role name from the ARN. The role is the last segment after "role/".
	// Example: arn:aws:iam::123456789012:role/my-lambda-role
	parts := splitRoleARN(arn)
	if len(parts) > 0 {
		return parts[len(parts)-1]
	}
	return ""
}

// splitRoleARN splits a role ARN on "/" to extract the role name from the last segment.
func splitRoleARN(arn string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(arn); i++ {
		if arn[i] == '/' {
			parts = append(parts, arn[start:i])
			start = i + 1
		}
	}
	if start < len(arn) {
		parts = append(parts, arn[start:])
	}
	return parts
}
