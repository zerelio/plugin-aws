package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Config holds AWS-specific plugin configuration. It is parsed from the
// key-value config map passed in the DiscoverRequest.
type Config struct {
	// Regions specifies which AWS regions to discover resources in.
	// If empty, the plugin discovers resources in all enabled regions.
	Regions []string `json:"regions" yaml:"regions"`

	// AssumeRole is an IAM role ARN to assume before making API calls.
	// The plugin will call sts:AssumeRole using the base credentials and
	// then use the assumed role credentials for all subsequent calls.
	AssumeRole string `json:"assume_role" yaml:"assume_role"`

	// Profile is the AWS CLI profile name to use for credentials.
	// This is useful for local development with named profiles.
	Profile string `json:"profile" yaml:"profile"`

	// MaxRetries is the maximum number of retry attempts for AWS API calls.
	// Defaults to 5 if not specified.
	MaxRetries int `json:"max_retries" yaml:"max_retries"`

	// Services specifies which AWS services to discover. If empty, all
	// supported services are discovered. Valid values: "ec2", "vpc", "s3",
	// "rds", "iam", "elb", "route53", "lambda", "dynamodb".
	Services []string `json:"services" yaml:"services"`

	// AccountID is the AWS account ID. If not provided, the plugin will
	// determine it automatically via sts:GetCallerIdentity.
	AccountID string `json:"account_id" yaml:"account_id"`
}

// ParseConfig parses a map[string]string into a Config struct. The map values
// may contain JSON-encoded strings for complex types (slices, nested objects).
func ParseConfig(raw map[string]string) (*Config, error) {
	cfg := &Config{
		MaxRetries: 5,
	}

	if raw == nil {
		return cfg, nil
	}

	// Handle simple string fields.
	if v, ok := raw["assume_role"]; ok {
		cfg.AssumeRole = v
	}
	if v, ok := raw["profile"]; ok {
		cfg.Profile = v
	}
	if v, ok := raw["account_id"]; ok {
		cfg.AccountID = v
	}
	if v, ok := raw["max_retries"]; ok {
		var n int
		if err := json.Unmarshal([]byte(v), &n); err != nil {
			// Try parsing as plain integer string.
			_, err2 := fmt.Sscanf(v, "%d", &n)
			if err2 != nil {
				return nil, fmt.Errorf("invalid max_retries value %q: %w", v, err)
			}
		}
		cfg.MaxRetries = n
	}

	// Handle slice fields: try JSON array first, fall back to comma-separated.
	cfg.Regions = parseStringSlice(raw, "regions")
	cfg.Services = parseStringSlice(raw, "services")

	return cfg, nil
}

// parseStringSlice extracts a string slice from a config map. It first attempts
// to parse the value as a JSON array. If that fails, it falls back to a
// comma-separated string.
func parseStringSlice(raw map[string]string, key string) []string {
	v, ok := raw[key]
	if !ok || v == "" {
		return nil
	}

	// Try JSON array.
	var result []string
	if err := json.Unmarshal([]byte(v), &result); err == nil {
		return result
	}

	// Fall back to comma-separated.
	parts := strings.Split(v, ",")
	for i, p := range parts {
		parts[i] = strings.TrimSpace(p)
	}
	return parts
}

// Validate checks the configuration for errors and returns any issues found.
func (c *Config) Validate() []string {
	var errors []string

	if c.MaxRetries < 0 {
		errors = append(errors, "max_retries must be non-negative")
	}
	if c.MaxRetries > 20 {
		errors = append(errors, "max_retries should not exceed 20")
	}

	for _, region := range c.Regions {
		if !isValidRegion(region) {
			errors = append(errors, fmt.Sprintf("invalid AWS region: %s", region))
		}
	}

	for _, svc := range c.Services {
		if !isValidService(svc) {
			errors = append(errors, fmt.Sprintf("unknown AWS service: %s (valid: ec2, vpc, s3, rds, iam, elb, route53, lambda, dynamodb)", svc))
		}
	}

	return errors
}

// ShouldDiscoverService returns true if the given service should be discovered
// based on the configuration. If no services are specified, all are discovered.
func (c *Config) ShouldDiscoverService(service string) bool {
	if len(c.Services) == 0 {
		return true
	}
	for _, s := range c.Services {
		if strings.EqualFold(s, service) {
			return true
		}
	}
	return false
}

// isValidRegion performs a basic check on an AWS region name.
func isValidRegion(region string) bool {
	if len(region) < 4 {
		return false
	}
	// AWS regions follow the pattern: xx-xxxxx-n (e.g., us-east-1, eu-west-2, ap-southeast-1)
	parts := strings.Split(region, "-")
	if len(parts) < 3 {
		return false
	}
	return true
}

// isValidService checks if the service name is recognized.
func isValidService(service string) bool {
	valid := map[string]bool{
		"ec2":     true,
		"vpc":     true,
		"s3":      true,
		"rds":     true,
		"iam":     true,
		"elb":     true,
		"route53": true,
		"lambda":  true,
		"dynamodb": true,
	}
	return valid[strings.ToLower(service)]
}
