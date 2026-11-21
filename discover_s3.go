package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	plugintypes "github.com/zerelio/plugin-sdk/plugin"
	"github.com/zerelio/plugin-sdk/types"

	sdk "github.com/zerelio/plugin-sdk"
)

// DiscoverS3 discovers all S3 buckets. For each bucket, it retrieves the
// location, versioning status, encryption configuration, public access block
// settings, and bucket policy. It creates CONTAINS relationships from the
// account to each bucket.
func DiscoverS3(ctx context.Context, clients *AWSClients, cfg *Config, ch chan<- plugintypes.DiscoveryEvent) (int32, int32) {
	var resourcesFound, relationshipsFound int32

	// List all buckets using the global client.
	listResult, err := clients.S3.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		ch <- sdk.SendError("S3_ERROR", fmt.Sprintf("S3 ListBuckets failed: %v", err), true)
		return 0, 0
	}

	// Filter buckets by region if specific regions are configured.
	regionSet := make(map[string]bool, len(cfg.Regions))
	for _, r := range cfg.Regions {
		regionSet[r] = true
	}

	for _, bucket := range listResult.Buckets {
		bucketName := aws.ToString(bucket.Name)

		// Get bucket location.
		locationResp, err := clients.S3.GetBucketLocation(ctx, &s3.GetBucketLocationInput{
			Bucket: aws.String(bucketName),
		})
		bucketRegion := "us-east-1"
		if err != nil {
			ch <- sdk.SendError("S3_WARNING", fmt.Sprintf("Failed to get location for bucket %s: %v", bucketName, err), true)
		} else {
			// AWS returns an empty string for us-east-1.
			loc := string(locationResp.LocationConstraint)
			if loc == "" {
				bucketRegion = "us-east-1"
			} else {
				bucketRegion = loc
			}
		}

		// Skip if the bucket is in a region not in our configured list.
		if len(regionSet) > 0 && !regionSet[bucketRegion] {
			continue
		}

		// Create a region-specific client for the bucket if needed.
		bucketClient := clients.S3
		if bucketRegion != clients.Region && bucketRegion != "global" {
			bucketClient = s3.NewFromConfig(clients.cfg, func(o *s3.Options) {
				o.Region = bucketRegion
			})
		}

		res := sdk.NewResource("aws", types.TypeS3Bucket, bucketRegion, clients.AccountID, bucketName)
		res.Name = bucketName

		if bucket.CreationDate != nil {
			res.Properties["creation_date"] = bucket.CreationDate.Format("2006-01-02T15:04:05Z")
		}

		// Get versioning.
		discoverS3Versioning(ctx, bucketClient, bucketName, res.Properties)

		// Get encryption.
		discoverS3Encryption(ctx, bucketClient, bucketName, res.Properties)

		// Get public access block.
		discoverS3PublicAccessBlock(ctx, bucketClient, bucketName, res.Properties)

		// Get bucket policy status (public/not public).
		discoverS3PolicyStatus(ctx, bucketClient, bucketName, res.Properties)

		// Tags.
		discoverS3Tags(ctx, bucketClient, bucketName, res)

		// Raw data summary (we don't store full API responses for S3).
		rawMap := map[string]string{
			"bucket":     bucketName,
			"region":     bucketRegion,
			"versioning": res.Properties["versioning"],
			"encryption": res.Properties["encryption_type"],
			"is_public":  res.Properties["is_public"],
		}
		rawData, _ := json.Marshal(rawMap)
		res.RawData = rawData

		sdk.EmitResource(ch, res)
		resourcesFound++

		// CONTAINS: Account -> Bucket.
		accountID := fmt.Sprintf("aws:account:%s:%s", "global", clients.AccountID)
		rel := sdk.NewRelationship(accountID, res.ID, types.RelContains)
		rel.Properties["service"] = "s3"
		sdk.EmitRelationship(ch, rel)
		relationshipsFound++
	}

	return resourcesFound, relationshipsFound
}

