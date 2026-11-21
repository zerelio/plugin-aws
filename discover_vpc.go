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

// DiscoverVPC discovers VPC networking resources: VPCs, subnets, route tables,
// internet gateways, and NAT gateways. It creates CONTAINS relationships for
// the VPC -> Subnet/RouteTable/IGW hierarchy and ROUTES_TO relationships for
// route table entries.
func DiscoverVPC(ctx context.Context, clients *AWSClients, ch chan<- plugintypes.DiscoveryEvent) (int32, int32) {
	var resourcesFound, relationshipsFound int32

	// Discover VPCs.
	vpcIDs := discoverVPCs(ctx, clients, ch, &resourcesFound, &relationshipsFound)

	// Discover subnets.
	discoverSubnets(ctx, clients, ch, &resourcesFound, &relationshipsFound)

	// Discover route tables.
	discoverRouteTables(ctx, clients, ch, &resourcesFound, &relationshipsFound)

	// Discover internet gateways.
	discoverInternetGateways(ctx, clients, ch, &resourcesFound, &relationshipsFound, vpcIDs)

	// Discover NAT gateways.
	discoverNATGateways(ctx, clients, ch, &resourcesFound, &relationshipsFound)

	return resourcesFound, relationshipsFound
}

// discoverVPCs lists all VPCs in the region.
func discoverVPCs(ctx context.Context, clients *AWSClients, ch chan<- plugintypes.DiscoveryEvent, resFound, relFound *int32) []string {
	var vpcIDs []string

	paginator := ec2.NewDescribeVpcsPaginator(clients.EC2, &ec2.DescribeVpcsInput{})

	err := Paginate(ctx, func() (bool, error) {
		if !paginator.HasMorePages() {
			return false, nil
		}

		page, err := paginator.NextPage(ctx)
		if err != nil {
			return false, fmt.Errorf("ec2:DescribeVpcs failed in %s: %w", clients.Region, err)
		}

		for _, vpc := range page.Vpcs {
			vpcID := aws.ToString(vpc.VpcId)
			name := getVPCName(vpc)

			res := sdk.NewResource("aws", types.TypeVPC, clients.Region, clients.AccountID, vpcID)
			res.Name = name

			props := res.Properties
			props["vpc_id"] = vpcID
			props["state"] = string(vpc.State)

			if vpc.CidrBlock != nil {
				props["cidr_block"] = *vpc.CidrBlock
			}
			if vpc.IsDefault != nil {
				props["is_default"] = fmt.Sprintf("%t", *vpc.IsDefault)
			}
			if vpc.InstanceTenancy != "" {
				props["instance_tenancy"] = string(vpc.InstanceTenancy)
			}
			if vpc.DhcpOptionsId != nil {
				props["dhcp_options_id"] = *vpc.DhcpOptionsId
			}

			// IPv6 CIDR blocks.
			var ipv6CIDRs []string
			for _, c := range vpc.Ipv6CidrBlockAssociationSet {
				if c.Ipv6CidrBlock != nil {
					ipv6CIDRs = append(ipv6CIDRs, *c.Ipv6CidrBlock)
				}
			}
			if len(ipv6CIDRs) > 0 {
				props["ipv6_cidr_blocks"] = strings.Join(ipv6CIDRs, ",")
			}

			// Tags.
			if len(vpc.Tags) > 0 {
				tagMap := make(map[string]string, len(vpc.Tags))
				for _, t := range vpc.Tags {
					if t.Key != nil && t.Value != nil {
						tagMap[*t.Key] = *t.Value
					}
				}
				res.Tags = sdk.TagsToStrings(tagMap)
			}

			rawData, _ := json.Marshal(vpc)
			res.RawData = rawData

			sdk.EmitResource(ch, res)
			*resFound++
			vpcIDs = append(vpcIDs, vpcID)
		}

		return paginator.HasMorePages(), nil
	}, 5)

	if err != nil {
		ch <- sdk.SendError("VPC_ERROR", fmt.Sprintf("VPC discovery failed in %s: %v", clients.Region, err), true)
	}

	return vpcIDs
}

