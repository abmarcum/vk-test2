############################################
# Simple HTTP Server - GCP Infrastructure
############################################

terraform {
  required_version = ">= 1.5.0"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 5.30"
    }
    google-beta = {
      source  = "hashicorp/google-beta"
      version = "~> 5.30"
    }
  }

  backend "gcs" {
    bucket = "REPLACE_ME_TF_STATE_BUCKET"
    prefix = "simple-http-server/state"
  }
}

############################
# Variables
############################

variable "project_id" {
  description = "GCP project ID"
  type        = string
}

variable "region" {
  description = "Primary GCP region"
  type        = string
  default     = "us-central1"
}

variable "zones" {
  description = "Zones for the GKE node pool"
  type        = list(string)
  default     = ["us-central1-a", "us-central1-b", "us-central1-c"]
}

variable "environment" {
  description = "Deployment environment name"
  type        = string
  default     = "production"
}

variable "cluster_name" {
  description = "GKE cluster name"
  type        = string
  default     = "simple-http-server-cluster"
}

variable "node_machine_type" {
  description = "Machine type for GKE nodes"
  type        = string
  default     = "e2-standard-2"
}

variable "min_node_count" {
  type    = number
  default = 3
}

variable "max_node_count" {
  type    = number
  default = 10
}

variable "repository_id" {
  description = "Artifact Registry repository name"
  type        = string
  default     = "simple-http-server"
}

############################
# Providers
############################

provider "google" {
  project = var.project_id
  region  = var.region
}

provider "google-beta" {
  project = var.project_id
  region  = var.region
}

############################
# Enable required APIs
############################

resource "google_project_service" "apis" {
  for_each = toset([
    "compute.googleapis.com",
    "container.googleapis.com",
    "artifactregistry.googleapis.com",
    "iam.googleapis.com",
    "servicenetworking.googleapis.com",
    "cloudresourcemanager.googleapis.com",
    "logging.googleapis.com",
    "monitoring.googleapis.com",
  ])

  project            = var.project_id
  service            = each.value
  disable_on_destroy = false
}

############################
# Networking
############################

resource "google_compute_network" "vpc" {
  name                    = "${var.cluster_name}-vpc"
  auto_create_subnetworks = false
  depends_on              = [google_project_service.apis]
}

resource "google_compute_subnetwork" "subnet" {
  name          = "${var.cluster_name}-subnet"
  ip_cidr_range = "10.10.0.0/20"
  region        = var.region
  network       = google_compute_network.vpc.id

  secondary_ip_range {
    range_name    = "gke-pods"
    ip_cidr_range = "10.20.0.0/14"
  }
  secondary_ip_range {
    range_name    = "gke-services"
    ip_cidr_range = "10.30.0.0/20"
  }

  private_ip_google_access = true
}

resource "google_compute_router" "router" {
  name    = "${var.cluster_name}-router"
  region  = var.region
  network = google_compute_network.vpc.id
}

resource "google_compute_router_nat" "nat" {
  name                               = "${var.cluster_name}-nat"
  router                             = google_compute_router.router.name
  region                              = var.region
  nat_ip_allocate_option              = "AUTO_ONLY"
  source_subnetwork_ip_ranges_to_nat  = "ALL_SUBNETWORKS_ALL_IP_RANGES"
}

# Firewall: allow health checks and LB traffic to reach nodes on 8080/8443
resource "google_compute_firewall" "allow_lb_and_health_checks" {
  name    = "${var.cluster_name}-allow-lb-health-checks"
  network = google_compute_network.vpc.id

  allow {
    protocol = "tcp"
    ports    = ["8080", "8443"]
  }

  # Google LB / health check ranges
  source_ranges = ["130.211.0.0/22", "35.191.0.0/16"]
  target_tags   = ["simple-http-server-node"]
}

resource "google_compute_firewall" "allow_internal" {
  name    = "${var.cluster_name}-allow-internal"
  network = google_compute_network.vpc.id

  allow {
    protocol = "tcp"
    ports    = ["0-65535"]
  }
  allow {
    protocol = "udp"
    ports    = ["0-65535"]
  }

  source_ranges = ["10.10.0.0/20", "10.20.0.0/14", "10.30.0.0/20"]
}

