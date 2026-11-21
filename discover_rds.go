package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"

	plugintypes "github.com/zerelio/plugin-sdk/plugin"
	"github.com/zerelio/plugin-sdk/types"

	sdk "github.com/zerelio/plugin-sdk"
)

// DiscoverRDS discovers RDS instances, clusters, subnet groups, and parameter
// groups. It creates CONTAINS relationships (Account -> RDS), CONNECTS_TO
// relationships (RDS -> SubnetGroup -> Subnets), and BACKED_BY relationships
// (RDS -> KMS key when encrypted).
func DiscoverRDS(ctx context.Context, clients *AWSClients, ch chan<- plugintypes.DiscoveryEvent) (int32, int32) {
	var resourcesFound, relationshipsFound int32

	// Discover DB subnet groups first (needed for relationships).
	subnetGroupMap := discoverDBSubnetGroups(ctx, clients, ch, &resourcesFound)

	// Discover DB instances.
	discoverDBInstances(ctx, clients, ch, &resourcesFound, &relationshipsFound, subnetGroupMap)

	// Discover DB clusters (Aurora).
	discoverDBClusters(ctx, clients, ch, &resourcesFound, &relationshipsFound)

	return resourcesFound, relationshipsFound
}

// discoverDBSubnetGroups discovers all DB subnet groups and returns a map of
// subnet group name -> resource ID.
func discoverDBSubnetGroups(ctx context.Context, clients *AWSClients, ch chan<- plugintypes.DiscoveryEvent, resFound *int32) map[string]string {
	subnetGroupMap := make(map[string]string)

	paginator := rds.NewDescribeDBSubnetGroupsPaginator(clients.RDS, &rds.DescribeDBSubnetGroupsInput{})

	err := Paginate(ctx, func() (bool, error) {
		if !paginator.HasMorePages() {
			return false, nil
		}

		page, err := paginator.NextPage(ctx)
		if err != nil {
			return false, fmt.Errorf("rds:DescribeDBSubnetGroups failed in %s: %w", clients.Region, err)
		}

		for _, sg := range page.DBSubnetGroups {
			sgName := aws.ToString(sg.DBSubnetGroupName)
			sgARN := aws.ToString(sg.DBSubnetGroupArn)

			res := sdk.NewResourceWithID(sgARN, "aws", types.TypeRDSSubnetGroup, clients.Region, clients.AccountID, sgName)
			res.Name = sgName

			props := res.Properties
			props["subnet_group_name"] = sgName
			props["arn"] = sgARN

			if sg.VpcId != nil {
				props["vpc_id"] = *sg.VpcId
			}
			if sg.DBSubnetGroupDescription != nil {
				props["description"] = *sg.DBSubnetGroupDescription
			}
			if sg.SubnetGroupStatus != nil {
				props["status"] = *sg.SubnetGroupStatus
			}

			// Subnet IDs.
			var subnetIDs []string
			for _, subnet := range sg.Subnets {
				if subnet.SubnetIdentifier != nil {
					subnetIDs = append(subnetIDs, *subnet.SubnetIdentifier)
				}
			}
			if len(subnetIDs) > 0 {
				props["subnet_ids"] = strings.Join(subnetIDs, ",")
			}

			rawData, _ := json.Marshal(sg)
			res.RawData = rawData

			sdk.EmitResource(ch, res)
			*resFound++

			subnetGroupMap[sgName] = res.ID

			// CONTAINS: VPC -> SubnetGroup.
			if sg.VpcId != nil {
				vpcID := fmt.Sprintf("%s:%s:%s:%s:%s", "aws", types.TypeVPC, clients.Region, clients.AccountID, *sg.VpcId)
				rel := sdk.NewRelationship(vpcID, res.ID, types.RelContains)
				sdk.EmitRelationship(ch, rel)
			}
		}

		return paginator.HasMorePages(), nil
	}, 5)

	if err != nil {
		ch <- sdk.SendError("RDS_SUBNET_GROUP_ERROR", fmt.Sprintf("RDS subnet group discovery failed in %s: %v", clients.Region, err), true)
	}

	return subnetGroupMap
}

