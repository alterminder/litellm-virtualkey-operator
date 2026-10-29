# LiteLLM VirtualKey Operator — Design

This document describes the architecture, LiteLLM API contracts, reconciliation algorithm, and security properties of `litellm-virtualkey-operator`.

## Problem Statement

When virtual API keys originate in an external secret store and are synced into Kubernetes Secrets (for example by Infisical or External Secrets Operator), seeding those keys into LiteLLM with a periodic shell Job has six operational defects:

1. Every run rewrites unchanged keys.
2. Deleting before recreating introduces an HTTP `401` outage window.
3. A failed create after deletion leaves the key absent in LiteLLM.
4. Secret rotations wait for the next CronJob schedule to reach LiteLLM.
5. Allowed model lists are hard-coded in scripts rather than declared as Kubernetes resources.
6. Operators must inspect Job logs instead of resource status conditions to determine sync state.

`litellm-virtualkey-operator` replaces periodic seeding with level-triggered reconciliation, metadata-only Secret watches, zero-downtime key rotation, finalizer-driven revocation, and standard Kubernetes status conditions.

## API

Group/version/kind: `litellm.alter.lab/v1alpha1`, kind `VirtualKey`, namespaced (short name `vk`).
The [Go types](../api/v1alpha1/virtualkey_types.go) define the schema.

| Spec field | Contract |
|---|---|
| `alias` | Required and immutable (`self == oldSelf`). 1–246 characters, reserving 9 characters for `-rotating` within LiteLLM's optional 255-character strict alias limit. |
| `secretRef.name`, `secretRef.key` | Required strings, 1–253 characters. Reference a `Secret` in the same namespace as the `VirtualKey`. |
| `models` | Required non-empty set of non-empty model names. LiteLLM treats an empty list as allowing all models, so at least one entry is required. |
| `userID` | Optional string. Maps to LiteLLM `user_id`. |
| `metadata` | Optional string-to-string map. The operator manages only the keys declared in this map and preserves unmanaged keys. |

Status contains `observedGeneration` and standard `metav1.Condition` entries.
The `Ready` condition uses these reasons:

| Reason | Ready | Meaning |
|---|---|---|
| `InSync` | `True` | The LiteLLM key matches the Secret value and all managed spec fields. |
| `SecretMissing` | `False` | A required Secret (value Secret or master-key Secret) or its data key is missing. |
| `InvalidSecret` | `False` | The referenced Secret value does not start with `sk-`. |
| `LiteLLMError` | `False` | A LiteLLM API call failed, returned an invalid response, or found an alias collision. |
| `Rotating` | `False` | Key rotation is in progress. |

The client sends JSON metadata on update and preserves arbitrary JSON values (such as arrays, nested objects, booleans, nulls, and exact integers via `json.Number`) returned by LiteLLM in unmanaged metadata keys. Generation metadata is string-valued because newly created keys contain only `spec.metadata`.

Proxy connection settings are configured on the manager process rather than in custom resources:

- `--litellm-url` (required)
- `--master-key-secret` (default `litellm-credentials`)
- `--master-key-key` (default `LITELLM_MASTER_KEY`)
- `--master-key-namespace` (default downward-API `POD_NAMESPACE`; required if unset)

Both the virtual-key `Secret` and the master-key `Secret` are read via the uncached `APIReader`. The master key is read on every reconcile so master-key rotation is picked up without restarting the operator. Secret watches cache `PartialObjectMetadata` only, never `Secret` data. The manager cache covers only `--master-key-namespace`: the operator, `VirtualKey` resources, referenced Secrets, and master-key Secret share a single namespace (`litellm` in the default kustomize overlay).

## Verified LiteLLM v1.93.2 Behaviour

Source: `BerriAI/litellm` tag **v1.93.2**, commit `cd1bd0f4b8af865f8d05fbd938392fdd4703babc`.
Verified directly against upstream source; all paths below are relative to `BerriAI/litellm`.

