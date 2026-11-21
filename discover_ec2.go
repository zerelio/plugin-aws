package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	plugintypes "github.com/zerelio/plugin-sdk/plugin"
	"github.com/zerelio/plugin-sdk/types"

	sdk "github.com/zerelio/plugin-sdk"
)

// DiscoverEC2 discovers EC2 instances across all configured regions. It extracts
// instance metadata and creates CONTAINS relationships in the account -> region
// -> VPC -> subnet -> instance hierarchy, RUNS_ON relationships for IAM instance
// profiles, and CONNECTS_TO relationships for security groups.
func DiscoverEC2(ctx context.Context, clients *AWSClients, ch chan<- plugintypes.DiscoveryEvent) (int32, int32) {
	var resourcesFound, relationshipsFound int32

	paginator := ec2.NewDescribeInstancesPaginator(clients.EC2, &ec2.DescribeInstancesInput{})

	err := Paginate(ctx, func() (bool, error) {
		if !paginator.HasMorePages() {
			return false, nil
		}

		page, err := paginator.NextPage(ctx)
		if err != nil {
			return false, fmt.Errorf("ec2:DescribeInstances failed in %s: %w", clients.Region, err)
		}

		for _, reservation := range page.Reservations {
			for _, instance := range reservation.Instances {
				// Skip terminated instances.
				if instance.State != nil && instance.State.Name == ec2types.InstanceStateNameTerminated {
					continue
				}

				res := buildEC2InstanceResource(instance, clients)
				sdk.EmitResource(ch, res)
				resourcesFound++

				// Create CONTAINS relationships: VPC -> Subnet -> Instance.
				if instance.VpcId != nil && instance.SubnetId != nil {
					vpcID := fmt.Sprintf("%s:%s:%s:%s:%s", "aws", types.TypeVPC, clients.Region, clients.AccountID, *instance.VpcId)
					subnetID := fmt.Sprintf("%s:%s:%s:%s:%s", "aws", types.TypeSubnet, clients.Region, clients.AccountID, *instance.SubnetId)

					rel := sdk.NewRelationship(vpcID, subnetID, types.RelContains)
					sdk.EmitRelationship(ch, rel)
					relationshipsFound++

					rel = sdk.NewRelationship(subnetID, res.ID, types.RelContains)
					sdk.EmitRelationship(ch, rel)
					relationshipsFound++
				}

				// Create RUNS_ON relationship: Instance -> IAM Role.
				if instance.IamInstanceProfile != nil && instance.IamInstanceProfile.Arn != nil {
					profileARN := *instance.IamInstanceProfile.Arn
					// Extract role name from instance profile ARN.
					roleName := extractRoleNameFromProfileARN(profileARN)
					if roleName != "" {
						roleID := fmt.Sprintf("%s:%s:global:%s:%s", "aws", types.TypeIAMRole, clients.AccountID, roleName)
						rel := sdk.NewRelationship(res.ID, roleID, types.RelRunsOn)
						rel.Properties["iam_profile_arn"] = profileARN
						sdk.EmitRelationship(ch, rel)
						relationshipsFound++
					}
				}

				// Create CONNECTS_TO relationships: Instance -> Security Groups.
				for _, sg := range instance.SecurityGroups {
					if sg.GroupId != nil {
						sgID := fmt.Sprintf("%s:%s:%s:%s:%s", "aws", types.TypeEC2SecurityGroup, clients.Region, clients.AccountID, *sg.GroupId)
						rel := sdk.NewRelationship(res.ID, sgID, types.RelConnectsTo)
						if sg.GroupName != nil {
							rel.Properties["group_name"] = *sg.GroupName
						}
						sdk.EmitRelationship(ch, rel)
						relationshipsFound++
					}
				}
			}
		}

		return paginator.HasMorePages(), nil
	}, 5)

	if err != nil {
		ch <- sdk.SendError("EC2_ERROR", fmt.Sprintf("EC2 discovery failed in %s: %v", clients.Region, err), true)
	}

	return resourcesFound, relationshipsFound
}

