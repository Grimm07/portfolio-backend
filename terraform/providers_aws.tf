# AWS provider + supporting providers for the contact backend (Plan 2a).
# ACM/CloudFront/WAF in Plan 2b also require us-east-1, so a single region keeps things simple.
#
# NOTE: provider version pins for aws/archive live in main.tf's single `required_providers`
# block. OpenTofu permits only ONE required_providers per module (the plan's original note
# that they merge across files was incorrect), so they are consolidated there.

provider "aws" {
  region = var.aws_region
  default_tags {
    tags = {
      Project   = "portfolio-contact"
      ManagedBy = "opentofu"
    }
  }
}

locals {
  name_prefix = "portfolio-contact"
  # Derive the From address from the domain — no email literal hardcoded.
  from_email = "noreply@${var.domain_name}"
}