// discoverS3Versioning retrieves the versioning status of a bucket.
func discoverS3Versioning(ctx context.Context, client *s3.Client, bucketName string, props map[string]string) {
	resp, err := client.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{
		Bucket: aws.String(bucketName),
	})
	if err != nil {
		props["versioning_error"] = truncateError(err.Error(), 200)
		return
	}
	if resp.Status == s3types.BucketVersioningStatusEnabled {
		props["versioning"] = "enabled"
	} else if resp.Status == s3types.BucketVersioningStatusSuspended {
		props["versioning"] = "suspended"
	} else {
		props["versioning"] = "disabled"
	}

	if resp.MFADelete != s3types.MFADeleteStatus("") {
		props["mfa_delete"] = string(resp.MFADelete)
	}
}

// discoverS3Encryption retrieves the server-side encryption configuration.
func discoverS3Encryption(ctx context.Context, client *s3.Client, bucketName string, props map[string]string) {
	resp, err := client.GetBucketEncryption(ctx, &s3.GetBucketEncryptionInput{
		Bucket: aws.String(bucketName),
	})
	if err != nil {
		// Encryption not configured is not an error.
		props["encryption_type"] = "none"
		return
	}

	if len(resp.ServerSideEncryptionConfiguration.Rules) > 0 {
		rule := resp.ServerSideEncryptionConfiguration.Rules[0]
		if rule.ApplyServerSideEncryptionByDefault != nil {
			props["encryption_type"] = string(rule.ApplyServerSideEncryptionByDefault.SSEAlgorithm)
			if rule.ApplyServerSideEncryptionByDefault.KMSMasterKeyID != nil {
				props["kms_key_id"] = *rule.ApplyServerSideEncryptionByDefault.KMSMasterKeyID
			}
		}
		if rule.BucketKeyEnabled != nil {
			props["bucket_key_enabled"] = fmt.Sprintf("%t", *rule.BucketKeyEnabled)
		}
	}
}

// discoverS3PublicAccessBlock retrieves the public access block configuration.
func discoverS3PublicAccessBlock(ctx context.Context, client *s3.Client, bucketName string, props map[string]string) {
	resp, err := client.GetPublicAccessBlock(ctx, &s3.GetPublicAccessBlockInput{
		Bucket: aws.String(bucketName),
	})
	if err != nil {
		// No public access block configuration is common.
		props["public_access_block"] = "not_configured"
		return
	}

	config := resp.PublicAccessBlockConfiguration
	if config != nil {
		allTrue := aws.ToBool(config.BlockPublicAcls) && aws.ToBool(config.BlockPublicPolicy) &&
			aws.ToBool(config.IgnorePublicAcls) && aws.ToBool(config.RestrictPublicBuckets)
		if allTrue {
			props["public_access_block"] = "all_blocked"
		} else {
			props["public_access_block"] = "partially_configured"
			props["block_public_acls"] = fmt.Sprintf("%t", aws.ToBool(config.BlockPublicAcls))
			props["block_public_policy"] = fmt.Sprintf("%t", aws.ToBool(config.BlockPublicPolicy))
			props["ignore_public_acls"] = fmt.Sprintf("%t", aws.ToBool(config.IgnorePublicAcls))
			props["restrict_public_buckets"] = fmt.Sprintf("%t", aws.ToBool(config.RestrictPublicBuckets))
		}
	}
}

// discoverS3PolicyStatus checks if a bucket has a public policy.
func discoverS3PolicyStatus(ctx context.Context, client *s3.Client, bucketName string, props map[string]string) {
	resp, err := client.GetBucketPolicyStatus(ctx, &s3.GetBucketPolicyStatusInput{
		Bucket: aws.String(bucketName),
	})
	if err != nil {
		props["is_public"] = "unknown"
		return
	}

	if resp.PolicyStatus != nil {
		props["is_public"] = fmt.Sprintf("%t", *resp.PolicyStatus.IsPublic)
	}
}

// discoverS3Tags retrieves the tags for a bucket.
func discoverS3Tags(ctx context.Context, client *s3.Client, bucketName string, res *types.Resource) {
	resp, err := client.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{
		Bucket: aws.String(bucketName),
	})
	if err != nil {
		// No tags is not an error.
		return
	}

	if len(resp.TagSet) > 0 {
		tagMap := make(map[string]string, len(resp.TagSet))
		for _, t := range resp.TagSet {
			if t.Key != nil && t.Value != nil {
				tagMap[*t.Key] = *t.Value
			}
		}
		res.Tags = sdk.TagsToStrings(tagMap)
	}
}

// truncateError truncates an error message to maxLen characters.
func truncateError(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
