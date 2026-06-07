terraform {
  required_version = ">= 1.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.70"
    }
  }
}

# DNS is on Amazon Route 53 (migrated off Cloudflare). The SES DKIM CNAMEs are managed via the
# AWS provider in ses.tf (data.aws_route53_zone + aws_route53_record); there is no longer a
# Cloudflare provider or token.
