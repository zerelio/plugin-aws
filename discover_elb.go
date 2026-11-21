package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	elbv2 "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"

	plugintypes "github.com/zerelio/plugin-sdk/plugin"
	"github.com/zerelio/plugin-sdk/types"

	sdk "github.com/zerelio/plugin-sdk"
)

// DiscoverELB discovers ALB/NLB/CLB load balancers, target groups, and listeners.
// It creates CONNECTS_TO relationships (ELB -> TargetGroup -> Instance) and
// ROUTES_TO relationships (ELB -> Subnets).
func DiscoverELB(ctx context.Context, clients *AWSClients, ch chan<- plugintypes.DiscoveryEvent) (int32, int32) {
	var resourcesFound, relationshipsFound int32

	// Discover target groups first (needed for ELB relationships).
	tgMap := discoverTargetGroups(ctx, clients, ch, &resourcesFound, &relationshipsFound)

	// Discover load balancers.
	discoverLoadBalancers(ctx, clients, ch, &resourcesFound, &relationshipsFound, tgMap)

	return resourcesFound, relationshipsFound
}

// discoverLoadBalancers discovers all ALB and NLB load balancers (ELBv2).
func discoverLoadBalancers(ctx context.Context, clients *AWSClients, ch chan<- plugintypes.DiscoveryEvent, resFound, relFound *int32, tgMap map[string]string) {
	paginator := elbv2.NewDescribeLoadBalancersPaginator(clients.ELBv2, &elbv2.DescribeLoadBalancersInput{})

	err := Paginate(ctx, func() (bool, error) {
		if !paginator.HasMorePages() {
			return false, nil
		}

		page, err := paginator.NextPage(ctx)
		if err != nil {
			return false, fmt.Errorf("elbv2:DescribeLoadBalancers failed in %s: %w", clients.Region, err)
		}

		for _, lb := range page.LoadBalancers {
			lbName := aws.ToString(lb.LoadBalancerName)
			lbARN := aws.ToString(lb.LoadBalancerArn)

			res := sdk.NewResourceWithID(lbARN, "aws", types.TypeELB, clients.Region, clients.AccountID, lbName)
			res.Name = lbName

			props := res.Properties
			props["load_balancer_name"] = lbName
			props["arn"] = lbARN

			if lb.DNSName != nil {
				props["dns_name"] = *lb.DNSName
			}
			if lb.Scheme != "" {
				props["scheme"] = string(lb.Scheme)
			}
			if lb.Type != "" {
				props["type"] = string(lb.Type)
			}
			if lb.State != nil && lb.State.Code != "" {
				props["state"] = string(lb.State.Code)
			}
			if lb.VpcId != nil {
				props["vpc_id"] = *lb.VpcId
			}
			if lb.IpAddressType != "" {
				props["ip_address_type"] = string(lb.IpAddressType)
			}
			if lb.CreatedTime != nil {
				props["created_time"] = lb.CreatedTime.Format("2006-01-02T15:04:05Z")
			}

			// Availability zones and subnets.
			var subnetIDs, azs []string
			for _, az := range lb.AvailabilityZones {
				if az.SubnetId != nil {
					subnetIDs = append(subnetIDs, *az.SubnetId)
				}
				if az.ZoneName != nil {
					azs = append(azs, *az.ZoneName)
				}
			}
			if len(subnetIDs) > 0 {
				props["subnet_ids"] = strings.Join(subnetIDs, ",")
			}
			if len(azs) > 0 {
				props["availability_zones"] = strings.Join(azs, ",")
			}

			// Security groups.
			var sgIDs []string
			for _, sgID := range lb.SecurityGroups {
				sgIDs = append(sgIDs, sgID)
			}
			if len(sgIDs) > 0 {
				props["security_group_ids"] = strings.Join(sgIDs, ",")
			}

			// Tags.
			tagResult, err := clients.ELBv2.DescribeTags(ctx, &elbv2.DescribeTagsInput{
				ResourceArns: []string{lbARN},
			})
			if err == nil && len(tagResult.TagDescriptions) > 0 {
				for _, desc := range tagResult.TagDescriptions {
					if len(desc.Tags) > 0 {
						tagMap := make(map[string]string, len(desc.Tags))
						for _, t := range desc.Tags {
							if t.Key != nil && t.Value != nil {
								tagMap[*t.Key] = *t.Value
							}
						}
						res.Tags = sdk.TagsToStrings(tagMap)
					}
				}
			}

			rawData, _ := json.Marshal(lb)
			res.RawData = rawData

			sdk.EmitResource(ch, res)
			*resFound++

			// ROUTES_TO: ELB -> Subnets.
			for _, az := range lb.AvailabilityZones {
				if az.SubnetId != nil {
					subnetResID := fmt.Sprintf("%s:%s:%s:%s:%s", "aws", types.TypeSubnet, clients.Region, clients.AccountID, *az.SubnetId)
					rel := sdk.NewRelationship(res.ID, subnetResID, types.RelRoutesTo)
					sdk.EmitRelationship(ch, rel)
					*relFound++
				}
			}

			// CONNECTS_TO: ELB -> Security Groups.
			for _, sgID := range lb.SecurityGroups {
				sgResID := fmt.Sprintf("%s:%s:%s:%s:%s", "aws", types.TypeEC2SecurityGroup, clients.Region, clients.AccountID, sgID)
				rel := sdk.NewRelationship(res.ID, sgResID, types.RelConnectsTo)
				sdk.EmitRelationship(ch, rel)
				*relFound++
			}

			// Discover listeners for this load balancer.
			discoverListeners(ctx, clients, ch, lbARN, res.ID, resFound, relFound, tgMap)
		}

		return paginator.HasMorePages(), nil
	}, 5)

	if err != nil {
		ch <- sdk.SendError("ELB_ERROR", fmt.Sprintf("ELB discovery failed in %s: %v", clients.Region, err), true)
	}
}

