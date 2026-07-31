# GCP infrastructure for "Simple HTTP Server" (GKE + Artifact Registry + LB)
# ============================================================================

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
    bucket = "REPLACE_WITH_TF_STATE_BUCKET"
    prefix = "simple-http-server/state"
  }
}

# ---------------------------------------------------------------------------
# Variables
# ---------------------------------------------------------------------------
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

variable "environment" {
  description = "Deployment environment (dev/staging/prod)"
  type        = string
  default     = "prod"
}

variable "app_name" {
  description = "Application name"
  type        = string
  default     = "simple-http-server"
}

variable "network_name" {
  description = "VPC network name"
  type        = string
  default     = "simple-http-server-vpc"
}

variable "subnet_cidr" {
  description = "Primary subnet CIDR"
  type        = string
  default     = "10.10.0.0/20"
}

variable "pods_cidr" {
  description = "Secondary range for GKE pods"
  type        = string
  default     = "10.20.0.0/16"
}

variable "services_cidr" {
  description = "Secondary range for GKE services"
  type        = string
  default     = "10.30.0.0/20"
}

variable "master_ipv4_cidr" {
  description = "CIDR for GKE private master"
  type        = string
  default     = "172.16.0.0/28"
}

variable "node_machine_type" {
  description = "Machine type for GKE nodes"
  type        = string
  default     = "e2-standard-2"
}

variable "min_node_count" {
  type    = number
  default = 2
}

variable "max_node_count" {
  type    = number
  default = 6
}

variable "authorized_cidrs" {
  description = "CIDRs allowed to reach the GKE master API"
  type        = list(string)
  default     = ["0.0.0.0/0"] # tighten in production
}

# ---------------------------------------------------------------------------
# Provider
# ---------------------------------------------------------------------------
provider "google" {
  project = var.project_id
  region  = var.region
}

provider "google-beta" {
  project = var.project_id
  region  = var.region
}

# ---------------------------------------------------------------------------
# Enable required APIs
# ---------------------------------------------------------------------------
resource "google_project_service" "apis" {
  for_each = toset([
    "container.googleapis.com",
    "compute.googleapis.com",
    "artifactregistry.googleapis.com",
    "iam.googleapis.com",
    "cloudresourcemanager.googleapis.com",
    "logging.googleapis.com",
    "monitoring.googleapis.com",
  ])
  project            = var.project_id
  service            = each.key
  disable_on_destroy = false
}

# ---------------------------------------------------------------------------
# Networking
# ---------------------------------------------------------------------------
resource "google_compute_network" "vpc" {
  name                    = var.network_name
  auto_create_subnetworks = false
  depends_on              = [google_project_service.apis]
}

resource "google_compute_subnetwork" "subnet" {
  name          = "${var.app_name}-subnet"
  ip_cidr_range = var.subnet_cidr
  region        = var.region
  network       = google_compute_network.vpc.id

  secondary_ip_range {
    range_name    = "pods"
    ip_cidr_range = var.pods_cidr
  }

  secondary_ip_range {
    range_name    = "services"
    ip_cidr_range = var.services_cidr
  }

  private_ip_google_access = true
}

resource "google_compute_router" "router" {
  name    = "${var.app_name}-router"
  region  = var.region
  network = google_compute_network.vpc.id
}

resource "google_compute_router_nat" "nat" {
  name                               = "${var.app_name}-nat"
  router                             = google_compute_router.router.name
  region                             = var.region
  nat_ip_allocate_option             = "AUTO_ONLY"
  source_subnetwork_ip_ranges_to_nat = "ALL_SUBNETWORKS_ALL_IP_RANGES"
}

# Static external IP for the L4 Load Balancer service
resource "google_compute_address" "lb_ip" {
  name         = "${var.app_name}-lb-ip"
  region       = var.region
  address_type = "EXTERNAL"
}

# Firewall: allow health checks + LB traffic to node pool
resource "google_compute_firewall" "allow_health_checks" {
  name    = "${var.app_name}-allow-health-checks"
  network = google_compute_network.vpc.id

  allow {
    protocol = "tcp"
    ports    = ["80", "443", "8080", "8443", "9090"]
  }

  source_ranges = [
    "130.211.0.0/22",
    "35.191.0.0/16",
  ]
  target_tags = ["${var.app_name}-node"]
}

resource "google_compute_firewall" "allow_internal" {
  name    = "${var.app_name}-allow-internal"
  network = google_compute_network.vpc.id

  allow {
    protocol = "tcp"
    ports    = ["0-65535"]
  }
  allow {
    protocol = "udp"
    ports    = ["0-65535"]
  }

  source_ranges = [var.subnet_cidr, var.pods_cidr, var.services_cidr]
}

