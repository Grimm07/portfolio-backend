# Remote backend — AWS S3, one state object per environment in that env's OWN account.
#
# dev  -> bucket shadowspire-dev-state-176355979099  (account 176355979099)
# prod -> bucket shadowspire-prod-state-681053994223 (account 681053994223)
#
# Because dev and prod are separate AWS accounts, each env's portfolio-deploy role can only
# reach its own state bucket. So `bucket` and `dynamodb_table` are NOT hardcoded here — they
# are supplied per-env at init via partial backend config:
#
#   tofu init -backend-config=backend-dev.hcl     # or backend-prod.hcl
#
# State locking uses the env's shadowspire-<env>-tf-lock DynamoDB table.
terraform {
  backend "s3" {
    key     = "portfolio/terraform.tfstate"
    region  = "us-east-1"
    encrypt = true
    # bucket         = (per-env, via -backend-config)
    # dynamodb_table = (per-env, via -backend-config)
  }
}
