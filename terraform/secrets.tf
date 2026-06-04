# CONTACT_EMAIL stored in Secrets Manager (not a plaintext Lambda env var), per the
# project's privacy-first posture. The ingest Lambda fetches it at runtime (cached per container).
resource "aws_secretsmanager_secret" "contact_email" {
  name        = "${local.name_prefix}-contact-email"
  description = "Recipient address for contact form emails"
}

resource "aws_secretsmanager_secret_version" "contact_email" {
  secret_id     = aws_secretsmanager_secret.contact_email.id
  secret_string = var.contact_email
}
