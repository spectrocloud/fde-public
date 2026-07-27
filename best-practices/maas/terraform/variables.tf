variable "maas_api_url" {
  description = "MAAS API URL"
  default     = "http://maas.company.com:5240/MAAS"
}

variable "maas_api_key" {
  description = "MAAS API key"
  default     = "api.spectrocloud.com"
}

variable "maas_api_ver" {
  description = "MAAS API version"
  default     = "2.0"
}

variable "maas_install" {
  description = "MAAS installation type"
  default     = "snap"
}