// discoverDBInstances discovers all RDS DB instances.
func discoverDBInstances(ctx context.Context, clients *AWSClients, ch chan<- plugintypes.DiscoveryEvent, resFound, relFound *int32, subnetGroupMap map[string]string) {
	paginator := rds.NewDescribeDBInstancesPaginator(clients.RDS, &rds.DescribeDBInstancesInput{})

	err := Paginate(ctx, func() (bool, error) {
		if !paginator.HasMorePages() {
			return false, nil
		}

		page, err := paginator.NextPage(ctx)
		if err != nil {
			return false, fmt.Errorf("rds:DescribeDBInstances failed in %s: %w", clients.Region, err)
		}

		for _, db := range page.DBInstances {
			dbID := aws.ToString(db.DBInstanceIdentifier)
			dbARN := aws.ToString(db.DBInstanceArn)

			res := sdk.NewResourceWithID(dbARN, "aws", types.TypeRDSInstance, clients.Region, clients.AccountID, dbID)
			res.Name = dbID

			props := res.Properties
			props["db_instance_identifier"] = dbID
			props["arn"] = dbARN

			if db.Engine != nil {
				props["engine"] = *db.Engine
			}
			if db.EngineVersion != nil {
				props["engine_version"] = *db.EngineVersion
			}
			if db.DBInstanceClass != nil {
				props["instance_class"] = *db.DBInstanceClass
			}
			if db.DBInstanceStatus != nil {
				props["status"] = *db.DBInstanceStatus
			}
			if db.AllocatedStorage != nil {
				props["allocated_storage_gb"] = fmt.Sprintf("%d", *db.AllocatedStorage)
			}
			if db.StorageType != nil {
				props["storage_type"] = *db.StorageType
			}
			if db.StorageEncrypted != nil {
				props["storage_encrypted"] = fmt.Sprintf("%t", *db.StorageEncrypted)
			}
			if db.MultiAZ != nil {
				props["multi_az"] = fmt.Sprintf("%t", *db.MultiAZ)
			}
			if db.AvailabilityZone != nil {
				props["availability_zone"] = *db.AvailabilityZone
			}
			if db.SecondaryAvailabilityZone != nil {
				props["secondary_az"] = *db.SecondaryAvailabilityZone
			}
			if db.MasterUsername != nil {
				props["master_username"] = *db.MasterUsername
			}
			if db.Endpoint != nil && db.Endpoint.Port != nil {
				props["port"] = fmt.Sprintf("%d", *db.Endpoint.Port)
			}
			if db.Endpoint != nil {
				if db.Endpoint.Address != nil {
					props["endpoint_address"] = *db.Endpoint.Address
				}
			}
			if db.DBName != nil {
				props["database_name"] = *db.DBName
			}
			if db.BackupRetentionPeriod != nil {
				props["backup_retention_days"] = fmt.Sprintf("%d", *db.BackupRetentionPeriod)
			}
			if db.DeletionProtection != nil {
				props["deletion_protection"] = fmt.Sprintf("%t", *db.DeletionProtection)
			}
			if db.PerformanceInsightsEnabled != nil {
				props["performance_insights"] = fmt.Sprintf("%t", *db.PerformanceInsightsEnabled)
			}
			if db.Iops != nil {
				props["iops"] = fmt.Sprintf("%d", *db.Iops)
			}
			if db.MaxAllocatedStorage != nil {
				props["max_allocated_storage_gb"] = fmt.Sprintf("%d", *db.MaxAllocatedStorage)
			}

			// VPC and subnet group.
			if db.DBSubnetGroup != nil && db.DBSubnetGroup.DBSubnetGroupName != nil {
				props["subnet_group"] = *db.DBSubnetGroup.DBSubnetGroupName
				if db.DBSubnetGroup.VpcId != nil {
					props["vpc_id"] = *db.DBSubnetGroup.VpcId
				}
			}

			// Security groups.
			var sgIDs []string
			for _, sg := range db.VpcSecurityGroups {
				if sg.VpcSecurityGroupId != nil {
					sgIDs = append(sgIDs, *sg.VpcSecurityGroupId)
				}
			}
			if len(sgIDs) > 0 {
				props["security_group_ids"] = strings.Join(sgIDs, ",")
			}

			// KMS key.
			if db.KmsKeyId != nil {
				props["kms_key_id"] = *db.KmsKeyId
			}

			// Tags.
			if len(db.TagList) > 0 {
				tagMap := make(map[string]string, len(db.TagList))
				for _, t := range db.TagList {
					if t.Key != nil && t.Value != nil {
						tagMap[*t.Key] = *t.Value
					}
				}
				res.Tags = sdk.TagsToStrings(tagMap)
			}

			rawData, _ := json.Marshal(db)
			res.RawData = rawData

			sdk.EmitResource(ch, res)
			*resFound++

			// CONTAINS: Account -> RDS Instance.
			accountID := fmt.Sprintf("aws:account:%s:%s", "global", clients.AccountID)
			rel := sdk.NewRelationship(accountID, res.ID, types.RelContains)
			rel.Properties["service"] = "rds"
			sdk.EmitRelationship(ch, rel)
			*relFound++

			// CONNECTS_TO: RDS -> SubnetGroup.
			if db.DBSubnetGroup != nil && db.DBSubnetGroup.DBSubnetGroupName != nil {
				sgName := *db.DBSubnetGroup.DBSubnetGroupName
				if sgResID, ok := subnetGroupMap[sgName]; ok {
					rel := sdk.NewRelationship(res.ID, sgResID, types.RelConnectsTo)
					sdk.EmitRelationship(ch, rel)
					*relFound++
				}
			}

			// BACKED_BY: RDS -> KMS Key (if encrypted).
			if db.StorageEncrypted != nil && *db.StorageEncrypted && db.KmsKeyId != nil {
				kmKeyID := fmt.Sprintf("aws:kms:key:%s", *db.KmsKeyId)
				rel := sdk.NewRelationship(res.ID, kmKeyID, types.RelBackedBy)
				rel.Properties["key_type"] = "aws_kms"
				sdk.EmitRelationship(ch, rel)
				*relFound++
			}

			// CONNECTS_TO: RDS -> Security Groups.
			for _, sg := range db.VpcSecurityGroups {
				if sg.VpcSecurityGroupId != nil {
					sgResID := fmt.Sprintf("%s:%s:%s:%s:%s", "aws", types.TypeEC2SecurityGroup, clients.Region, clients.AccountID, *sg.VpcSecurityGroupId)
					rel := sdk.NewRelationship(res.ID, sgResID, types.RelConnectsTo)
					if sg.Status != nil {
						rel.Properties["status"] = *sg.Status
					}
					sdk.EmitRelationship(ch, rel)
					*relFound++
				}
			}
		}

		return paginator.HasMorePages(), nil
	}, 5)

	if err != nil {
		ch <- sdk.SendError("RDS_INSTANCE_ERROR", fmt.Sprintf("RDS instance discovery failed in %s: %v", clients.Region, err), true)
	}
}

