package main

import (
	"context"
	"fmt"
	"sync/atomic"

	plugintypes "github.com/zerelio/plugin-sdk/plugin"

	sdk "github.com/zerelio/plugin-sdk"
)

const (
	pluginName    = "aws"
	pluginVersion = "0.1.0"
)

// AWSPlugin implements the DiscoveryPlugin interface for AWS.
// It orchestrates discovery across all configured AWS services (EC2, VPC, S3,
// RDS, IAM, ELB, Route53, Lambda, DynamoDB) and streams discovered resources
// and relationships back to the orchestrator.
type AWSPlugin struct {
	sdk.BasePlugin
}

// Info returns metadata about the AWS discovery plugin.
func (p *AWSPlugin) Info(ctx context.Context) (*plugintypes.PluginInfo, error) {
	return &plugintypes.PluginInfo{
		Name:        pluginName,
		Version:     pluginVersion,
		Description: "Discovers AWS infrastructure resources including EC2, VPC, S3, RDS, IAM, ELB, Route53, Lambda, and DynamoDB",
		Capabilities: []string{
			"compute",
			"networking",
			"storage",
			"databases",
			"security",
			"dns",
			"load_balancing",
			"serverless",
			"nosql",
		},
		ConfigSchema: `{
  "type": "object",
  "properties": {
    "regions": {
      "type": "array",
      "items": {"type": "string"},
      "description": "AWS regions to discover. Empty means all enabled regions."
    },
    "assume_role": {
      "type": "string",
      "description": "IAM role ARN to assume for discovery."
    },
    "profile": {
      "type": "string",
      "description": "AWS CLI named profile to use."
    },
    "max_retries": {
      "type": "integer",
      "default": 5,
      "description": "Maximum number of retry attempts for AWS API calls."
    },
    "services": {
      "type": "array",
      "items": {"type": "string", "enum": ["ec2","vpc","s3","rds","iam","elb","route53","lambda","dynamodb"]},
      "description": "AWS services to discover. Empty means all services."
    },
    "account_id": {
      "type": "string",
      "description": "AWS account ID. Auto-detected if not specified."
    }
  }
}`,
		CredentialTypes: []string{
			"aws_access_key",
			"aws_iam_role",
			"aws_instance_profile",
			"aws_assume_role",
		},
	}, nil
}

