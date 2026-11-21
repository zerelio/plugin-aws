package main

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	sdk "github.com/zerelio/plugin-sdk"
)

// AWSClients holds configured AWS SDK clients for all services used by the
// discovery plugin. Clients are region-specific; for global services (IAM,
// Route53, S3) the us-east-1 client is used as the canonical client.
type AWSClients struct {
	// AccountID is the resolved AWS account ID.
	AccountID string

	// Region is the region these clients are configured for.
	Region string

	// EC2 is the Amazon EC2 client.
	EC2 *ec2.Client

	// S3 is the Amazon S3 client.
	S3 *s3.Client

	// RDS is the Amazon RDS client.
	RDS *rds.Client

	// IAM is the Amazon IAM client.
	IAM *iam.Client

	// ELBv2 is the Amazon ELBv2 (ALB/NLB) client.
	ELBv2 *elasticloadbalancingv2.Client

	// Route53 is the Amazon Route53 client.
	Route53 *route53.Client

	// Lambda is the AWS Lambda client.
	Lambda *lambda.Client

	// DynamoDB is the Amazon DynamoDB client.
	DynamoDB *dynamodb.Client

	// cfg is the underlying AWS config (for creating per-region clients).
	cfg aws.Config
}

// ClientBuilder creates AWS SDK clients from the plugin configuration.
// It handles credential resolution (access keys, assumed roles, instance
// profiles, environment variables) and rate limiting configuration.
type ClientBuilder struct {
	config *Config
	logger *sdk.BasePlugin
}

// NewClientBuilder creates a new ClientBuilder.
func NewClientBuilder(cfg *Config, base *sdk.BasePlugin) *ClientBuilder {
	return &ClientBuilder{
		config: cfg,
		logger: base,
	}
}

// BuildGlobalClients creates AWS clients for global services (IAM, Route53)
// and resolves the account ID. These clients use us-east-1 as their region.
func (cb *ClientBuilder) BuildGlobalClients(ctx context.Context) (*AWSClients, error) {
	opts := []func(*config.LoadOptions) error{
		config.WithRetryMaxAttempts(cb.config.MaxRetries),
	}

	// Use named profile if specified.
	if cb.config.Profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(cb.config.Profile))
	}

	// Load the default config.
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Assume role if specified.
	if cb.config.AssumeRole != "" {
		stsClient := sts.NewFromConfig(cfg)
		creds := stscreds.NewAssumeRoleProvider(stsClient, cb.config.AssumeRole)
		cfg.Credentials = aws.NewCredentialsCache(creds)
	}

	// Resolve account ID.
	accountID := cb.config.AccountID
	if accountID == "" {
		accountID, err = cb.resolveAccountID(ctx, cfg)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve AWS account ID: %w", err)
		}
	}

	return &AWSClients{
		AccountID: accountID,
		Region:    "global",
		cfg:       cfg,
		IAM:       iam.NewFromConfig(cfg),
		Route53:   route53.NewFromConfig(cfg),
	}, nil
}

// BuildRegionalClients creates AWS clients scoped to a specific region.
// The returned AWSClients has all regional service clients configured.
func (cb *ClientBuilder) BuildRegionalClients(ctx context.Context, region string, globalClients *AWSClients) (*AWSClients, error) {
	opts := []func(*config.LoadOptions) error{
		config.WithRegion(region),
		config.WithRetryMaxAttempts(cb.config.MaxRetries),
	}

	if cb.config.Profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(cb.config.Profile))
	}

	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config for region %s: %w", region, err)
	}

	if cb.config.AssumeRole != "" {
		stsClient := sts.NewFromConfig(cfg)
		creds := stscreds.NewAssumeRoleProvider(stsClient, cb.config.AssumeRole)
		cfg.Credentials = aws.NewCredentialsCache(creds)
	}

	return &AWSClients{
		AccountID: globalClients.AccountID,
		Region:    region,
		cfg:       cfg,
		EC2:       ec2.NewFromConfig(cfg),
		S3:        s3.NewFromConfig(cfg),
		RDS:       rds.NewFromConfig(cfg),
		IAM:       globalClients.IAM,
		ELBv2:     elasticloadbalancingv2.NewFromConfig(cfg),
		Lambda:    lambda.NewFromConfig(cfg),
		DynamoDB:  dynamodb.NewFromConfig(cfg),
		Route53:   globalClients.Route53,
	}, nil
}

// resolveAccountID calls sts:GetCallerIdentity to determine the current
// account ID.
func (cb *ClientBuilder) resolveAccountID(ctx context.Context, cfg aws.Config) (string, error) {
	stsClient := sts.NewFromConfig(cfg)

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	resp, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", fmt.Errorf("sts:GetCallerIdentity failed: %w", err)
	}
	if resp.Account == nil {
		return "", fmt.Errorf("sts:GetCallerIdentity returned nil account")
	}
	return *resp.Account, nil
}

// GetEnabledRegions returns the list of regions to discover. If the config
// specifies regions, those are used. Otherwise, it queries AWS for all
// opt-in-status=opted-in regions.
func (cb *ClientBuilder) GetEnabledRegions(ctx context.Context, clients *AWSClients) ([]string, error) {
	if len(cb.config.Regions) > 0 {
		return cb.config.Regions, nil
	}

	// Use the EC2 client from globalClients or build a us-east-1 client.
	ec2Client := ec2.NewFromConfig(clients.cfg)

	resp, err := ec2Client.DescribeRegions(ctx, &ec2.DescribeRegionsInput{
		Filters: []ec2types.Filter{
			{
				Name:   aws.String("opt-in-status"),
				Values: []string{"opt-in-not-required", "opted-in"},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to describe AWS regions: %w", err)
	}

	regions := make([]string, 0, len(resp.Regions))
	for _, r := range resp.Regions {
		if r.RegionName != nil {
			regions = append(regions, *r.RegionName)
		}
	}

	return regions, nil
}

// isThrottlingError checks if an error is an AWS API throttling error.
func isThrottlingError(err error) bool {
	if err == nil {
		return false
	}
	// AWS SDK v2 throttling errors typically contain these codes.
	errMsg := err.Error()
	return contains(errMsg, "Throttling") ||
		contains(errMsg, "RequestLimitExceeded") ||
		contains(errMsg, "Rate exceeded") ||
		contains(errMsg, "SlowDown")
}

// contains checks if a string contains a substring (case-sensitive).
func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// Paginate is a generic helper that handles AWS API pagination with rate-limit
// aware backoff. It calls the provided function with each page of results,
// automatically retrying on throttling errors with exponential backoff.
func Paginate(ctx context.Context, fn func() (bool, error), maxRetries int) error {
	backoff := time.Second
	for retries := 0; retries < maxRetries; {
		hasMore, err := fn()
		if err != nil {
			if isThrottlingError(err) && retries < maxRetries {
				retries++
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(backoff):
					backoff = backoff * 2
					if backoff > 30*time.Second {
						backoff = 30 * time.Second
					}
				}
				continue
			}
			return err
		}
		if !hasMore {
			return nil
		}
		retries = 0 // Reset retry counter on success.
		backoff = time.Second
	}
	return fmt.Errorf("max retries (%d) exceeded", maxRetries)
}