# ---------------------------------------------------------------------------
# IAM: dedicated service account for GKE nodes (least privilege)
# ---------------------------------------------------------------------------
resource "google_service_account" "gke_nodes" {
  account_id   = "${var.app_name}-gke-nodes"
  display_name = "GKE Nodes SA for ${var.app_name}"
}

resource "google_project_iam_member" "gke_node_roles" {
  for_each = toset([
    "roles/logging.logWriter",
    "roles/monitoring.metricWriter",
    "roles/monitoring.viewer",
    "roles/artifactregistry.reader",
    "roles/stackdriver.resourceMetadata.writer",
  ])
  project = var.project_id
  role    = each.key
  member  = "serviceAccount:${google_service_account.gke_nodes.email}"
}

# Workload Identity binding for the application (K8s SA -> GCP SA)
resource "google_service_account" "app_workload_identity" {
  account_id   = "${var.app_name}-gsa"
  display_name = "Workload Identity SA for ${var.app_name}"
}

resource "google_service_account_iam_member" "wi_binding" {
  service_account_id = google_service_account.app_workload_identity.name
  role                = "roles/iam.workloadIdentityUser"
  member              = "serviceAccount:${var.project_id}.svc.id.goog[simple-http-server/simple-http-server]"
}

# ---------------------------------------------------------------------------
# Artifact Registry (Docker images)
# ---------------------------------------------------------------------------
resource "google_artifact_registry_repository" "docker_repo" {
  location      = var.region
  repository_id = var.app_name
  description   = "Docker images for ${var.app_name}"
  format        = "DOCKER"
  depends_on    = [google_project_service.apis]
}

# ---------------------------------------------------------------------------
# GKE Cluster (private, regional, Workload Identity, no default node pool)
# ---------------------------------------------------------------------------
resource "google_container_cluster" "primary" {
  name     = "${var.app_name}-cluster"
  location = var.region
  project  = var.project_id

  network    = google_compute_network.vpc.id
  subnetwork = google_compute_subnetwork.subnet.id

  remove_default_node_pool = true
  initial_node_count       = 1

  release_channel {
    channel = "REGULAR"
  }

  workload_identity_config {
    workload_pool = "${var.project_id}.svc.id.goog"
  }

  ip_allocation_policy {
    cluster_secondary_range_name  = "pods"
    services_secondary_range_name = "services"
  }

  private_cluster_config {
    enable_private_nodes    = true
    enable_private_endpoint = false
    master_ipv4_cidr_block  = var.master_ipv4_cidr
  }

  dynamic "master_authorized_networks_config" {
    for_each = [1]
    content {
      dynamic "cidr_blocks" {
        for_each = var.authorized_cidrs
        content {
          cidr_block   = cidr_blocks.value
          display_name = "authorized-${cidr_blocks.key}"
        }
      }
    }
  }

  addons_config {
    horizontal_pod_autoscaling {
      disabled = false
    }
    http_load_balancing {
      disabled = false
    }
  }

  logging_service    = "logging.googleapis.com/kubernetes"
  monitoring_service = "monitoring.googleapis.com/kubernetes"

  depends_on = [google_project_service.apis]
}

resource "google_container_node_pool" "primary_nodes" {
  name     = "${var.app_name}-node-pool"
  location = var.region
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
    service_account = google_service_account.gke_nodes.email
    oauth_scopes    = ["https://www.googleapis.com/auth/cloud-platform"]
    tags            = ["${var.app_name}-node"]

    labels = {
      app         = var.app_name
      environment = var.environment
    }

    shielded_instance_config {
      enable_secure_boot          = true
      enable_integrity_monitoring = true
    }

    workload_metadata_config {
      mode = "GKE_METADATA"
    }
  }
}

# ---------------------------------------------------------------------------
# Outputs
# ---------------------------------------------------------------------------
output "cluster_name" {
  value = google_container_cluster.primary.name
}

output "cluster_endpoint" {
  value     = google_container_cluster.primary.endpoint
  sensitive = true
}

output "cluster_ca_certificate" {
  value     = google_container_cluster.primary.master_auth[0].cluster_ca_certificate
  sensitive = true
}

output "artifact_registry_repository" {
  value = "${var.region}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.docker_repo.repository_id}"
}

output "load_balancer_static_ip" {
  value = google_compute_address.lb_ip.address
}

output "gke_node_service_account" {
  value = google_service_account.gke_nodes.email
}

output "workload_identity_service_account" {
  value = google_service_account.app_workload_identity.email
}