// discoverSubnets lists all subnets in the region.
func discoverSubnets(ctx context.Context, clients *AWSClients, ch chan<- plugintypes.DiscoveryEvent, resFound, relFound *int32) {
	paginator := ec2.NewDescribeSubnetsPaginator(clients.EC2, &ec2.DescribeSubnetsInput{})

	err := Paginate(ctx, func() (bool, error) {
		if !paginator.HasMorePages() {
			return false, nil
		}

		page, err := paginator.NextPage(ctx)
		if err != nil {
			return false, fmt.Errorf("ec2:DescribeSubnets failed in %s: %w", clients.Region, err)
		}

		for _, subnet := range page.Subnets {
			subnetID := aws.ToString(subnet.SubnetId)
			name := getSubnetName(subnet)

			res := sdk.NewResource("aws", types.TypeSubnet, clients.Region, clients.AccountID, subnetID)
			res.Name = name

			props := res.Properties
			props["subnet_id"] = subnetID
			props["state"] = string(subnet.State)

			if subnet.VpcId != nil {
				props["vpc_id"] = *subnet.VpcId
			}
			if subnet.CidrBlock != nil {
				props["cidr_block"] = *subnet.CidrBlock
			}
			if subnet.AvailabilityZone != nil {
				props["availability_zone"] = *subnet.AvailabilityZone
			}
			if subnet.AvailableIpAddressCount != nil {
				props["available_ips"] = fmt.Sprintf("%d", *subnet.AvailableIpAddressCount)
			}
			if subnet.DefaultForAz != nil {
				props["default_for_az"] = fmt.Sprintf("%t", *subnet.DefaultForAz)
			}
			if subnet.MapPublicIpOnLaunch != nil {
				props["map_public_ip"] = fmt.Sprintf("%t", *subnet.MapPublicIpOnLaunch)
			}
			if subnet.OwnerId != nil {
				props["owner_id"] = *subnet.OwnerId
			}

			// IPv6 CIDR blocks.
			var ipv6CIDRs []string
			for _, c := range subnet.Ipv6CidrBlockAssociationSet {
				if c.Ipv6CidrBlock != nil {
					ipv6CIDRs = append(ipv6CIDRs, *c.Ipv6CidrBlock)
				}
			}
			if len(ipv6CIDRs) > 0 {
				props["ipv6_cidr_blocks"] = strings.Join(ipv6CIDRs, ",")
			}

			// Tags.
			if len(subnet.Tags) > 0 {
				tagMap := make(map[string]string, len(subnet.Tags))
				for _, t := range subnet.Tags {
					if t.Key != nil && t.Value != nil {
						tagMap[*t.Key] = *t.Value
					}
				}
				res.Tags = sdk.TagsToStrings(tagMap)
			}

			rawData, _ := json.Marshal(subnet)
			res.RawData = rawData

			sdk.EmitResource(ch, res)
			*resFound++

			// CONTAINS: VPC -> Subnet.
			if subnet.VpcId != nil {
				vpcID := fmt.Sprintf("%s:%s:%s:%s:%s", "aws", types.TypeVPC, clients.Region, clients.AccountID, *subnet.VpcId)
				rel := sdk.NewRelationship(vpcID, res.ID, types.RelContains)
				sdk.EmitRelationship(ch, rel)
				*relFound++
			}
		}

		return paginator.HasMorePages(), nil
	}, 5)

	if err != nil {
		ch <- sdk.SendError("SUBNET_ERROR", fmt.Sprintf("Subnet discovery failed in %s: %v", clients.Region, err), true)
	}
}