// buildEC2InstanceResource creates a Resource from an EC2 Instance.
func buildEC2InstanceResource(instance ec2types.Instance, clients *AWSClients) *types.Resource {
	instanceID := aws.ToString(instance.InstanceId)
	name := getInstanceName(instance)

	res := sdk.NewResource("aws", types.TypeEC2Instance, clients.Region, clients.AccountID, instanceID)
	res.Name = name

	props := res.Properties
	props["instance_id"] = instanceID

	if instance.InstanceType != "" {
		props["instance_type"] = string(instance.InstanceType)
	}
	if instance.State != nil {
		props["state"] = string(instance.State.Name)
	}
	if instance.ImageId != nil {
		props["ami_id"] = *instance.ImageId
	}
	if instance.VpcId != nil {
		props["vpc_id"] = *instance.VpcId
	}
	if instance.SubnetId != nil {
		props["subnet_id"] = *instance.SubnetId
	}
	if instance.PrivateIpAddress != nil {
		props["private_ip"] = *instance.PrivateIpAddress
	}
	if instance.PublicIpAddress != nil {
		props["public_ip"] = *instance.PublicIpAddress
	}
	if instance.PrivateDnsName != nil {
		props["private_dns"] = *instance.PrivateDnsName
	}
	if instance.PublicDnsName != nil {
		props["public_dns"] = *instance.PublicDnsName
	}
	if instance.KeyName != nil {
		props["key_name"] = *instance.KeyName
	}
	if instance.Architecture != "" {
		props["architecture"] = string(instance.Architecture)
	}
	if instance.Platform != "" {
		props["platform"] = string(instance.Platform)
	}
	if instance.RootDeviceType != "" {
		props["root_device_type"] = string(instance.RootDeviceType)
	}
	if instance.RootDeviceName != nil {
		props["root_device_name"] = *instance.RootDeviceName
	}
	if instance.LaunchTime != nil {
		props["launch_time"] = instance.LaunchTime.Format("2006-01-02T15:04:05Z")
	}
	if instance.IamInstanceProfile != nil && instance.IamInstanceProfile.Arn != nil {
		props["iam_instance_profile_arn"] = *instance.IamInstanceProfile.Arn
	}
	if instance.InstanceLifecycle != "" {
		props["lifecycle"] = string(instance.InstanceLifecycle)
	}
	if instance.CpuOptions != nil {
		props["cpu_cores"] = fmt.Sprintf("%d", instance.CpuOptions.CoreCount)
	}

	// Security group IDs.
	var sgIDs []string
	for _, sg := range instance.SecurityGroups {
		if sg.GroupId != nil {
			sgIDs = append(sgIDs, *sg.GroupId)
		}
	}
	if len(sgIDs) > 0 {
		props["security_group_ids"] = strings.Join(sgIDs, ",")
	}

	// Tags.
	if len(instance.Tags) > 0 {
		tagMap := make(map[string]string, len(instance.Tags))
		for _, t := range instance.Tags {
			if t.Key != nil && t.Value != nil {
				tagMap[*t.Key] = *t.Value
			}
		}
		res.Tags = sdk.TagsToStrings(tagMap)
	}

	// Raw data.
	rawData, _ := json.Marshal(instance)
	res.RawData = rawData

	return res
}

// getInstanceName extracts the Name tag from an EC2 instance, falling back to
// the instance ID if no Name tag is set.
func getInstanceName(instance ec2types.Instance) string {
	for _, t := range instance.Tags {
		if t.Key != nil && *t.Key == "Name" && t.Value != nil {
			return *t.Value
		}
	}
	if instance.InstanceId != nil {
		return *instance.InstanceId
	}
	return "unknown"
}

// extractRoleNameFromProfileARN extracts the role name from an IAM instance
// profile ARN. The ARN format is:
// arn:aws:iam::account:instance-profile/role-name
func extractRoleNameFromProfileARN(arn string) string {
	parts := strings.Split(arn, "/")
	if len(parts) >= 2 {
		return parts[len(parts)-1]
	}
	return ""
}
