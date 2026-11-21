terraform {
  required_version = ">= 1.5.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
    local = {
      source  = "hashicorp/local"
      version = "~> 2.5"
    }
  }

  # Backend stays local on purpose; point it at a remote backend (S3...)
  # before any real deployment.
}

provider "aws" {
  region = var.resource_plan.region

  # Offline planning: let `terraform plan` run without real credentials or
  # API round-trips (this template is a simulation, not a live deployment).
  # Safe to remove for real use.
  skip_credentials_validation = true
  skip_requesting_account_id  = true
  skip_metadata_api_check     = true
}

# Second-city provider for the DR read replicas (modules/dr-replica).
provider "aws" {
  alias  = "dr"
  region = var.resource_plan.dr.region

  skip_credentials_validation = true
  skip_requesting_account_id  = true
  skip_metadata_api_check     = true
}

# Remote state: local by default for the simulation. For production use an
# S3 backend with locking (create the bucket and DynamoDB table first):
#
# backend "s3" {
#   bucket       = "bank-dcn-tfstate"
#   key          = "dcn/terraform.tfstate"
#   region       = "ap-southeast-2"
#   dynamodb_table = "bank-dcn-tflock"
#   encrypt      = true
# }
