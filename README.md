# Bifröst

Bifröst is a portal for managing Frontend Platform resources. In Norse mythology, Bifröst is a bridge that connects Midgard, the realm of humans, with Asgard, the realm of the gods. At NAV Bifröst is a bridge that connects developers with the resources they need to build and run their frontend applications.

## Features

* [x] Manage Unleash Instances

## Pre-requisites

### Google Clooud Service Account

Bifröst needs a Google Cloud service account with the following roles:

* Cloud SQL Admin

### Google Cloud Resources

Bifröst needs the following Google Cloud resources:

* A Google Cloud PostgreSQL instance for Unleash databases

### Kubernetes Resources

Bifröst needs the following Kubernetes resources:

* [Unleasherator](https://github.com/nais/unleasherator)
* [FQDN Network Policy Operator](https://github.com/GoogleCloudPlatform/gke-fqdnnetworkpolicies-golang)

## Configuration

Bifröst is configured using environment variables. The following variables are required:

### Google Configuration

| Variable                    | Description                 |
|-----------------------------|-----------------------------|
| `BIFROST_GOOGLE_PROJECT_ID` | The Google Cloud project ID |

### OAuth JWT Configuration


| Variable                                          | Description                                    |
|---------------------------------------------------|------------------------------------------------|
| `BIFROST_UNLEASH_INSTANCE_WEB_OAUTH_JWT_AUDIENCE` | The expected audience for OAuth JWT validation |

### Unleash Configuration**

| Variable | Description |
| -------- |  ------- |
| `BIFROST_UNLEASH_INSTANCE_NAMESPACE` | The Kubernetes namespace where Unleash instances are deployed |
| `BIFROST_UNLEASH_INSTANCE_SERVICE_ACCOUNT` | The Kubernetes service account used by Unleash instances |
| `BIFROST_UNLEASH_SQL_INSTANCE_ID` | The SQL instance ID for Unleash databases |
| `BIFROST_UNLEASH_SQL_INSTANCE_REGION` | The SQL instance region for Unleash databases |
| `BIFROST_UNLEASH_SQL_INSTANCE_ADDRESS` | The SQL instance address for Unleash databases |
| `BIFROST_UNLEASH_INSTANCE_WEB_INGRESS_HOST` | The ingress host for Unleash instances Web UI |
| `BIFROST_UNLEASH_INSTANCE_WEB_INGRESS_CLASS` | The ingress class for Unleash instances Web UI |
| `BIFROST_UNLEASH_INSTANCE_API_INGRESS_HOST` | The ingress host for Unleash instances API |
| `BIFROST_UNLEASH_INSTANCE_API_INGRESS_CLASS` | The ingress class for Unleash instances API |

## Local development

### Prerequisite

* Google Cloud Service Account
* Local Kubernetes Cluster

With you local Kubernetes cluster apply the required custom resource definitions:

```bash
kubectl apply -f https://raw.githubusercontent.com/GoogleCloudPlatform/gke-fqdnnetworkpolicies-golang/main/config/crd/bases/networking.gke.io_fqdnnetworkpolicies.yaml
kubectl apply -f https://raw.githubusercontent.com/nais/unleasherator/main/config/crd/bases/unleash.nais.io_unleashes.yaml
```

### Environment variables

The following environment variables needs to be set in addition to the configuration variables above:

| Variable | Value | Description |
| -------- |  ---- | ----------- |
| `BIFROST_SERVER_HOST` | `127.0.0.1` | The host for the Bifröst server |
| `GOOGLE_APPLICATION_CREDENTIALS` | <path-to-file> | Google Cloud service account credentials |
| `KUBECONFIG` | <path-to-file> | Path to Kubernetes configuration file |

### Start the server

```shell
make start
```

## License

[MIT](LICENSE)