| Behaviour | Upstream source (`file:line`) |
|---|---|
| Storage uses lowercase SHA-256 hex of the entire UTF-8 key, including `sk-`. | [`litellm/proxy/utils.py:2955`, `PrismaClient.hash_token`](https://github.com/BerriAI/litellm/blob/cd1bd0f4b8af865f8d05fbd938392fdd4703babc/litellm/proxy/utils.py#L2955-L2959), [`:3587`, `insert_data`](https://github.com/BerriAI/litellm/blob/cd1bd0f4b8af865f8d05fbd938392fdd4703babc/litellm/proxy/utils.py#L3587-L3616) |
| `/v2/key/info` accepts `keys`, `key_aliases`, or both, combines selectors, returns `info: []` when empty, and removes `token` from each entry. | [`litellm/proxy/_types.py:1195`, `KeyRequest`](https://github.com/BerriAI/litellm/blob/cd1bd0f4b8af865f8d05fbd938392fdd4703babc/litellm/proxy/_types.py#L1195-L1204), [`litellm/proxy/management_endpoints/key_management_endpoints.py:3352`, `info_key_fn_v2`](https://github.com/BerriAI/litellm/blob/cd1bd0f4b8af865f8d05fbd938392fdd4703babc/litellm/proxy/management_endpoints/key_management_endpoints.py#L3352-L3395) |
| The `get_data` `find_all` branch hashes list entries only when they start with the case-sensitive prefix `sk-`; `_hash_token_if_needed` uses the same rule. | [`litellm/proxy/utils.py:3281`, `PrismaClient.get_data`](https://github.com/BerriAI/litellm/blob/cd1bd0f4b8af865f8d05fbd938392fdd4703babc/litellm/proxy/utils.py#L3281-L3299), [`:5121`, `_hash_token_if_needed`](https://github.com/BerriAI/litellm/blob/cd1bd0f4b8af865f8d05fbd938392fdd4703babc/litellm/proxy/utils.py#L5121-L5131) |
| Generation rejects custom keys without `sk-` with HTTP 400 (no extra minimum length or character rule). `disable_custom_api_keys=true` rejects custom keys with HTTP 403. | [`litellm/proxy/management_endpoints/key_management_endpoints.py:1004`, `_common_key_generation_helper`](https://github.com/BerriAI/litellm/blob/cd1bd0f4b8af865f8d05fbd938392fdd4703babc/litellm/proxy/management_endpoints/key_management_endpoints.py#L1004-L1020), [`:131`, `_check_custom_key_allowed`](https://github.com/BerriAI/litellm/blob/cd1bd0f4b8af865f8d05fbd938392fdd4703babc/litellm/proxy/management_endpoints/key_management_endpoints.py#L131-L142) |
| Generate and update enforce alias uniqueness across keys; update excludes the current key from that check. | [`litellm/proxy/management_endpoints/key_management_endpoints.py:6359`, `_enforce_unique_key_alias`](https://github.com/BerriAI/litellm/blob/cd1bd0f4b8af865f8d05fbd938392fdd4703babc/litellm/proxy/management_endpoints/key_management_endpoints.py#L6359-L6389), [generate call at :1004](https://github.com/BerriAI/litellm/blob/cd1bd0f4b8af865f8d05fbd938392fdd4703babc/litellm/proxy/management_endpoints/key_management_endpoints.py#L1004-L1009), [update call at :2619](https://github.com/BerriAI/litellm/blob/cd1bd0f4b8af865f8d05fbd938392fdd4703babc/litellm/proxy/management_endpoints/key_management_endpoints.py#L2619-L2623) |
| Alias validation always rejects `..` path segments and control or line-separator characters. Optional strict validation defaults to false; when enabled, it requires 2–255 characters, alphanumeric ends, and only `a-zA-Z0-9_-/.@`. | [`litellm/proxy/management_endpoints/key_management_endpoints.py:6313`, `_validate_key_alias_format`](https://github.com/BerriAI/litellm/blob/cd1bd0f4b8af865f8d05fbd938392fdd4703babc/litellm/proxy/management_endpoints/key_management_endpoints.py#L6313-L6356), [`litellm/secret_managers/base_secret_manager.py:9`, `raise_if_unsafe_secret_name`](https://github.com/BerriAI/litellm/blob/cd1bd0f4b8af865f8d05fbd938392fdd4703babc/litellm/secret_managers/base_secret_manager.py#L9-L21) |
| `/key/update` accepts a raw key or hash as `key` and can rename `key_alias`. | [`litellm/proxy/management_endpoints/key_management_endpoints.py:2010`, `_get_and_validate_existing_key`](https://github.com/BerriAI/litellm/blob/cd1bd0f4b8af865f8d05fbd938392fdd4703babc/litellm/proxy/management_endpoints/key_management_endpoints.py#L2010-L2044), [`:2610`, `update_key_fn`](https://github.com/BerriAI/litellm/blob/cd1bd0f4b8af865f8d05fbd938392fdd4703babc/litellm/proxy/management_endpoints/key_management_endpoints.py#L2610-L2636) |
| `/key/delete` accepts raw keys or hashes in `keys`, or aliases in `key_aliases`. Non-empty `keys` takes precedence. Missing keys can return HTTP 404. | [`litellm/proxy/management_endpoints/key_management_endpoints.py:3191`, `delete_key_fn`](https://github.com/BerriAI/litellm/blob/cd1bd0f4b8af865f8d05fbd938392fdd4703babc/litellm/proxy/management_endpoints/key_management_endpoints.py#L3191-L3209), [`:3951`, `delete_verification_tokens`](https://github.com/BerriAI/litellm/blob/cd1bd0f4b8af865f8d05fbd938392fdd4703babc/litellm/proxy/management_endpoints/key_management_endpoints.py#L3951-L3962) |
| Metadata supports JSON values, including arrays such as `{"guardrails":["pii"]}`. Response metadata must preserve these values. | [`litellm/models/verification_token.py:17`, `LiteLLM_VerificationToken`](https://github.com/BerriAI/litellm/blob/cd1bd0f4b8af865f8d05fbd938392fdd4703babc/litellm/models/verification_token.py#L17-L32), [`litellm/proxy/_types.py:1034`, `GenerateRequestBase`](https://github.com/BerriAI/litellm/blob/cd1bd0f4b8af865f8d05fbd938392fdd4703babc/litellm/proxy/_types.py#L1020-L1034) |
| `/key/update` replaces supplied metadata instead of merging unmanaged keys, while preserving reserved fields. Omitting metadata preserves the existing map. | [`litellm/proxy/management_endpoints/key_management_endpoints.py:1819`, `prepare_metadata_fields`](https://github.com/BerriAI/litellm/blob/cd1bd0f4b8af865f8d05fbd938392fdd4703babc/litellm/proxy/management_endpoints/key_management_endpoints.py#L1819-L1841) |

For example, `example-key-rotating` is 20 characters and passes both baseline and strict alias validation; a 246-character `spec.alias` plus the 9-character `-rotating` suffix fits within the 255-character strict limit.

## Reconcile Algorithm (Option A: Lookup by Secret Value)

All LiteLLM operations use HTTP `POST` JSON bodies; secret values never appear in URLs or query parameters. On any LiteLLM error, the controller sets `Ready=False` (`LiteLLMError`) with a fixed, secret-safe message, updates `observedGeneration`, and returns an error for controller-runtime exponential backoff.

1. Read the referenced `Secret` and data key via `APIReader`. If either is missing, set `Ready=False` (`SecretMissing`) and requeue after 1 minute — never delete the existing LiteLLM key. If the value does not start with `sk-`, set `Ready=False` (`InvalidSecret`), make no LiteLLM call, and requeue after 1 minute.
2. Call `/v2/key/info` with `{"keys":[value]}`.
3. If the value already exists with `key_alias == spec.alias`, compare `models` (as a set), `user_id`, and the keys listed in `spec.metadata`. Equal state performs zero writes. Drift performs a single `/key/update` by value after cloning existing LiteLLM metadata and overwriting only `spec.metadata` keys, preserving all unmanaged JSON values. Once the primary key is confirmed in place, look up `<alias>-rotating` by alias and delete it if present (`404` = already absent). Set `Ready=True` (`InSync`).
4. If the value exists under `<alias>-rotating`, resume an interrupted rotation at step **6b**. If it exists under any other alias, set `Ready=False` (`LiteLLMError`) without modifying that foreign key.
5. If the value is absent, call `/v2/key/info` with `{"key_aliases":[alias]}`. If the alias is absent, generate the supplied value directly under `alias`, then look up `<alias>-rotating` by alias and delete it if present (`404` = already absent) so any superseded temporary key is cleaned up only after the new primary key works. Set `Ready=True` (`InSync`).
6. If `alias` exists with a different value, rotate in order while reporting `Ready=False` (`Rotating`):
   - **6a0:** Look up `<alias>-rotating` by `key_aliases` and delete it if present (`404` = gone). Step 2 already proved the current Secret value is absent, so any temporary key found here holds a superseded value from an earlier interrupted rotation.
   - **6a:** Generate the new Secret value under `<alias>-rotating` with the desired managed fields.
   - **6b:** Delete the old key using `key_aliases: [alias]` (treating HTTP `404` as already gone).
   - **6c:** Update the new key (by value) with `key_alias: alias` and the current desired managed fields, merging any existing metadata on the temporary key. This also applies spec edits made during an interrupted rotation. Set `Ready=True` (`InSync`).

The previous key continues to authenticate requests until step **6b**, while the replacement key authenticates requests starting at step **6a** — there is no window where neither key works. `<alias>-rotating` is reserved for that `VirtualKey`; collisions between `VirtualKey` primary and `-rotating` aliases in the watched namespace are rejected before any write or deletion.

### Crash Recovery

| Interruption point | Next reconcile |
|---|---|
| After **6a** | Lookup by the current Secret value finds `<alias>-rotating`. Resumes at **6b**, deletes old primary, and renames at **6c**. |
| **6b** completed, **6c** failed, then Secret rotated again | Lookup by the newest Secret value and primary alias finds nothing. Step **5** generates the newest primary first, then deletes superseded `<alias>-rotating`. If cleanup fails, step **3** retries cleanup while the primary stays live. |
| After **6b** | Lookup by value finds `<alias>-rotating`. Step **6b** sees `404` (already deleted) and continues to **6c** rename. |
| Secret rotated again (`v1` -> `v2`) before **6b** | Lookup by `v2` finds nothing; primary alias still holds `v0`. Step **6a0** deletes superseded temporary `v1`, then **6a–6c** rotate `v0` -> `v2`. |

### Rejected Alternatives

- **Option B (store previous hash in an operator-managed Secret):** Rejected because it introduces state synchronization, requires `Secret` write RBAC, and persists additional secret-derived material.
- **Option C (compare stored hash from `/v2/key/info`):** Rejected because LiteLLM v1.93.2 strips `token` from `/v2/key/info` responses.

### Logging and Event Discipline

Each successful LiteLLM write logs exactly one `Info` line (`Generated LiteLLM key`, `Updated LiteLLM key`, or `Deleted LiteLLM key`) with `alias` and algorithm `step`. A `404` on delete-by-alias is treated as already absent without logging a write. Steady-state no-op reconciles log only at `V(1)`.

Status is patched only when it changes. `VirtualKey` watch events use `GenerationChangedPredicate` so status-only patches do not retrigger reconciliation or reset error backoff (deleting a finalized custom resource increments `.metadata.generation`, so finalizer execution is still triggered immediately). Successful reconciles set `RequeueAfter: 10m` for periodic read-only drift checks. Condition messages are fixed strings, and `litellm.APIError` exposes only the endpoint path and HTTP status code — never secret values, hashes, transport URLs, or upstream response bodies. No Kubernetes `Event` objects are emitted.

## Watches and Finalizer

- `VirtualKey` resources are indexed by `spec.secretRef.name` so `Secret` events enqueue only referencing `VirtualKey` objects.
- Changes to the master-key `Secret` enqueue all `VirtualKey` objects in the namespace.
- Before the first LiteLLM write, the controller patches finalizer `litellm.alter.lab/finalizer` onto the `VirtualKey`.
- On deletion, the controller reads only the master-key `Secret` (so deletion succeeds even after the value `Secret` has already been removed) and deletes both `spec.alias` and `<spec.alias>-rotating` by `key_aliases` (`404` = gone). The finalizer is removed only after both aliases are confirmed absent.
- If another `VirtualKey` collides with `spec.alias` or `<spec.alias>-rotating`, deletion removes the finalizer without calling LiteLLM so it cannot delete a key owned by the surviving resource. If listing `VirtualKey` objects fails, the finalizer is retained and retried.
- If the master key is missing during deletion, the finalizer is retained with `Ready=False` (`SecretMissing`) and requeued after 1 minute; on LiteLLM errors, the finalizer is retained with `Ready=False` (`LiteLLMError`) and retried with backoff.

## Security, RBAC, and Network Guidance

| Area | Design |
|---|---|
| Secret safety | Raw virtual keys, the master key, and SHA-256 token hashes never appear in logs, status conditions, Kubernetes events, or returned Go errors. HTTP redirects are rejected so `Authorization` headers and key-bearing bodies are never forwarded. |
| RBAC and cache | `config/default` deploys a namespaced `Role` and `RoleBinding` in `litellm` granting `secrets` `get;list;watch` and `virtualkeys` CRUD plus `virtualkeys/status` and `virtualkeys/finalizers`. The manager cache is restricted to that single namespace; no cluster-wide runtime RBAC is required when metrics are disabled (`--metrics-bind-address=0`, the default). |
| Network policy | In restricted clusters, controller pods need egress only to cluster DNS, the Kubernetes API server, and the LiteLLM proxy HTTP port (for example TCP `4000`). No public internet egress is needed. |
| Pod security | Runs under the Kubernetes `restricted` Pod Security Standard: `runAsNonRoot: true` (UID `65532` in `gcr.io/distroless/static:nonroot`), `readOnlyRootFilesystem: true`, `allowPrivilegeEscalation: false`, `capabilities.drop: [ALL]`, and `seccompProfile.type: RuntimeDefault`. |

Budgets, rate limits, teams, users, and `/key/regenerate` are out of scope.

## Implementation Status

1. **Scaffold (Complete):** Kubebuilder v4 API (`litellm.alter.lab/v1alpha1`, `VirtualKey`), CEL validation rules, CRD, and `envtest` suite.
2. **LiteLLM client (Complete):** `/v2/key/info` (by alias and by value), `/key/generate`, `/key/update`, `/key/delete`, redirect rejection, `json.Number` metadata preservation, and secret-safe `APIError`.
3. **Reconciler (Complete):** Option A reconciliation, zero-downtime rotation, crash-recovery paths, structured unmanaged metadata preservation, and `envtest` + fake LiteLLM coverage.
4. **Finalizer and watches (Complete):** `litellm.alter.lab/finalizer`, `PartialObjectMetadata` Secret watches, field indexing, single-namespace cache, and namespaced RBAC.
5. **End-to-end suite (Complete):** Isolated `kind` test suite against real PostgreSQL and LiteLLM proxy (`CGO_ENABLED=0 make test-e2e`).

### E2E Verification Summary

`CGO_ENABLED=0 make test-e2e` creates an isolated `kind` cluster, builds and loads the operator image locally, deploys PostgreSQL and LiteLLM with `prisma db push` schema initialization, generates random `sk-` credentials in memory (passed via stdin/Secrets, never written to disk), and tears down the cluster on exit.

| Component | Pinned test image |
|---|---|
| LiteLLM proxy and `schema-sync` init | `ghcr.io/berriai/litellm-database@sha256:2bcadb9905b692a5ca4dadd51bccaaa23a95dc8a0f9322d9c9787f0ac2aeb50e` |
| PostgreSQL 17 (`17-bookworm`) | `postgres@sha256:839fbb5a4942a7758dd93e18b42ea5649807491a0545b5eeeb8f0b8bb11857a5` |
| Continuous HTTP rotation probe (`8.12.1`) | `curlimages/curl@sha256:94e9e444bcba979c2ea12e27ae39bee4cd10bc7041a472c4727a558e213744e6` |

LiteLLM defaults `UserAPIKeyCacheTTLEnum.in_memory_cache_ttl` to 60 seconds (`litellm/proxy/proxy_server.py:1888-1889`, configurable via [`general_settings.user_api_key_cache_ttl`](https://docs.litellm.ai/docs/proxy/config_settings)). The e2e fixture keeps this default unchanged and allows up to 3 minutes for old-key revocation.

Verified behaviours in the e2e suite:

- **Zero-write metadata reconcile:** Labeling the referenced `Secret` triggers a complete reconcile that performs zero LiteLLM write operations.
- **Sampled continuous availability during rotation:** A probe pod queries `/v1/models` with both `v1` and `v2` every ~200ms across rotation (`both_down=0` across all sampled rounds; e.g. `rounds=7, first_v2_200=3, first_v1_401=3, both_down=0`, old-key revocation observed in `1.168s` on a single-worker proxy).
- **Exact write count:** Rotation logs exactly three writes in order: step `6a` (`Generated LiteLLM key`), step `6b` (`Deleted LiteLLM key`), and step `6c` (`Updated LiteLLM key`).
- **Finalizer revocation:** Deleting the `VirtualKey` revokes `v2` (`HTTP 401`) before the custom resource disappears.
- **Secret-leak verification:** Operator logs, all cluster `Event` objects, and pre-deletion `VirtualKey` status contain none of the generated keys, master key, database password, or their SHA-256 hex digests.

## Decision Log

| Decision | Rationale |
|---|---|
| API group `litellm.alter.lab` | Identifies the `VirtualKey` CRD; Kubernetes API groups are logical identifiers and do not resolve via DNS. |
| Rotation Option A (lookup by Secret value + temporary `<alias>-rotating` key) | Avoids persisting secret-derived state in operator-managed resources and works with LiteLLM v1.93.2, where `/v2/key/info` omits stored token hashes. |
| Require `sk-` prefix and `MaxLength=246` on `spec.alias` | LiteLLM custom-key generation and `/v2/key/info` key-value hashing require `sk-`; reserving 9 characters guarantees `<alias>-rotating` fits LiteLLM's 255-character strict alias limit. |
| Uncached `APIReader` Secret reads + `PartialObjectMetadata` watches | Prevents caching `Secret` payloads in controller memory while still reacting immediately to Secret updates and master-key rotations. |
| Single-namespace cache and namespaced `Role`/`RoleBinding` | Keeps runtime permissions scoped to a single namespace (`litellm` by default) with zero cluster-wide RBAC requirements when metrics are disabled. |