// discoverListeners discovers listeners for a load balancer.
func discoverListeners(ctx context.Context, clients *AWSClients, ch chan<- plugintypes.DiscoveryEvent, lbARN, lbResID string, resFound, relFound *int32, tgMap map[string]string) {
	paginator := elbv2.NewDescribeListenersPaginator(clients.ELBv2, &elbv2.DescribeListenersInput{
		LoadBalancerArn: aws.String(lbARN),
	})

	err := Paginate(ctx, func() (bool, error) {
		if !paginator.HasMorePages() {
			return false, nil
		}

		page, err := paginator.NextPage(ctx)
		if err != nil {
			return false, fmt.Errorf("elbv2:DescribeListeners failed: %w", err)
		}

		for _, listener := range page.Listeners {
			listenerARN := aws.ToString(listener.ListenerArn)
			listenerID := fmt.Sprintf("listener:%s", listenerARN)

			res := sdk.NewResourceWithID(listenerARN, "aws", types.TypeELBListener, clients.Region, clients.AccountID, listenerID)
			res.Name = fmt.Sprintf("listener-%d", aws.ToInt32(listener.Port))

			props := res.Properties
			props["listener_arn"] = listenerARN
			props["load_balancer_arn"] = lbARN

			if listener.Port != nil {
				props["port"] = fmt.Sprintf("%d", *listener.Port)
			}
			if listener.Protocol != "" {
				props["protocol"] = string(listener.Protocol)
			}
			if listener.SslPolicy != nil {
				props["ssl_policy"] = *listener.SslPolicy
			}

			// Default actions.
			var defaultActions []string
			for _, action := range listener.DefaultActions {
				if action.TargetGroupArn != nil {
					defaultActions = append(defaultActions, fmt.Sprintf("forward:%s", *action.TargetGroupArn))
				} else if action.Type != "" {
					defaultActions = append(defaultActions, string(action.Type))
				}
			}
			if len(defaultActions) > 0 {
				props["default_actions"] = strings.Join(defaultActions, ";")
			}

			rawData, _ := json.Marshal(listener)
			res.RawData = rawData

			sdk.EmitResource(ch, res)
			*resFound++

			// CONTAINS: ELB -> Listener.
			rel := sdk.NewRelationship(lbResID, res.ID, types.RelContains)
			sdk.EmitRelationship(ch, rel)
			*relFound++

			// ROUTES_TO: Listener -> Target Group (from default actions).
			for _, action := range listener.DefaultActions {
				if action.TargetGroupArn != nil {
					tgARN := *action.TargetGroupArn
					if tgResID, ok := tgMap[tgARN]; ok {
						rel := sdk.NewRelationship(res.ID, tgResID, types.RelRoutesTo)
						sdk.EmitRelationship(ch, rel)
						*relFound++
					}
				}
			}
		}

		return paginator.HasMorePages(), nil
	}, 5)

	if err != nil {
		// Non-fatal: listener discovery failure should not stop ELB discovery.
		ch <- sdk.SendError("ELB_LISTENER_ERROR", fmt.Sprintf("Listener discovery failed for %s: %v", lbARN, err), true)
	}
}

