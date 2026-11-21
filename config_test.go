package main

import (
	"testing"
)

func TestParseConfig(t *testing.T) {
	tests := []struct {
		name    string
		raw     map[string]string
		want    *Config
		wantErr bool
	}{
		{
			name: "empty config",
			raw:  nil,
			want: &Config{
				MaxRetries: 5,
			},
			wantErr: false,
		},
		{
			name: "simple string fields",
			raw: map[string]string{
				"assume_role": "arn:aws:iam::123456789012:role/MyRole",
				"profile":     "my-profile",
				"account_id":  "123456789012",
			},
			want: &Config{
				AssumeRole: "arn:aws:iam::123456789012:role/MyRole",
				Profile:    "my-profile",
				AccountID:  "123456789012",
				MaxRetries: 5,
			},
			wantErr: false,
		},
		{
			name: "max_retries as integer string",
			raw: map[string]string{
				"max_retries": "10",
			},
			want: &Config{
				MaxRetries: 10,
			},
			wantErr: false,
		},
		{
			name: "regions as comma-separated",
			raw: map[string]string{
				"regions": "us-east-1, us-west-2, eu-west-1",
			},
			want: &Config{
				Regions:    []string{"us-east-1", "us-west-2", "eu-west-1"},
				MaxRetries: 5,
			},
			wantErr: false,
		},
		{
			name: "services as comma-separated",
			raw: map[string]string{
				"services": "ec2, s3, rds",
			},
			want: &Config{
				Services:   []string{"ec2", "s3", "rds"},
				MaxRetries: 5,
			},
			wantErr: false,
		},
		{
			name: "regions as JSON array",
			raw: map[string]string{
				"regions": `["us-east-1", "us-west-2"]`,
			},
			want: &Config{
				Regions:    []string{"us-east-1", "us-west-2"},
				MaxRetries: 5,
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseConfig(tt.raw)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseConfig() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got.MaxRetries != tt.want.MaxRetries {
				t.Errorf("MaxRetries = %v, want %v", got.MaxRetries, tt.want.MaxRetries)
			}
			if got.AssumeRole != tt.want.AssumeRole {
				t.Errorf("AssumeRole = %v, want %v", got.AssumeRole, tt.want.AssumeRole)
			}
			if got.Profile != tt.want.Profile {
				t.Errorf("Profile = %v, want %v", got.Profile, tt.want.Profile)
			}
			if got.AccountID != tt.want.AccountID {
				t.Errorf("AccountID = %v, want %v", got.AccountID, tt.want.AccountID)
			}
			if len(got.Regions) != len(tt.want.Regions) {
				t.Errorf("Regions length = %v, want %v", len(got.Regions), len(tt.want.Regions))
			}
			if len(got.Services) != len(tt.want.Services) {
				t.Errorf("Services length = %v, want %v", len(got.Services), len(tt.want.Services))
			}
		})
	}
}

func TestConfig_Validate(t *testing.T) {
	tests := []struct {
		name   string
		config *Config
		want   int // number of errors expected
	}{
		{
			name: "valid config",
			config: &Config{
				Regions:    []string{"us-east-1", "us-west-2"},
				Services:   []string{"ec2", "s3"},
				MaxRetries: 5,
			},
			want: 0,
		},
		{
			name: "negative max_retries",
			config: &Config{
				MaxRetries: -1,
			},
			want: 1,
		},
		{
			name: "max_retries too high",
			config: &Config{
				MaxRetries: 25,
			},
			want: 1,
		},
		{
			name: "invalid region",
			config: &Config{
				Regions: []string{"invalid"},
			},
			want: 1,
		},
		{
			name: "invalid service",
			config: &Config{
				Services: []string{"invalid-service"},
			},
			want: 1,
		},
		{
			name: "multiple errors",
			config: &Config{
				MaxRetries: -1,
				Regions:    []string{"invalid"},
				Services:   []string{"invalid-service"},
			},
			want: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errors := tt.config.Validate()
			if len(errors) != tt.want {
				t.Errorf("Validate() returned %d errors, want %d: %v", len(errors), tt.want, errors)
			}
		})
	}
}

func TestConfig_ShouldDiscoverService(t *testing.T) {
	tests := []struct {
		name    string
		config  *Config
		service string
		want    bool
	}{
		{
			name:    "empty services discovers all",
			config:  &Config{Services: []string{}},
			service: "ec2",
			want:    true,
		},
		{
			name:    "service in list",
			config:  &Config{Services: []string{"ec2", "s3"}},
			service: "ec2",
			want:    true,
		},
		{
			name:    "service not in list",
			config:  &Config{Services: []string{"ec2", "s3"}},
			service: "rds",
			want:    false,
		},
		{
			name:    "case insensitive",
			config:  &Config{Services: []string{"EC2", "S3"}},
			service: "ec2",
			want:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.config.ShouldDiscoverService(tt.service)
			if got != tt.want {
				t.Errorf("ShouldDiscoverService() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsValidRegion(t *testing.T) {
	tests := []struct {
		name   string
		region string
		want   bool
	}{
		{"us-east-1", "us-east-1", true},
		{"us-west-2", "us-west-2", true},
		{"eu-west-1", "eu-west-1", true},
		{"ap-southeast-1", "ap-southeast-1", true},
		{"invalid", "invalid", false},
		{"too-short", "ab", false},
		{"no-dashes", "useast1", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isValidRegion(tt.region)
			if got != tt.want {
				t.Errorf("isValidRegion(%q) = %v, want %v", tt.region, got, tt.want)
			}
		})
	}
}

func TestIsValidService(t *testing.T) {
	validServices := []string{"ec2", "vpc", "s3", "rds", "iam", "elb", "route53", "lambda", "dynamodb"}
	invalidServices := []string{"sqs", "sns", "", "UNKNOWN"}

	for _, svc := range validServices {
		t.Run("valid_"+svc, func(t *testing.T) {
			if !isValidService(svc) {
				t.Errorf("isValidService(%q) = false, want true", svc)
			}
		})
	}

	for _, svc := range invalidServices {
		t.Run("invalid_"+svc, func(t *testing.T) {
			if isValidService(svc) {
				t.Errorf("isValidService(%q) = true, want false", svc)
			}
		})
	}
}
