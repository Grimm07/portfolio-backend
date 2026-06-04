terraform {
  required_version = ">= 1.0"

  required_providers {
    cloudflare = {
      source  = "cloudflare/cloudflare"
      version = "~> 5.16"
    }
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.70"
    }
    archive = {
      source  = "hashicorp/archive"
      version = "~> 2.6"
    }
  }
}

# Cloudflare is retained ONLY to manage the SES DKIM CNAME records in the zone (see ses.tf).
# The apex/www/dev hostnames now point at CloudFront and are managed by the infra repo.
provider "cloudflare" {
  api_token = var.cloudflare_api_token
}