// discoverTargetGroups discovers all target groups and returns a map of
// target group ARN -> resource ID.
func discoverTargetGroups(ctx context.Context, clients *AWSClients, ch chan<- plugintypes.DiscoveryEvent, resFound, relFound *int32) map[string]string {
	tgMap := make(map[string]string)

	paginator := elbv2.NewDescribeTargetGroupsPaginator(clients.ELBv2, &elbv2.DescribeTargetGroupsInput{})

	err := Paginate(ctx, func() (bool, error) {
		if !paginator.HasMorePages() {
			return false, nil
		}

		page, err := paginator.NextPage(ctx)
		if err != nil {
			return false, fmt.Errorf("elbv2:DescribeTargetGroups failed in %s: %w", clients.Region, err)
		}

		for _, tg := range page.TargetGroups {
			tgName := aws.ToString(tg.TargetGroupName)
			tgARN := aws.ToString(tg.TargetGroupArn)

			res := sdk.NewResourceWithID(tgARN, "aws", types.TypeELBTargetGroup, clients.Region, clients.AccountID, tgName)
			res.Name = tgName

			props := res.Properties
			props["target_group_name"] = tgName
			props["arn"] = tgARN

			if tg.Protocol != "" {
				props["protocol"] = string(tg.Protocol)
			}
			if tg.Port != nil {
				props["port"] = fmt.Sprintf("%d", *tg.Port)
			}
			if tg.VpcId != nil {
				props["vpc_id"] = *tg.VpcId
			}
			if tg.HealthCheckProtocol != "" {
				props["health_check_protocol"] = string(tg.HealthCheckProtocol)
			}
			if tg.HealthCheckPort != nil {
				props["health_check_port"] = *tg.HealthCheckPort
			}
			if tg.HealthCheckPath != nil {
				props["health_check_path"] = *tg.HealthCheckPath
			}
			if tg.HealthCheckIntervalSeconds != nil {
				props["health_check_interval"] = fmt.Sprintf("%d", *tg.HealthCheckIntervalSeconds)
			}
			if tg.HealthCheckTimeoutSeconds != nil {
				props["health_check_timeout"] = fmt.Sprintf("%d", *tg.HealthCheckTimeoutSeconds)
			}
			if tg.HealthyThresholdCount != nil {
				props["healthy_threshold"] = fmt.Sprintf("%d", *tg.HealthyThresholdCount)
			}
			if tg.UnhealthyThresholdCount != nil {
				props["unhealthy_threshold"] = fmt.Sprintf("%d", *tg.UnhealthyThresholdCount)
			}
			if tg.TargetType != "" {
				props["target_type"] = string(tg.TargetType)
			}
			if tg.IpAddressType != "" {
				props["ip_address_type"] = string(tg.IpAddressType)
			}

			// Load balancer ARNs.
			if len(tg.LoadBalancerArns) > 0 {
				props["load_balancer_arns"] = strings.Join(tg.LoadBalancerArns, ",")
			}

			// Tags.
			tagResult, err := clients.ELBv2.DescribeTags(ctx, &elbv2.DescribeTagsInput{
				ResourceArns: []string{tgARN},
			})
			if err == nil && len(tagResult.TagDescriptions) > 0 {
				for _, desc := range tagResult.TagDescriptions {
					if len(desc.Tags) > 0 {
						tagMap := make(map[string]string, len(desc.Tags))
						for _, t := range desc.Tags {
							if t.Key != nil && t.Value != nil {
								tagMap[*t.Key] = *t.Value
							}
						}
						res.Tags = sdk.TagsToStrings(tagMap)
					}
				}
			}

			rawData, _ := json.Marshal(tg)
			res.RawData = rawData

			sdk.EmitResource(ch, res)
			*resFound++

			tgMap[tgARN] = res.ID

			// CONNECTS_TO: TargetGroup -> registered targets (instances).
			discoverTargetHealth(ctx, clients, ch, tgARN, res.ID, relFound)
		}

		return paginator.HasMorePages(), nil
	}, 5)

	if err != nil {
		ch <- sdk.SendError("TARGET_GROUP_ERROR", fmt.Sprintf("Target group discovery failed in %s: %v", clients.Region, err), true)
	}

	return tgMap
}

// discoverTargetHealth discovers registered targets in a target group and
// creates CONNECTS_TO relationships.
func discoverTargetHealth(ctx context.Context, clients *AWSClients, ch chan<- plugintypes.DiscoveryEvent, tgARN, tgResID string, relFound *int32) {
	result, err := clients.ELBv2.DescribeTargetHealth(ctx, &elbv2.DescribeTargetHealthInput{
		TargetGroupArn: aws.String(tgARN),
	})
	if err != nil {
		return
	}

	for _, desc := range result.TargetHealthDescriptions {
		if desc.Target == nil {
			continue
		}

		target := desc.Target
		if target.Id != nil {
			targetID := *target.Id
			// Determine if target is an instance or IP/Lambda.
			if target.Port != nil {
				targetID = fmt.Sprintf("%s:%d", targetID, *target.Port)
			}

			rel := sdk.NewRelationship(tgResID, targetID, types.RelConnectsTo)
			if desc.TargetHealth != nil {
				rel.Properties["health_status"] = string(desc.TargetHealth.State)
			}
			if target.AvailabilityZone != nil {
				rel.Properties["availability_zone"] = *target.AvailabilityZone
			}
			sdk.EmitRelationship(ch, rel)
		}
	}
}
