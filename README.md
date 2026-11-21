# Zerelio AWS Plugin

Discovers AWS infrastructure resources and streams them into the Zerelio knowledge graph.

## What It Discovers

| Resource type       | Description                            |
|---------------------|----------------------------------------|
| `aws.ec2.instance`  | EC2 instances (all states)             |
| `aws.s3.bucket`     | S3 buckets                             |
| `aws.rds.instance`  | RDS database instances                 |
| `aws.lambda.function` | Lambda functions                     |
| `aws.vpc`           | Virtual Private Clouds                 |
| `aws.iam.role`      | IAM roles                              |
| `aws.iam.user`      | IAM users                              |
| `aws.elb`           | Elastic Load Balancers (classic + ALB/NLB) |
| `aws.dynamodb.table` | DynamoDB tables                       |
| `aws.route53.zone`  | Route53 hosted zones                   |

## Configuration

| Environment variable    | Required | Description                                      |
|-------------------------|----------|--------------------------------------------------|
| `AWS_ACCESS_KEY_ID`     | Yes*     | AWS access key                                   |
| `AWS_SECRET_ACCESS_KEY` | Yes*     | AWS secret key                                   |
| `AWS_REGION`            | Yes      | AWS region to discover (e.g. `us-east-1`)        |
| `AWS_PROFILE`           | No       | Named profile from `~/.aws/credentials` for assumed roles |

\* Not required when using `AWS_PROFILE` or IAM instance roles.

The plugin respects the standard AWS credential chain — environment variables, shared credentials file, instance metadata, and ECS task roles are all supported.

## Adding to a Zerelio Scope

```json
{
  "scope_name": "My AWS Production",
  "plugin_name": "aws",
  "config": {
    "region": "us-east-1",
    "account_id": "123456789012"
  },
  "schedule": "@every 1h"
}
```

## Part of Zerelio

This plugin is part of the [Zerelio](https://github.com/zerelio/knowledge) infrastructure knowledge graph platform.
