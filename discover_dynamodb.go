package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	plugintypes "github.com/zerelio/plugin-sdk/plugin"
	"github.com/zerelio/plugin-sdk/types"

	sdk "github.com/zerelio/plugin-sdk"
)

// DiscoverDynamoDB discovers all DynamoDB tables in the region. It extracts
// table metadata (status, billing mode, item count, size, stream config, GSIs)
// and creates a CONTAINS relationship from the account to each table, plus
// BACKED_BY relationships for KMS-encrypted tables.
func DiscoverDynamoDB(ctx context.Context, clients *AWSClients, ch chan<- plugintypes.DiscoveryEvent) (int32, int32) {
	var resourcesFound, relationshipsFound int32

	paginator := dynamodb.NewListTablesPaginator(clients.DynamoDB, &dynamodb.ListTablesInput{})

	err := Paginate(ctx, func() (bool, error) {
		if !paginator.HasMorePages() {
			return false, nil
		}

		page, err := paginator.NextPage(ctx)
		if err != nil {
			return false, fmt.Errorf("dynamodb:ListTables failed in %s: %w", clients.Region, err)
		}

		for _, tableName := range page.TableNames {
			desc, err := clients.DynamoDB.DescribeTable(ctx, &dynamodb.DescribeTableInput{
				TableName: &tableName,
			})
			if err != nil {
				ch <- sdk.SendError("DYNAMODB_ERROR",
					fmt.Sprintf("dynamodb:DescribeTable failed for %s in %s: %v", tableName, clients.Region, err), true)
				continue
			}

			table := desc.Table
			tableARN := aws.ToString(table.TableArn)

			res := sdk.NewResourceWithID(tableARN, "aws", types.TypeDynamoDBTable, clients.Region, clients.AccountID, tableName)
			res.Name = tableName

			props := res.Properties
			props["table_name"] = tableName
			props["arn"] = tableARN

			if table.TableStatus != "" {
				props["status"] = string(table.TableStatus)
			}
			if table.ItemCount != nil {
				props["item_count"] = fmt.Sprintf("%d", *table.ItemCount)
			}
			if table.TableSizeBytes != nil {
				props["table_size_bytes"] = fmt.Sprintf("%d", *table.TableSizeBytes)
			}
			if table.CreationDateTime != nil {
				props["creation_date"] = table.CreationDateTime.Format("2006-01-02T15:04:05Z")
			}

			// Billing mode (defaults to PROVISIONED if not explicitly set).
			if table.BillingModeSummary != nil {
				props["billing_mode"] = string(table.BillingModeSummary.BillingMode)
			} else {
				props["billing_mode"] = "PROVISIONED"
			}

			// Stream settings.
			if table.StreamSpecification != nil {
				props["stream_enabled"] = fmt.Sprintf("%t", *table.StreamSpecification.StreamEnabled)
				if table.StreamSpecification.StreamViewType != "" {
					props["stream_view_type"] = string(table.StreamSpecification.StreamViewType)
				}
			}
			if table.LatestStreamArn != nil {
				props["stream_arn"] = *table.LatestStreamArn
				props["stream_label"] = *table.LatestStreamLabel
			}

			// Encryption (KMS).
			if table.SSEDescription != nil {
				props["sse_type"] = string(table.SSEDescription.SSEType)
				if table.SSEDescription.KMSMasterKeyArn != nil {
					props["kms_key_arn"] = *table.SSEDescription.KMSMasterKeyArn
				}
			}

			// Read/write capacity.
			if table.ProvisionedThroughput != nil {
				if table.ProvisionedThroughput.ReadCapacityUnits != nil {
					props["read_capacity"] = fmt.Sprintf("%d", *table.ProvisionedThroughput.ReadCapacityUnits)
				}
				if table.ProvisionedThroughput.WriteCapacityUnits != nil {
					props["write_capacity"] = fmt.Sprintf("%d", *table.ProvisionedThroughput.WriteCapacityUnits)
				}
			}

			// Primary key schema.
			if len(table.KeySchema) > 0 {
				var keyAttrs []string
				for _, ks := range table.KeySchema {
					keyAttrs = append(keyAttrs, fmt.Sprintf("%s(%s)", string(ks.KeyType), *ks.AttributeName))
				}
				props["key_schema"] = strings.Join(keyAttrs, ", ")
			}

			// Global secondary indexes.
			if len(table.GlobalSecondaryIndexes) > 0 {
				var gsiNames []string
				for _, gsi := range table.GlobalSecondaryIndexes {
					if gsi.IndexName != nil {
						gsiNames = append(gsiNames, *gsi.IndexName)
					}
				}
				props["gsi_count"] = fmt.Sprintf("%d", len(gsiNames))
				props["gsi_names"] = strings.Join(gsiNames, ",")
			}

			// Local secondary indexes.
			if len(table.LocalSecondaryIndexes) > 0 {
				var lsiNames []string
				for _, lsi := range table.LocalSecondaryIndexes {
					if lsi.IndexName != nil {
						lsiNames = append(lsiNames, *lsi.IndexName)
					}
				}
				props["lsi_count"] = fmt.Sprintf("%d", len(lsiNames))
				props["lsi_names"] = strings.Join(lsiNames, ",")
			}

			// Deletion protection.
			if table.DeletionProtectionEnabled != nil {
				props["deletion_protection"] = fmt.Sprintf("%t", *table.DeletionProtectionEnabled)
			}

			// Table class.
			if table.TableClassSummary != nil {
				props["table_class"] = string(table.TableClassSummary.TableClass)
			}

			// Tags via ListTagsOfResource.
			tagResp, err := clients.DynamoDB.ListTagsOfResource(ctx, &dynamodb.ListTagsOfResourceInput{
				ResourceArn: &tableARN,
			})
			if err == nil && len(tagResp.Tags) > 0 {
				tagMap := make(map[string]string, len(tagResp.Tags))
				for _, t := range tagResp.Tags {
					if t.Key != nil && t.Value != nil {
						tagMap[*t.Key] = *t.Value
					}
				}
				res.Tags = sdk.TagsToStrings(tagMap)
			}

			rawData, _ := json.Marshal(table)
			res.RawData = rawData

			sdk.EmitResource(ch, res)
			resourcesFound++

			// CONTAINS: Account -> DynamoDB Table.
			accountID := fmt.Sprintf("aws:account:%s:%s", "global", clients.AccountID)
			rel := sdk.NewRelationship(accountID, res.ID, types.RelContains)
			rel.Properties["service"] = "dynamodb"
			sdk.EmitRelationship(ch, rel)
			relationshipsFound++

			// BACKED_BY: DynamoDB Table -> KMS Key (if encrypted with KMS).
			if table.SSEDescription != nil && table.SSEDescription.KMSMasterKeyArn != nil {
				kmsKeyARN := *table.SSEDescription.KMSMasterKeyArn
				kmsKeyID := fmt.Sprintf("aws:kms:key:%s", kmsKeyARN)
				rel := sdk.NewRelationship(res.ID, kmsKeyID, types.RelBackedBy)
				rel.Properties["key_type"] = "aws_kms"
				rel.Properties["kms_key_arn"] = kmsKeyARN
				sdk.EmitRelationship(ch, rel)
				relationshipsFound++
			}
		}

		return paginator.HasMorePages(), nil
	}, 5)

	if err != nil {
		ch <- sdk.SendError("DYNAMODB_ERROR", fmt.Sprintf("DynamoDB discovery failed in %s: %v", clients.Region, err), true)
	}

	return resourcesFound, relationshipsFound
}
