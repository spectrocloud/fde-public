terraform {
  required_providers {
    maas = {
      source  = "canonical/maas"
      version = "2.7.2"
    }
  }
}

provider "maas" {
  api_version = var.maas_api_ver
  api_key     = var.maas_api_key
  api_url     = var.maas_api_url

  installation_method = var.maas_install
}
