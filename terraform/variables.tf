variable "cloudflare_api_token" {
  description = "Cloudflare API token with appropriate permissions"
  type        = string
  sensitive   = true
}

variable "cloudflare_zone_id" {
  description = "Cloudflare zone ID for the domain (trystan-tbm.dev)"
  type        = string
}

variable "contact_email" {
  description = "Email address to receive contact form submissions"
  type        = string
  sensitive   = true
}

variable "domain_name" {
  description = "Domain name for the portfolio (default: trystan-tbm.dev)"
  type        = string
  default     = "trystan-tbm.dev"
}

variable "aws_region" {
  description = "AWS region for all contact backend resources"
  type        = string
  default     = "us-east-1"
}

variable "environment" {
  description = "Deployment environment; drives SSM parameter paths (/portfolio/<env>/*)"
  type        = string

  validation {
    condition     = contains(["dev", "prod"], var.environment)
    error_message = "environment must be \"dev\" or \"prod\"."
  }
}
