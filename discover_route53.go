package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	r53types "github.com/aws/aws-sdk-go-v2/service/route53/types"

	plugintypes "github.com/zerelio/plugin-sdk/plugin"
	"github.com/zerelio/plugin-sdk/types"

	sdk "github.com/zerelio/plugin-sdk"
)

// DiscoverRoute53 discovers Route53 hosted zones and record sets. It creates
// CONTAINS relationships (Account -> Zone -> Record) and ROUTES_TO relationships
// (Record -> ELB / CloudFront / instance).
func DiscoverRoute53(ctx context.Context, clients *AWSClients, ch chan<- plugintypes.DiscoveryEvent) (int32, int32) {
	var resourcesFound, relationshipsFound int32

	// List hosted zones.
	paginator := route53.NewListHostedZonesPaginator(clients.Route53, &route53.ListHostedZonesInput{})

	err := Paginate(ctx, func() (bool, error) {
		if !paginator.HasMorePages() {
			return false, nil
		}

		page, err := paginator.NextPage(ctx)
		if err != nil {
			return false, fmt.Errorf("route53:ListHostedZones failed: %w", err)
		}

		for _, zone := range page.HostedZones {
			zoneID := aws.ToString(zone.Id)
			zoneName := strings.TrimSuffix(aws.ToString(zone.Name), ".")
			zoneARN := fmt.Sprintf("arn:aws:route53::%s:hostedzone/%s", clients.AccountID, strings.TrimPrefix(zoneID, "/hostedzone/"))

			res := sdk.NewResourceWithID(zoneARN, "aws", types.TypeRoute53Zone, "global", clients.AccountID, zoneID)
			res.Name = zoneName

			props := res.Properties
			props["zone_id"] = zoneID
			props["zone_name"] = zoneName
			props["arn"] = zoneARN

			if zone.Config != nil {
				if zone.Config.PrivateZone {
					props["private"] = fmt.Sprintf("%t", zone.Config.PrivateZone)
				}
				if zone.Config.Comment != nil {
					props["comment"] = *zone.Config.Comment
				}
			}

			if zone.ResourceRecordSetCount != nil {
				props["record_count"] = fmt.Sprintf("%d", *zone.ResourceRecordSetCount)
			}

			// Tags for the hosted zone.
			zoneIDClean := strings.TrimPrefix(zoneID, "/hostedzone/")
			tagResult, err := clients.Route53.ListTagsForResource(ctx, &route53.ListTagsForResourceInput{
				ResourceType: r53types.TagResourceTypeHostedzone,
				ResourceId:   aws.String(zoneIDClean),
			})
			if err == nil && tagResult.ResourceTagSet != nil && len(tagResult.ResourceTagSet.Tags) > 0 {
				tagMap := make(map[string]string, len(tagResult.ResourceTagSet.Tags))
				for _, t := range tagResult.ResourceTagSet.Tags {
					if t.Key != nil && t.Value != nil {
						tagMap[*t.Key] = *t.Value
					}
				}
				res.Tags = sdk.TagsToStrings(tagMap)
			}

			rawData, _ := json.Marshal(zone)
			res.RawData = rawData

			sdk.EmitResource(ch, res)
			resourcesFound++

			// CONTAINS: Account -> Zone.
			accountID := fmt.Sprintf("aws:account:%s:%s", "global", clients.AccountID)
			rel := sdk.NewRelationship(accountID, res.ID, types.RelContains)
			rel.Properties["service"] = "route53"
			sdk.EmitRelationship(ch, rel)
			relationshipsFound++

			// Discover records for this zone.
			rCount, relCount := discoverRecordsForZone(ctx, clients, ch, zoneID, res.ID)
			resourcesFound += rCount
			relationshipsFound += relCount
		}

		return paginator.HasMorePages(), nil
	}, 5)

	if err != nil {
		ch <- sdk.SendError("ROUTE53_ERROR", fmt.Sprintf("Route53 discovery failed: %v", err), true)
	}

	return resourcesFound, relationshipsFound
}

