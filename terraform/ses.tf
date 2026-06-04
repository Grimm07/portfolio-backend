# Verify the sending domain and publish DKIM so contact emails deliver. DNS now lives in
# Amazon Route 53 (migrated off Cloudflare), so the DKIM CNAMEs are created there.
resource "aws_ses_domain_identity" "main" {
  domain = var.domain_name
}

resource "aws_ses_domain_dkim" "main" {
  domain = aws_ses_domain_identity.main.domain
}

# The public hosted zone for the domain, shared across environments (dev + prod each publish
# their own DKIM tokens into it).
#
# NOTE (cross-account): this looks the zone up in whichever account the apply runs in. The
# `portfolio-deploy` OIDC role for each environment must therefore be able to read AND write this
# zone — i.e. the zone lives in that account, or the role is granted cross-account Route 53 access.
# Dev (176355979099) and prod (681053994223) are separate accounts; if the zone lives in only one
# of them, the other env needs a delegated/cross-account path to write its records.
data "aws_route53_zone" "main" {
  name         = var.domain_name
  private_zone = false
}

# Three DKIM CNAME records in the Route 53 hosted zone. Each env's tokens are unique (they come
# from that env's own SES domain identity), so dev and prod never collide on a record name.
resource "aws_route53_record" "ses_dkim" {
  count   = 3
  zone_id = data.aws_route53_zone.main.zone_id
  name    = "${aws_ses_domain_dkim.main.dkim_tokens[count.index]}._domainkey.${var.domain_name}"
  type    = "CNAME"
  ttl     = 300
  records = ["${aws_ses_domain_dkim.main.dkim_tokens[count.index]}.dkim.amazonses.com"]
}

# Recipient identity. SES stays in sandbox; both sender (domain) and recipient (your
# inbox) are verified, so no production-access request is needed. Verifying the email
# identity triggers a one-time confirmation email to CONTACT_EMAIL (manual click).
resource "aws_ses_email_identity" "recipient" {
  email = var.contact_email
}