// Discover performs a full AWS infrastructure discovery across all configured
// regions and services. It streams resources, relationships, progress updates,
// and errors back to the orchestrator through the returned channel.
func (p *AWSPlugin) Discover(ctx context.Context, req *plugintypes.DiscoverRequest) (<-chan plugintypes.DiscoveryEvent, error) {
	ch := make(chan plugintypes.DiscoveryEvent, 256)

	go func() {
		defer close(ch)

		// Parse configuration.
		cfg, err := ParseConfig(req.Config)
		if err != nil {
			ch <- sdk.SendError("CONFIG_ERROR", fmt.Sprintf("failed to parse config: %v", err), false)
			return
		}

		// Build global clients (IAM, Route53) and resolve account ID.
		builder := NewClientBuilder(cfg, &p.BasePlugin)
		globalClients, err := builder.BuildGlobalClients(ctx)
		if err != nil {
			ch <- sdk.SendError("AUTH_ERROR", fmt.Sprintf("failed to create AWS clients: %v", err), false)
			return
		}

		p.LogInfo("starting AWS discovery",
			"account_id", globalClients.AccountID,
			"configured_regions", fmt.Sprintf("%v", cfg.Regions),
			"services", fmt.Sprintf("%v", cfg.Services),
		)

		// Get list of regions to discover.
		regions, err := builder.GetEnabledRegions(ctx, globalClients)
		if err != nil {
			ch <- sdk.SendError("REGION_ERROR", fmt.Sprintf("failed to get regions: %v", err), true)
			return
		}

		// Track progress.
		var resourcesFound, relationshipsFound int32
		_ = len(regions) * countEnabledServices(cfg) // totalSteps for progress tracking

		// Send initial progress.
		ch <- sdk.SendProgress("Starting AWS discovery", 0, 0, 0)

		// --- Discover global services ---

		// IAM (global service, discover once).
		if cfg.ShouldDiscoverService("iam") {
			p.LogInfo("discovering IAM resources")
			ch <- sdk.SendProgress("Discovering IAM resources", 0,
				atomic.LoadInt32(&resourcesFound), atomic.LoadInt32(&relationshipsFound))

			rCount, relCount := DiscoverIAM(ctx, globalClients, ch)
			atomic.AddInt32(&resourcesFound, rCount)
			atomic.AddInt32(&relationshipsFound, relCount)
		}

		// S3 buckets (need to discover per region for location, but list is global).
		if cfg.ShouldDiscoverService("s3") {
			p.LogInfo("discovering S3 buckets")
			ch <- sdk.SendProgress("Discovering S3 buckets", 10,
				atomic.LoadInt32(&resourcesFound), atomic.LoadInt32(&relationshipsFound))

			rCount, relCount := DiscoverS3(ctx, globalClients, cfg, ch)
			atomic.AddInt32(&resourcesFound, rCount)
			atomic.AddInt32(&relationshipsFound, relCount)
		}

		// Route53 (global service).
		if cfg.ShouldDiscoverService("route53") {
			p.LogInfo("discovering Route53 resources")
			ch <- sdk.SendProgress("Discovering Route53 resources", 15,
				atomic.LoadInt32(&resourcesFound), atomic.LoadInt32(&relationshipsFound))

			rCount, relCount := DiscoverRoute53(ctx, globalClients, ch)
			atomic.AddInt32(&resourcesFound, rCount)
			atomic.AddInt32(&relationshipsFound, relCount)
		}

		// --- Discover regional services ---

		for i, region := range regions {
			select {
			case <-ctx.Done():
				ch <- sdk.SendError("CANCELLED", "discovery cancelled by context", false)
				return
			default:
			}

			p.LogInfo("discovering region", "region", region)
			pct := int32(20 + (80 * i / len(regions)))
			ch <- sdk.SendProgress(
				fmt.Sprintf("Discovering region %s (%d/%d)", region, i+1, len(regions)),
				pct,
				atomic.LoadInt32(&resourcesFound),
				atomic.LoadInt32(&relationshipsFound),
			)

			regionalClients, err := builder.BuildRegionalClients(ctx, region, globalClients)
			if err != nil {
				ch <- sdk.SendError("CLIENT_ERROR",
					fmt.Sprintf("failed to create clients for region %s: %v", region, err), true)
				continue
			}

			// EC2 instances.
			if cfg.ShouldDiscoverService("ec2") {
				rCount, relCount := DiscoverEC2(ctx, regionalClients, ch)
				atomic.AddInt32(&resourcesFound, rCount)
				atomic.AddInt32(&relationshipsFound, relCount)
			}

			// VPC networking.
			if cfg.ShouldDiscoverService("vpc") {
				rCount, relCount := DiscoverVPC(ctx, regionalClients, ch)
				atomic.AddInt32(&resourcesFound, rCount)
				atomic.AddInt32(&relationshipsFound, relCount)
			}

			// RDS databases.
			if cfg.ShouldDiscoverService("rds") {
				rCount, relCount := DiscoverRDS(ctx, regionalClients, ch)
				atomic.AddInt32(&resourcesFound, rCount)
				atomic.AddInt32(&relationshipsFound, relCount)
			}

			// ELB/ALB/NLB.
			if cfg.ShouldDiscoverService("elb") {
				rCount, relCount := DiscoverELB(ctx, regionalClients, ch)
				atomic.AddInt32(&resourcesFound, rCount)
				atomic.AddInt32(&relationshipsFound, relCount)
			}

			// Lambda functions.
			if cfg.ShouldDiscoverService("lambda") {
				rCount, relCount := DiscoverLambda(ctx, regionalClients, ch)
				atomic.AddInt32(&resourcesFound, rCount)
				atomic.AddInt32(&relationshipsFound, relCount)
			}

			// DynamoDB tables.
			if cfg.ShouldDiscoverService("dynamodb") {
				rCount, relCount := DiscoverDynamoDB(ctx, regionalClients, ch)
				atomic.AddInt32(&resourcesFound, rCount)
				atomic.AddInt32(&relationshipsFound, relCount)
			}
		}

		// Final progress.
		ch <- sdk.SendProgress(
			fmt.Sprintf("AWS discovery complete: %d resources, %d relationships",
				atomic.LoadInt32(&resourcesFound), atomic.LoadInt32(&relationshipsFound)),
			100,
			atomic.LoadInt32(&resourcesFound),
			atomic.LoadInt32(&relationshipsFound),
		)

		p.LogInfo("AWS discovery complete",
			"resources_found", atomic.LoadInt32(&resourcesFound),
			"relationships_found", atomic.LoadInt32(&relationshipsFound),
		)
	}()

	return ch, nil
}

// ValidateConfig checks whether the provided configuration is valid for the
// AWS plugin.
func (p *AWSPlugin) ValidateConfig(ctx context.Context, config map[string]string) (*plugintypes.ValidationResult, error) {
	cfg, err := ParseConfig(config)
	if err != nil {
		return &plugintypes.ValidationResult{
			Valid:  false,
			Errors: []string{fmt.Sprintf("failed to parse config: %v", err)},
		}, nil
	}

	errors := cfg.Validate()

	// Warnings.
	var warnings []string
	if len(cfg.Regions) == 0 {
		warnings = append(warnings, "no regions specified; will discover all enabled regions (may be slow)")
	}
	if cfg.AssumeRole == "" && cfg.Profile == "" {
		warnings = append(warnings, "no assume_role or profile specified; will use default credential chain")
	}

	return &plugintypes.ValidationResult{
		Valid:    len(errors) == 0,
		Errors:   errors,
		Warnings: warnings,
	}, nil
}

// countEnabledServices returns the number of services that will be discovered.
func countEnabledServices(cfg *Config) int {
	if len(cfg.Services) == 0 {
		return 9 // All services.
	}
	return len(cfg.Services)
}
