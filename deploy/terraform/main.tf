############################################
# Terraform & Provider configuration
############################################
terraform {
  required_version = ">= 1.5.0"

  required_providers {
    google      = {
      source  = "hashicorp/google"
      version = "~> 5.30"
    }
    google-beta = {
      source  = "hashicorp/google-beta"
      version = "~> 5.30"
    }
  }

  backend "gcs" {
    bucket = "REPLACE_WITH_TF_STATE_BUCKET"
    prefix = "simple-http-server/state"
  }
}

provider "google" {
  project = var.project_id
  region  = var.region
}

provider "google-beta" {
  project = var.project_id
  region  = var.region
}

############################################
# Variables
############################################
variable "project_id" {
  description = "GCP project ID"
  type        = string
}

variable "region" {
  description = "GCP region"
  type        = string
  default     = "us-central1"
}

variable "zone" {
  description = "GCP zone for zonal resources"
  type        = string
  default     = "us-central1-a"
}

variable "cluster_name" {
  description = "GKE cluster name"
  type        = string
  default     = "simple-http-server-gke"
}

variable "node_machine_type" {
  description = "Machine type for GKE nodes"
  type        = string
  default     = "e2-standard-2"
}

variable "min_node_count" {
  type    = number
  default = 1
}

variable "max_node_count" {
  type    = number
  default = 5
}

variable "environment" {
  type    = string
  default = "production"
}

############################################
# Networking (VPC + Subnet)
############################################
resource "google_compute_network" "vpc" {
  name                    = "simple-http-server-vpc"
  auto_create_subnetworks = false
  routing_mode            = "REGIONAL"
}

resource "google_compute_subnetwork" "subnet" {
  name                     = "simple-http-server-subnet"
  ip_cidr_range            = "10.10.0.0/20"
  region                   = var.region
  network                  = google_compute_network.vpc.id
  private_ip_google_access = true

  secondary_ip_range {
    range_name    = "pods"
    ip_cidr_range = "10.20.0.0/16"
  }

  secondary_ip_range {
    range_name    = "services"
    ip_cidr_range = "10.30.0.0/20"
  }
}

resource "google_compute_router" "router" {
  name    = "simple-http-server-router"
  region  = var.region
  network = google_compute_network.vpc.id
}

resource "google_compute_router_nat" "nat" {
  name                               = "simple-http-server-nat"
  router                             = google_compute_router.router.name
  region                             = var.region
  nat_ip_allocate_option             = "AUTO_ONLY"
  source_subnetwork_ip_ranges_to_nat = "ALL_SUBNETWORKS_ALL_IP_RANGES"
}

resource "google_compute_firewall" "allow_lb_healthcheck" {
  name    = "allow-gclb-healthcheck"
  network = google_compute_network.vpc.id

  allow {
    protocol = "tcp"
    ports    = ["8080", "8443"]
  }

  source_ranges = ["130.211.0.0/22", "35.191.0.0/16"]
  target_tags   = ["gke-node"]
}

############################################
# Artifact Registry (container images)
############################################
resource "google_artifact_registry_repository" "repo" {
  location      = var.region
  repository_id = "simple-http-server-repo"
  description   = "Container images for simple-http-server"
  format        = "DOCKER"

  cleanup_policies {
    id     = "keep-last-10"
    action = "KEEP"
    most_recent_versions {
      keep_count = 10
    }
  }
}

############################################
# GKE Cluster (private nodes, autoscaling)
############################################
resource "google_service_account" "gke_nodes" {
  account_id   = "simple-http-server-gke-sa"
  display_name = "GKE Node Service Account - simple-http-server"
}

resource "google_project_iam_member" "gke_sa_roles" {
  for_each = toset([
    "roles/logging.logWriter",
    "roles/monitoring.metricWriter",
    "roles/monitoring.viewer",
    "roles/artifactregistry.reader",
  ])
  project = var.project_id
  role    = each.value
  member  = "serviceAccount:${google_service_account.gke_nodes.email}"
}

resource "google_container_cluster" "primary" {
  name     = var.cluster_name
  location = var.zone

  # Use a separately managed node pool
  remove_default_node_pool = true
  initial_node_count       = 1

  network    = google_compute_network.vpc.id
  subnetwork = google_compute_subnetwork.subnet.id

  networking_mode = "VPC_NATIVE"
  ip_allocation_policy {
    cluster_secondary_range_name  = "pods"
    services_secondary_range_name = "services"
  }

  private_cluster_config {
    enable_private_nodes    = true
    enable_private_endpoint = false
    master_ipv4_cidr_block  = "172.16.0.0/28"
  }

  release_channel {
    channel = "REGULAR"
  }

  workload_identity_config {
    workload_pool = "${var.project_id}.svc.id.goog"
  }

  addons_config {
    http_load_balancing {
      disabled = false
    }
    horizontal_pod_autoscaling {
      disabled = false
    }
  }

  master_authorized_networks_config {
    cidr_blocks {
      cidr_block   = "0.0.0.0/0"
      display_name = "allow-all-temporary"
    }
  }

  deletion_protection = false

  resource_labels = {
    environment = var.environment
    app         = "simple-http-server"
  }
}

resource "google_container_node_pool" "primary_nodes" {
  name     = "${var.cluster_name}-node-pool"
  location = var.zone
  cluster  = google_container_cluster.primary.name

  autoscaling {
    min_node_count = var.min_node_count
    max_node_count = var.max_node_count
  }

  management {
    auto_repair  = true
    auto_upgrade = true
  }

  node_config {
    machine_type    = var.node_machine_type
    tags            = ["gke-node", "simple-http-server"]
    service_account = google_service_account.gke_nodes.email

    oauth_scopes = [
      "https://www.googleapis.com/auth/cloud-platform",
    ]

    workload_metadata_config {
      mode = "GKE_METADATA"
    }

    labels = {
      app = "simple-http-server"
    }

    shielded_instance_config {
      enable_secure_boot          = true
      enable_integrity_monitoring = true
    }
  }
}

############################################
# Global static IP for HTTPS Load Balancer / Ingress
############################################
resource "google_compute_global_address" "lb_ip" {
  name = "simple-http-server-ip"
}

############################################
# Outputs
############################################
output "cluster_name" {
  value = google_container_cluster.primary.name
}

output "cluster_endpoint" {
  value     = google_container_cluster.primary.endpoint
  sensitive = true
}

output "artifact_registry_repo" {
  value = "${var.region}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.repo.repository_id}"
}

output "load_balancer_static_ip" {
  value = google_compute_global_address.lb_ip.address
}

output "gke_node_service_account" {
  value = google_service_account.gke_nodes.email
}
