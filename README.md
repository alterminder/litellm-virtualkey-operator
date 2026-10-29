# litellm-virtualkey-operator

A Kubernetes operator that mirrors `Secret` values managed by an external secrets tool (such as External Secrets Operator or Infisical) into a [LiteLLM](https://github.com/BerriAI/litellm) proxy as virtual keys.

For the full reconciliation algorithm, verified LiteLLM API contracts, and crash-recovery design, see [docs/DESIGN.md](docs/DESIGN.md).

## What It Does

- **Mirrors Kubernetes Secrets into LiteLLM:** Watches a `VirtualKey` custom resource and its referenced Kubernetes `Secret`, creating or updating the corresponding virtual key in LiteLLM with the declared `models`, `user_id`, and `metadata`.
- **Zero writes when unchanged:** Steady-state drift checks and metadata-only `Secret` updates perform zero LiteLLM write calls when the key and managed attributes already match. Unmanaged metadata keys (such as `guardrails` arrays set in LiteLLM) are preserved on update.
- **Zero-downtime rotation:** When the `Secret` value changes, the operator creates the replacement key under `<alias>-rotating` *before* deleting the previous key and renaming `<alias>-rotating` to `<alias>`, avoiding any window where neither key works.
- **Revocation on `VirtualKey` deletion:** A finalizer (`litellm.alter.lab/finalizer`) deletes the key from LiteLLM when the `VirtualKey` resource is deleted. Missing or invalid `Secret` data sets `Ready=False` without deleting an active LiteLLM key.
- **Secret-safe observability:** Secret values, master keys, and SHA-256 token hashes are never written to logs, status conditions, Kubernetes events, or error strings; Secret watches cache metadata only.

## Requirements

- **Kubernetes cluster** v1.25+ (uses CEL `x-kubernetes-validations` for immutable `spec.alias`).
- **LiteLLM proxy with a database** (PostgreSQL; LiteLLM key-management endpoints require database persistence) and custom API keys enabled (`disable_custom_api_keys` must not be `true`).
- **Master-key `Secret`** in the watched namespace (`litellm-credentials` with key `LITELLM_MASTER_KEY` by default).
- **`sk-` key prefix:** Every virtual key value in a referenced `Secret` must start with `sk-` (required by LiteLLM custom-key generation and key-value lookup).

## Installation

By default, the operator runs in and watches **one namespace** (`litellm`). The controller `Deployment`, the LiteLLM master-key `Secret`, all `VirtualKey` resources, and all referenced virtual-key `Secret` objects live in that same namespace.

1. **Create the namespace** (the kustomize bundle does not own the `Namespace` object):

   ```sh
   kubectl create namespace litellm
   ```

2. **Create the LiteLLM master-key `Secret`** (if not already present):

   ```sh
   kubectl -n litellm create secret generic litellm-credentials \
     --from-literal=LITELLM_MASTER_KEY='sk-your-master-key'
   ```

3. **Build and push the operator image:**

   ```sh
   make docker-build docker-push IMG=<registry>/litellm-virtualkey-operator:<tag>
   ```

4. **Install the CRD and deploy the controller:**

   ```sh
   make deploy IMG=<registry>/litellm-virtualkey-operator:<tag>
   ```

   Alternatively, build a single-file manifest bundle under `dist/install.yaml`:

   ```sh
   make build-installer IMG=<registry>/litellm-virtualkey-operator:<tag>
   kubectl apply -f dist/install.yaml
   ```

### Changing the Watched Namespace

Because RBAC is scoped to a single namespace via `+kubebuilder:rbac` markers (no `ClusterRole` is used at runtime), changing the namespace from `litellm` to `<your-namespace>` requires two edits before deploying:

1. Update `namespace=<your-namespace>` on the four `// +kubebuilder:rbac:...` markers in [`internal/controller/virtualkey_controller.go`](internal/controller/virtualkey_controller.go) and regenerate `config/rbac/role.yaml`:

   ```sh
   make manifests
   ```

2. Update `namespace: <your-namespace>` in [`config/default/kustomization.yaml`](config/default/kustomization.yaml) (and `--litellm-url` in [`config/manager/manager.yaml`](config/manager/manager.yaml) if your LiteLLM Service URL differs). `POD_NAMESPACE` is injected via the downward API, so `--master-key-namespace` and the manager cache follow automatically.

## Usage and `VirtualKey` Reference

Create a `Secret` containing an `sk-` key (typically managed by your external secrets operator) and a `VirtualKey` resource in the same namespace:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: example-litellm-key
  namespace: litellm
type: Opaque
stringData:
  API_KEY: sk-replace-with-generated-secret
---
apiVersion: litellm.alter.lab/v1alpha1
kind: VirtualKey
metadata:
  name: example-key
  namespace: litellm
spec:
  alias: example-key
  secretRef:
    name: example-litellm-key
    key: API_KEY
  models:
    - gpt-4o-mini
  userID: example-user
  metadata:
    owner: example-team
    purpose: example
```

Check status with `kubectl -n litellm get virtualkeys` (or `kubectl -n litellm get vk`).

### `spec` Fields

| Field | Required | Description |
|---|---|---|
| `alias` | Yes | LiteLLM `key_alias` (1–246 chars, immutable). `<alias>-rotating` is reserved during key rotation. |
| `secretRef.name` | Yes | Name of the `Secret` in the same namespace holding the raw virtual key. |
| `secretRef.key` | Yes | Data key within `secretRef.name` whose value is the `sk-...` virtual key. |
| `models` | Yes | Non-empty set of model names the key is permitted to call. |
| `userID` | No | LiteLLM `user_id` associated with the key. |
| `metadata` | No | Flat `map[string]string` of managed LiteLLM metadata entries. Unmanaged keys already present on the LiteLLM key are preserved. |

### `Ready` Condition Reasons

| Reason | `Ready` | Meaning |
|---|---|---|
| `InSync` | `True` | The LiteLLM key matches the `Secret` value and all managed `spec` fields. |
| `SecretMissing` | `False` | The referenced virtual-key `Secret`, master-key `Secret`, or data key does not exist (requeued every 1m). |
| `InvalidSecret` | `False` | The virtual-key `Secret` value does not begin with `sk-` (requeued every 1m). |
| `Rotating` | `False` | Key rotation is in progress between temporary generation and final alias rename. |
| `LiteLLMError` | `False` | A LiteLLM API call failed, returned an unexpected payload, or detected an alias collision (retried with backoff). |

## Manager Flags

| Flag | Default | Description |
|---|---|---|
| `--litellm-url` | *(required)* | Base URL of the LiteLLM proxy (`http://litellm.litellm.svc.cluster.local:4000` in `config/manager/manager.yaml`). |
| `--master-key-secret` | `litellm-credentials` | Name of the `Secret` containing the LiteLLM master key. |
| `--master-key-key` | `LITELLM_MASTER_KEY` | Data key inside `--master-key-secret`. |
| `--master-key-namespace` | `$POD_NAMESPACE` | Namespace of the master-key `Secret` and the single namespace watched by the manager cache (required if `POD_NAMESPACE` is unset). |
| `--leader-elect` | `false` | Enable leader election (`true` in `config/manager/manager.yaml`). |
| `--health-probe-bind-address` | `:8081` | Bind address for `/healthz` and `/readyz` probes. |
| `--metrics-bind-address` | `0` | Metrics bind address (`0` disables metrics; `:8443` serves HTTPS, `:8080` serves HTTP). |

Standard `zap` logging flags (`--zap-log-level`, `--zap-devel`, `--zap-encoder`) are also supported.

## Deletion and Rollback Notes

- **Deleting a `VirtualKey` revokes the LiteLLM key:** Every synced `VirtualKey` has finalizer `litellm.alter.lab/finalizer`. Deleting a `VirtualKey` (including via `kubectl delete`, `make undeploy` if CRs are removed first, or GitOps pruning) deletes `<alias>` and `<alias>-rotating` from LiteLLM.
- **Missing Secrets do not revoke keys:** Deleting or breaking the referenced `Secret` marks the `VirtualKey` `Ready=False` (`SecretMissing` or `InvalidSecret`) and leaves the existing key untouched in LiteLLM.
- **Stopping or rolling back the operator without revoking live keys:** If you want to decommission or roll back the operator while keeping existing virtual keys active in LiteLLM:
  1. Scale `deployment/litellm-virtualkey-operator-controller-manager` to `0` replicas and wait for its pod to terminate.
  2. Do **not** delete `VirtualKey` resources while the controller is running. If removing the CRD/CRs later, strip `litellm.alter.lab/finalizer` from the `VirtualKey` objects only after the controller is stopped.

## Development

```sh
# Regenerate CRD/RBAC manifests and DeepCopy code
make manifests generate

# Run unit and envtest controller tests
CGO_ENABLED=0 make test

# Run golangci-lint (includes Kubernetes logcheck plugin)
CGO_ENABLED=0 make lint

# Run end-to-end tests in an isolated Kind cluster (requires Docker, kind, kubectl)
CGO_ENABLED=0 make test-e2e
```

> **Warning:** Always run `make test-e2e` against an isolated Kind cluster (created and cleaned up automatically by the target), never against a shared or production cluster.

## License

Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
