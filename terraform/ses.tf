# Verify the sending domain and publish DKIM so digest emails deliver. DNS lives in
# Cloudflare, so the DKIM CNAMEs are created there.
resource "aws_ses_domain_identity" "main" {
  domain = var.domain_name
}

resource "aws_ses_domain_dkim" "main" {
  domain = aws_ses_domain_identity.main.domain
}

# Three DKIM CNAME records in the Cloudflare zone (DNS-only; not proxied).
resource "cloudflare_dns_record" "ses_dkim" {
  count   = 3
  zone_id = var.cloudflare_zone_id
  name    = "${aws_ses_domain_dkim.main.dkim_tokens[count.index]}._domainkey"
  type    = "CNAME"
  content = "${aws_ses_domain_dkim.main.dkim_tokens[count.index]}.dkim.amazonses.com"
  proxied = false
  ttl     = 300
}

# Recipient identity. SES stays in sandbox; both sender (domain) and recipient (your
# inbox) are verified, so no production-access request is needed. Verifying the email
# identity triggers a one-time confirmation email to CONTACT_EMAIL (manual click).
resource "aws_ses_email_identity" "recipient" {
  email = var.contact_email
}