// discoverRouteTables lists all route tables and creates ROUTES_TO relationships.
func discoverRouteTables(ctx context.Context, clients *AWSClients, ch chan<- plugintypes.DiscoveryEvent, resFound, relFound *int32) {
	paginator := ec2.NewDescribeRouteTablesPaginator(clients.EC2, &ec2.DescribeRouteTablesInput{})

	err := Paginate(ctx, func() (bool, error) {
		if !paginator.HasMorePages() {
			return false, nil
		}

		page, err := paginator.NextPage(ctx)
		if err != nil {
			return false, fmt.Errorf("ec2:DescribeRouteTables failed in %s: %w", clients.Region, err)
		}

		for _, rt := range page.RouteTables {
			rtID := aws.ToString(rt.RouteTableId)
			name := getResourceName(rt.Tags)

			res := sdk.NewResource("aws", types.TypeRouteTable, clients.Region, clients.AccountID, rtID)
			res.Name = name

			props := res.Properties
			props["route_table_id"] = rtID

			if rt.VpcId != nil {
				props["vpc_id"] = *rt.VpcId
			}

			// Main route table association.
			for _, assoc := range rt.Associations {
				if assoc.Main != nil && *assoc.Main {
					props["is_main"] = "true"
				}
				if assoc.SubnetId != nil {
					props["associated_subnet"] = *assoc.SubnetId
				}
			}

			// Routes summary.
			var routes []string
			for _, route := range rt.Routes {
				dest := aws.ToString(route.DestinationCidrBlock)
				if dest == "" {
					dest = aws.ToString(route.DestinationIpv6CidrBlock)
				}
				target := ""
				if route.GatewayId != nil {
					target = *route.GatewayId
				} else if route.NatGatewayId != nil {
					target = *route.NatGatewayId
				} else if route.TransitGatewayId != nil {
					target = *route.TransitGatewayId
				} else if route.VpcPeeringConnectionId != nil {
					target = *route.VpcPeeringConnectionId
				} else if route.NetworkInterfaceId != nil {
					target = *route.NetworkInterfaceId
				} else if route.InstanceId != nil {
					target = *route.InstanceId
				}
				if dest != "" && target != "" {
					routes = append(routes, fmt.Sprintf("%s->%s", dest, target))
				}
			}
			if len(routes) > 0 {
				props["routes"] = strings.Join(routes, "; ")
			}

			// Tags.
			if len(rt.Tags) > 0 {
				tagMap := make(map[string]string, len(rt.Tags))
				for _, t := range rt.Tags {
					if t.Key != nil && t.Value != nil {
						tagMap[*t.Key] = *t.Value
					}
				}
				res.Tags = sdk.TagsToStrings(tagMap)
			}

			rawData, _ := json.Marshal(rt)
			res.RawData = rawData

			sdk.EmitResource(ch, res)
			*resFound++

			// CONTAINS: VPC -> RouteTable.
			if rt.VpcId != nil {
				vpcID := fmt.Sprintf("%s:%s:%s:%s:%s", "aws", types.TypeVPC, clients.Region, clients.AccountID, *rt.VpcId)
				rel := sdk.NewRelationship(vpcID, res.ID, types.RelContains)
				sdk.EmitRelationship(ch, rel)
				*relFound++
			}

			// ROUTES_TO: RouteTable -> IGW / NAT Gateway / Transit Gateway.
			for _, route := range rt.Routes {
				if route.GatewayId != nil && strings.HasPrefix(*route.GatewayId, "igw-") {
					igwResID := fmt.Sprintf("%s:%s:%s:%s:%s", "aws", types.TypeInternetGateway, clients.Region, clients.AccountID, *route.GatewayId)
					rel := sdk.NewRelationship(res.ID, igwResID, types.RelRoutesTo)
					if route.DestinationCidrBlock != nil {
						rel.Properties["destination"] = *route.DestinationCidrBlock
					}
					sdk.EmitRelationship(ch, rel)
					*relFound++
				}
				if route.NatGatewayId != nil {
					natResID := fmt.Sprintf("%s:%s:%s:%s:%s", "aws", types.TypeNATGateway, clients.Region, clients.AccountID, *route.NatGatewayId)
					rel := sdk.NewRelationship(res.ID, natResID, types.RelRoutesTo)
					if route.DestinationCidrBlock != nil {
						rel.Properties["destination"] = *route.DestinationCidrBlock
					}
					sdk.EmitRelationship(ch, rel)
					*relFound++
				}
			}
		}

		return paginator.HasMorePages(), nil
	}, 5)

	if err != nil {
		ch <- sdk.SendError("ROUTE_TABLE_ERROR", fmt.Sprintf("Route table discovery failed in %s: %v", clients.Region, err), true)
	}
}

// discoverInternetGateways lists all internet gateways.
func discoverInternetGateways(ctx context.Context, clients *AWSClients, ch chan<- plugintypes.DiscoveryEvent, resFound, relFound *int32, vpcIDs []string) {
	paginator := ec2.NewDescribeInternetGatewaysPaginator(clients.EC2, &ec2.DescribeInternetGatewaysInput{})

	err := Paginate(ctx, func() (bool, error) {
		if !paginator.HasMorePages() {
			return false, nil
		}

		page, err := paginator.NextPage(ctx)
		if err != nil {
			return false, fmt.Errorf("ec2:DescribeInternetGateways failed in %s: %w", clients.Region, err)
		}

		for _, igw := range page.InternetGateways {
			igwID := aws.ToString(igw.InternetGatewayId)
			name := getResourceName(igw.Tags)

			res := sdk.NewResource("aws", types.TypeInternetGateway, clients.Region, clients.AccountID, igwID)
			res.Name = name

			props := res.Properties
			props["internet_gateway_id"] = igwID

			if igw.OwnerId != nil {
				props["owner_id"] = *igw.OwnerId
			}

			// Attachments.
			var attachedVPCs []string
			for _, att := range igw.Attachments {
				if att.VpcId != nil {
					attachedVPCs = append(attachedVPCs, *att.VpcId)
				}
				if att.State != "" {
					props["state"] = string(att.State)
				}
			}
			if len(attachedVPCs) > 0 {
				props["attached_vpcs"] = strings.Join(attachedVPCs, ",")
			}

			// Tags.
			if len(igw.Tags) > 0 {
				tagMap := make(map[string]string, len(igw.Tags))
				for _, t := range igw.Tags {
					if t.Key != nil && t.Value != nil {
						tagMap[*t.Key] = *t.Value
					}
				}
				res.Tags = sdk.TagsToStrings(tagMap)
			}

			rawData, _ := json.Marshal(igw)
			res.RawData = rawData

			sdk.EmitResource(ch, res)
			*resFound++

			// CONTAINS: VPC -> IGW.
			for _, att := range igw.Attachments {
				if att.VpcId != nil {
					vpcID := fmt.Sprintf("%s:%s:%s:%s:%s", "aws", types.TypeVPC, clients.Region, clients.AccountID, *att.VpcId)
					rel := sdk.NewRelationship(vpcID, res.ID, types.RelContains)
					sdk.EmitRelationship(ch, rel)
					*relFound++
				}
			}
		}

		return paginator.HasMorePages(), nil
	}, 5)

	if err != nil {
		ch <- sdk.SendError("IGW_ERROR", fmt.Sprintf("Internet gateway discovery failed in %s: %v", clients.Region, err), true)
	}
}