############################
# Static external IP for the Load Balancer
############################

resource "google_compute_global_address" "lb_static_ip" {
  name = "${var.cluster_name}-lb-ip"
}

############################
# GKE Cluster (private, regional, Workload Identity enabled)
############################

resource "google_container_cluster" "primary" {
  name     = var.cluster_name
  location = var.region

  remove_default_node_pool = true
  initial_node_count       = 1

  network    = google_compute_network.vpc.id
  subnetwork = google_compute_subnetwork.subnet.id

  networking_mode = "VPC_NATIVE"
  ip_allocation_policy {
    cluster_secondary_range_name  = "gke-pods"
    services_secondary_range_name = "gke-services"
  }

  private_cluster_config {
    enable_private_nodes    = true
    enable_private_endpoint = false
    master_ipv4_cidr_block  = "172.16.0.0/28"
  }

  master_authorized_networks_config {
    cidr_blocks {
      cidr_block   = "0.0.0.0/0"
      display_name = "all (restrict in production)"
    }
  }

  workload_identity_config {
    workload_pool = "${var.project_id}.svc.id.goog"
  }

  release_channel {
    channel = "REGULAR"
  }

  addons_config {
    http_load_balancing {
      disabled = false
    }
    horizontal_pod_autoscaling {
      disabled = false
    }
  }

  logging_service    = "logging.googleapis.com/kubernetes"
  monitoring_service = "monitoring.googleapis.com/kubernetes"

  depends_on = [google_project_service.apis]
}

resource "google_container_node_pool" "primary_nodes" {
  name     = "${var.cluster_name}-node-pool"
  location = var.region
  cluster  = google_container_cluster.primary.name

  autoscaling {
    min_node_count = var.min_node_count
    max_node_count = var.max_node_count
  }

  node_config {
    machine_type = var.node_machine_type
    tags         = ["simple-http-server-node"]

    oauth_scopes = [
      "https://www.googleapis.com/auth/cloud-platform",
    ]

    workload_metadata_config {
      mode = "GKE_METADATA"
    }

    labels = {
      environment = var.environment
      app         = "simple-http-server"
    }

    shielded_instance_config {
      enable_secure_boot          = true
      enable_integrity_monitoring = true
    }
  }

  management {
    auto_repair  = true
    auto_upgrade = true
  }
}

############################
# Artifact Registry (Docker images)
############################

resource "google_artifact_registry_repository" "docker_repo" {
  location      = var.region
  repository_id = var.repository_id
  format        = "DOCKER"
  description   = "Docker images for Simple HTTP Server"

  depends_on = [google_project_service.apis]
}

############################
# IAM: Workload Identity binding for app service account
############################

resource "google_service_account" "app_sa" {
  account_id   = "simple-http-server"
  display_name = "Simple HTTP Server Workload Identity SA"
}

resource "google_project_iam_member" "app_sa_logging" {
  project = var.project_id
  role    = "roles/logging.logWriter"
  member  = "serviceAccount:${google_service_account.app_sa.email}"
}

resource "google_project_iam_member" "app_sa_monitoring" {
  project = var.project_id
  role    = "roles/monitoring.metricWriter"
  member  = "serviceAccount:${google_service_account.app_sa.email}"
}

resource "google_service_account_iam_binding" "workload_identity_binding" {
  service_account_id = google_service_account.app_sa.name
  role                = "roles/iam.workloadIdentityUser"

  members = [
    "serviceAccount:${var.project_id}.svc.id.goog[simple-http-server/simple-http-server-sa]",
  ]
}

############################
# Outputs
############################

output "cluster_name" {
  value = google_container_cluster.primary.name
}

output "cluster_endpoint" {
  value     = google_container_cluster.primary.endpoint
  sensitive = true
}

output "artifact_registry_repo" {
  value = "${var.region}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.docker_repo.repository_id}"
}

output "load_balancer_static_ip" {
  value = google_compute_global_address.lb_static_ip.address
}

output "app_service_account_email" {
  value = google_service_account.app_sa.email
}