// discoverRecordsForZone discovers all record sets in a hosted zone.
func discoverRecordsForZone(ctx context.Context, clients *AWSClients, ch chan<- plugintypes.DiscoveryEvent, zoneID, zoneResID string) (int32, int32) {
	var resourcesFound, relationshipsFound int32

	paginator := route53.NewListResourceRecordSetsPaginator(clients.Route53, &route53.ListResourceRecordSetsInput{
		HostedZoneId: aws.String(zoneID),
	})

	err := Paginate(ctx, func() (bool, error) {
		if !paginator.HasMorePages() {
			return false, nil
		}

		page, err := paginator.NextPage(ctx)
		if err != nil {
			return false, fmt.Errorf("route53:ListResourceRecordSets for zone %s failed: %w", zoneID, err)
		}

		for _, record := range page.ResourceRecordSets {
			recordName := strings.TrimSuffix(aws.ToString(record.Name), ".")
			recordType := string(record.Type)

			// Skip NS and SOA records for the zone apex (they are implicit).
			if recordType == "SOA" {
				continue
			}

			recordID := fmt.Sprintf("%s:%s:%s", strings.TrimPrefix(zoneID, "/hostedzone/"), recordType, recordName)

			res := sdk.NewResourceWithID(
				fmt.Sprintf("route53:record:%s", recordID),
				"aws", types.TypeRoute53Record, "global", clients.AccountID, recordID,
			)
			res.Name = recordName

			props := res.Properties
			props["record_name"] = recordName
			props["record_type"] = recordType
			props["zone_id"] = zoneID

			if record.TTL != nil {
				props["ttl"] = fmt.Sprintf("%d", *record.TTL)
			}
			if record.SetIdentifier != nil {
				props["set_identifier"] = *record.SetIdentifier
			}
			if record.Weight != nil {
				props["weight"] = fmt.Sprintf("%d", *record.Weight)
			}

			// Resource records (for A, CNAME, MX, TXT, etc.).
			var values []string
			for _, rr := range record.ResourceRecords {
				if rr.Value != nil {
					values = append(values, *rr.Value)
				}
			}
			if len(values) > 0 {
				props["values"] = strings.Join(values, ",")
			}

			// Alias target (for Alias records pointing to ELB, CloudFront, etc.).
			if record.AliasTarget != nil {
				if record.AliasTarget.DNSName != nil {
					props["alias_dns_name"] = *record.AliasTarget.DNSName
				}
				if record.AliasTarget.EvaluateTargetHealth {
					props["evaluate_target_health"] = fmt.Sprintf("%t", record.AliasTarget.EvaluateTargetHealth)
				}
				if record.AliasTarget.HostedZoneId != nil {
					props["alias_hosted_zone_id"] = *record.AliasTarget.HostedZoneId
				}
			}

			// Traffic policy.
			if record.TrafficPolicyInstanceId != nil {
				props["traffic_policy_instance_id"] = *record.TrafficPolicyInstanceId
			}

			// Geo/region routing.
			if record.GeoLocation != nil {
				if record.GeoLocation.ContinentCode != nil {
					props["geo_continent"] = *record.GeoLocation.ContinentCode
				}
				if record.GeoLocation.CountryCode != nil {
					props["geo_country"] = *record.GeoLocation.CountryCode
				}
			}

			rawData, _ := json.Marshal(record)
			res.RawData = rawData

			sdk.EmitResource(ch, res)
			resourcesFound++

			// CONTAINS: Zone -> Record.
			rel := sdk.NewRelationship(zoneResID, res.ID, types.RelContains)
			sdk.EmitRelationship(ch, rel)
			relationshipsFound++

			// ROUTES_TO: Record -> ELB / CloudFront / instance.
			if record.AliasTarget != nil && record.AliasTarget.DNSName != nil {
				dnsName := strings.TrimSuffix(*record.AliasTarget.DNSName, ".")
				targetID := identifyRecordTarget(dnsName, record.AliasTarget.HostedZoneId)
				if targetID != "" {
					rel := sdk.NewRelationship(res.ID, targetID, types.RelRoutesTo)
					rel.Properties["dns_name"] = dnsName
					sdk.EmitRelationship(ch, rel)
					relationshipsFound++
				}
			}

			// ROUTES_TO for CNAME records.
			if recordType == "CNAME" && len(values) > 0 {
				cnameTarget := strings.TrimSuffix(values[0], ".")
				targetID := identifyRecordTarget(cnameTarget, nil)
				if targetID != "" {
					rel := sdk.NewRelationship(res.ID, targetID, types.RelRoutesTo)
					rel.Properties["dns_name"] = cnameTarget
					sdk.EmitRelationship(ch, rel)
					relationshipsFound++
				}
			}
		}

		return paginator.HasMorePages(), nil
	}, 5)

	if err != nil {
		ch <- sdk.SendError("ROUTE53_RECORDS_ERROR", fmt.Sprintf("Route53 record discovery failed for zone %s: %v", zoneID, err), true)
	}

	return resourcesFound, relationshipsFound
}

// identifyRecordTarget attempts to identify what infrastructure a DNS record
// points to based on its DNS name. It returns a resource ID for known patterns.
func identifyRecordTarget(dnsName string, hostedZoneID *string) string {
	// ELB pattern: <name>-<id>.<region>.elb.amazonaws.com
	if strings.HasSuffix(dnsName, ".elb.amazonaws.com") {
		parts := strings.Split(dnsName, ".")
		if len(parts) >= 4 {
			// Extract region from the DNS name.
			region := parts[2]
			elbName := parts[0]
			elbResID := fmt.Sprintf("aws:%s:%s:%s:%s", "aws", types.TypeELB, region, elbName)
			return elbResID
		}
	}

	// CloudFront pattern: <distribution>.cloudfront.net
	if strings.HasSuffix(dnsName, ".cloudfront.net") {
		cfDistID := strings.TrimSuffix(dnsName, ".cloudfront.net")
		return fmt.Sprintf("aws:%s:global:%s:%s", "aws", types.TypeCloudFrontDist, cfDistID)
	}

	// S3 website pattern: <bucket>.s3-website-<region>.amazonaws.com
	if strings.Contains(dnsName, ".s3-website-") && strings.HasSuffix(dnsName, ".amazonaws.com") {
		parts := strings.Split(dnsName, ".")
		if len(parts) >= 1 {
			return fmt.Sprintf("aws:%s:global:%s:%s", "aws", types.TypeS3Bucket, parts[0])
		}
	}

	// ELB (dualstack) pattern: dualstack.<name>-<id>.<region>.elb.amazonaws.com
	if strings.HasPrefix(dnsName, "dualstack.") && strings.HasSuffix(dnsName, ".elb.amazonaws.com") {
		trimmed := strings.TrimPrefix(dnsName, "dualstack.")
		parts := strings.Split(trimmed, ".")
		if len(parts) >= 4 {
			region := parts[2]
			elbName := parts[0]
			return fmt.Sprintf("aws:%s:%s:%s:%s", "aws", types.TypeELB, region, elbName)
		}
	}

	return ""
}