// discoverNATGateways lists all NAT gateways.
func discoverNATGateways(ctx context.Context, clients *AWSClients, ch chan<- plugintypes.DiscoveryEvent, resFound, relFound *int32) {
	paginator := ec2.NewDescribeNatGatewaysPaginator(clients.EC2, &ec2.DescribeNatGatewaysInput{})

	err := Paginate(ctx, func() (bool, error) {
		if !paginator.HasMorePages() {
			return false, nil
		}

		page, err := paginator.NextPage(ctx)
		if err != nil {
			return false, fmt.Errorf("ec2:DescribeNatGateways failed in %s: %w", clients.Region, err)
		}

		for _, nat := range page.NatGateways {
			natID := aws.ToString(nat.NatGatewayId)
			name := getResourceName(nat.Tags)

			res := sdk.NewResource("aws", types.TypeNATGateway, clients.Region, clients.AccountID, natID)
			res.Name = name

			props := res.Properties
			props["nat_gateway_id"] = natID
			props["state"] = string(nat.State)

			if nat.VpcId != nil {
				props["vpc_id"] = *nat.VpcId
			}
			if nat.SubnetId != nil {
				props["subnet_id"] = *nat.SubnetId
			}
			if nat.ConnectivityType != "" {
				props["connectivity_type"] = string(nat.ConnectivityType)
			}

			// NAT gateway addresses.
			var eips []string
			for _, addr := range nat.NatGatewayAddresses {
				if addr.PublicIp != nil {
					eips = append(eips, *addr.PublicIp)
				}
				if addr.AllocationId != nil {
					props["eip_allocation_id"] = *addr.AllocationId
				}
			}
			if len(eips) > 0 {
				props["elastic_ips"] = strings.Join(eips, ",")
			}

			// Tags.
			if len(nat.Tags) > 0 {
				tagMap := make(map[string]string, len(nat.Tags))
				for _, t := range nat.Tags {
					if t.Key != nil && t.Value != nil {
						tagMap[*t.Key] = *t.Value
					}
				}
				res.Tags = sdk.TagsToStrings(tagMap)
			}

			rawData, _ := json.Marshal(nat)
			res.RawData = rawData

			sdk.EmitResource(ch, res)
			*resFound++

			// CONTAINS: VPC -> NAT Gateway.
			if nat.VpcId != nil {
				vpcID := fmt.Sprintf("%s:%s:%s:%s:%s", "aws", types.TypeVPC, clients.Region, clients.AccountID, *nat.VpcId)
				rel := sdk.NewRelationship(vpcID, res.ID, types.RelContains)
				sdk.EmitRelationship(ch, rel)
				*relFound++
			}
		}

		return paginator.HasMorePages(), nil
	}, 5)

	if err != nil {
		ch <- sdk.SendError("NAT_ERROR", fmt.Sprintf("NAT gateway discovery failed in %s: %v", clients.Region, err), true)
	}
}

// getVPCName extracts the Name tag from a VPC.
func getVPCName(vpc ec2types.Vpc) string {
	for _, t := range vpc.Tags {
		if t.Key != nil && *t.Key == "Name" && t.Value != nil {
			return *t.Value
		}
	}
	if vpc.VpcId != nil {
		return *vpc.VpcId
	}
	return "unknown"
}

// getSubnetName extracts the Name tag from a subnet.
func getSubnetName(subnet ec2types.Subnet) string {
	for _, t := range subnet.Tags {
		if t.Key != nil && *t.Key == "Name" && t.Value != nil {
			return *t.Value
		}
	}
	if subnet.SubnetId != nil {
		return *subnet.SubnetId
	}
	return "unknown"
}

// getResourceName extracts the Name tag from a generic tag list.
func getResourceName(tags []ec2types.Tag) string {
	for _, t := range tags {
		if t.Key != nil && *t.Key == "Name" && t.Value != nil {
			return *t.Value
		}
	}
	return ""
}