// discoverDBClusters discovers Aurora DB clusters.
func discoverDBClusters(ctx context.Context, clients *AWSClients, ch chan<- plugintypes.DiscoveryEvent, resFound, relFound *int32) {
	paginator := rds.NewDescribeDBClustersPaginator(clients.RDS, &rds.DescribeDBClustersInput{})

	err := Paginate(ctx, func() (bool, error) {
		if !paginator.HasMorePages() {
			return false, nil
		}

		page, err := paginator.NextPage(ctx)
		if err != nil {
			return false, fmt.Errorf("rds:DescribeDBClusters failed in %s: %w", clients.Region, err)
		}

		for _, cluster := range page.DBClusters {
			clusterID := aws.ToString(cluster.DBClusterIdentifier)
			clusterARN := aws.ToString(cluster.DBClusterArn)

			res := sdk.NewResourceWithID(clusterARN, "aws", types.TypeRDSCluster, clients.Region, clients.AccountID, clusterID)
			res.Name = clusterID

			props := res.Properties
			props["db_cluster_identifier"] = clusterID
			props["arn"] = clusterARN

			if cluster.Engine != nil {
				props["engine"] = *cluster.Engine
			}
			if cluster.EngineVersion != nil {
				props["engine_version"] = *cluster.EngineVersion
			}
			if cluster.Status != nil {
				props["status"] = *cluster.Status
			}
			if cluster.MasterUsername != nil {
				props["master_username"] = *cluster.MasterUsername
			}
			if cluster.Port != nil {
				props["port"] = fmt.Sprintf("%d", *cluster.Port)
			}
			if cluster.Endpoint != nil {
				props["endpoint"] = *cluster.Endpoint
			}
			if cluster.ReaderEndpoint != nil {
				props["reader_endpoint"] = *cluster.ReaderEndpoint
			}
			if cluster.MultiAZ != nil {
				props["multi_az"] = fmt.Sprintf("%t", *cluster.MultiAZ)
			}
			if cluster.StorageEncrypted != nil {
				props["storage_encrypted"] = fmt.Sprintf("%t", *cluster.StorageEncrypted)
			}
			if cluster.KmsKeyId != nil {
				props["kms_key_id"] = *cluster.KmsKeyId
			}
			if cluster.BackupRetentionPeriod != nil {
				props["backup_retention_days"] = fmt.Sprintf("%d", *cluster.BackupRetentionPeriod)
			}
			if cluster.DeletionProtection != nil {
				props["deletion_protection"] = fmt.Sprintf("%t", *cluster.DeletionProtection)
			}
			if cluster.DatabaseName != nil {
				props["database_name"] = *cluster.DatabaseName
			}
			if cluster.EngineMode != nil {
				props["engine_mode"] = *cluster.EngineMode
			}

			// Cluster members (instance references).
			var members []string
			for _, m := range cluster.DBClusterMembers {
				if m.DBInstanceIdentifier != nil {
					members = append(members, *m.DBInstanceIdentifier)
				}
			}
			if len(members) > 0 {
				props["cluster_members"] = strings.Join(members, ",")
			}

			// Tags.
			if len(cluster.TagList) > 0 {
				tagMap := make(map[string]string, len(cluster.TagList))
				for _, t := range cluster.TagList {
					if t.Key != nil && t.Value != nil {
						tagMap[*t.Key] = *t.Value
					}
				}
				res.Tags = sdk.TagsToStrings(tagMap)
			}

			rawData, _ := json.Marshal(cluster)
			res.RawData = rawData

			sdk.EmitResource(ch, res)
			*resFound++

			// CONTAINS: Account -> Cluster.
			accountID := fmt.Sprintf("aws:account:%s:%s", "global", clients.AccountID)
			rel := sdk.NewRelationship(accountID, res.ID, types.RelContains)
			rel.Properties["service"] = "rds"
			sdk.EmitRelationship(ch, rel)
			*relFound++

			// BACKED_BY: Cluster -> KMS Key (if encrypted).
			if cluster.StorageEncrypted != nil && *cluster.StorageEncrypted && cluster.KmsKeyId != nil {
				kmsKeyID := fmt.Sprintf("aws:kms:key:%s", *cluster.KmsKeyId)
				rel := sdk.NewRelationship(res.ID, kmsKeyID, types.RelBackedBy)
				rel.Properties["key_type"] = "aws_kms"
				sdk.EmitRelationship(ch, rel)
				*relFound++
			}
		}

		return paginator.HasMorePages(), nil
	}, 5)

	if err != nil {
		ch <- sdk.SendError("RDS_CLUSTER_ERROR", fmt.Sprintf("RDS cluster discovery failed in %s: %v", clients.Region, err), true)
	}
}
