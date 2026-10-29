/*
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
*/

package controller

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	litellmv1alpha1 "github.com/alterminder/litellm-virtualkey-operator/api/v1alpha1"
	"github.com/alterminder/litellm-virtualkey-operator/internal/litellm"
)

const secretRefIndex = "spec.secretRef.name"

const virtualKeyFinalizer = "litellm.alter.lab/finalizer"

var errAliasCollision = errors.New("VirtualKey aliases collide")

// VirtualKeyReconciler reconciles a VirtualKey object.
type VirtualKeyReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	APIReader       client.Reader
	LiteLLMURL      string
	MasterKeySecret types.NamespacedName
	MasterKeyKey    string
}

// +kubebuilder:rbac:groups=litellm.alter.lab,resources=virtualkeys,verbs=get;list;watch;create;update;patch;delete,namespace=litellm
// +kubebuilder:rbac:groups=litellm.alter.lab,resources=virtualkeys/status,verbs=get;update;patch,namespace=litellm
// +kubebuilder:rbac:groups=litellm.alter.lab,resources=virtualkeys/finalizers,verbs=update,namespace=litellm
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch,namespace=litellm

// Reconcile follows DESIGN.md option A without persisting key values or hashes.
func (r *VirtualKeyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var vk litellmv1alpha1.VirtualKey
	if err := r.Get(ctx, req.NamespacedName, &vk); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	logf.FromContext(ctx).V(1).Info("Reconciling VirtualKey", "alias", vk.Spec.Alias, "generation", vk.Generation)
	deleting := !vk.DeletionTimestamp.IsZero()
	if deleting && !controllerutil.ContainsFinalizer(&vk, virtualKeyFinalizer) {
		return ctrl.Result{}, nil
	}
	if err := r.checkAliases(ctx, &vk); err != nil {
		if deleting && errors.Is(err, errAliasCollision) {
			return ctrl.Result{}, r.removeFinalizer(ctx, &vk)
		}
		return ctrl.Result{}, errors.Join(err, r.setReady(ctx, &vk, "LiteLLMError"))
	}
	var value string
	if !deleting {
		var found bool
		var err error
		value, found, err = r.secretValue(ctx, types.NamespacedName{Namespace: vk.Namespace, Name: vk.Spec.SecretRef.Name}, vk.Spec.SecretRef.Key)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !found {
			return ctrl.Result{RequeueAfter: time.Minute}, r.setReady(ctx, &vk, "SecretMissing")
		}
		if !strings.HasPrefix(value, "sk-") {
			return ctrl.Result{RequeueAfter: time.Minute}, r.setReady(ctx, &vk, "InvalidSecret")
		}
	}
	masterKey, found, err := r.secretValue(ctx, r.MasterKeySecret, r.MasterKeyKey)
	if err != nil {
		return ctrl.Result{}, errors.Join(err, r.setReady(ctx, &vk, "SecretMissing"))
	}
	if !found || masterKey == "" {
		return ctrl.Result{RequeueAfter: time.Minute}, r.setReady(ctx, &vk, "SecretMissing")
	}
	api := litellm.New(r.LiteLLMURL, masterKey, &http.Client{Timeout: 30 * time.Second})
	if deleting {
		for _, alias := range []string{vk.Spec.Alias, vk.Spec.Alias + "-rotating"} {
			if err := deleteAlias(ctx, api, alias, "finalizer"); err != nil {
				return ctrl.Result{}, errors.Join(err, r.setReady(ctx, &vk, "LiteLLMError"))
			}
		}
		return ctrl.Result{}, r.removeFinalizer(ctx, &vk)
	}
	if !controllerutil.ContainsFinalizer(&vk, virtualKeyFinalizer) {
		before := vk.DeepCopy()
		controllerutil.AddFinalizer(&vk, virtualKeyFinalizer)
		if err := r.Patch(ctx, &vk, client.MergeFrom(before)); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.syncKey(ctx, &vk, api, value); err != nil {
		return ctrl.Result{}, errors.Join(err, r.setReady(ctx, &vk, "LiteLLMError"))
	}
	return ctrl.Result{RequeueAfter: 10 * time.Minute}, r.setReady(ctx, &vk, "InSync")
}

func (r *VirtualKeyReconciler) removeFinalizer(ctx context.Context, vk *litellmv1alpha1.VirtualKey) error {
	before := vk.DeepCopy()
	controllerutil.RemoveFinalizer(vk, virtualKeyFinalizer)
	return r.Patch(ctx, vk, client.MergeFrom(before))
}

func (r *VirtualKeyReconciler) secretValue(ctx context.Context, name types.NamespacedName, key string) (string, bool, error) {
	var secret corev1.Secret
	if err := r.APIReader.Get(ctx, name, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, errors.New("could not read required Secret")
	}
	value, found := secret.Data[key]
	return string(value), found, nil
}

func (r *VirtualKeyReconciler) syncKey(ctx context.Context, vk *litellmv1alpha1.VirtualKey, api *litellm.Client, value string) error {
	actual, err := api.KeyInfoByValue(ctx, value) // Step 2.
	if err != nil {
		return err
	}
	if actual != nil {
		switch actual.KeyAlias {
		case vk.Spec.Alias:
			// Step 3: keep the current primary available while removing stale temporary keys.
			if err := updateManaged(ctx, api, vk.Spec, value, actual); err != nil {
				return err
			}
			return cleanupTemporary(ctx, api, vk.Spec.Alias, "3")
		case vk.Spec.Alias + "-rotating":
			return r.finishRotation(ctx, vk, api, value, actual) // Step 4: resume at 6b.
		default:
			return errors.New("the Secret value belongs to another alias")
		}
	}
	primary, err := api.KeyInfo(ctx, vk.Spec.Alias) // Step 5.
	if err != nil {
		return err
	}
	if primary == nil {
		if _, err := generateKey(ctx, api, vk.Spec, value, vk.Spec.Alias, "5"); err != nil {
			return err
		}
		return cleanupTemporary(ctx, api, vk.Spec.Alias, "5")
	}
	if err := r.setReady(ctx, vk, "Rotating"); err != nil {
		return err
	}
	if err := cleanupTemporary(ctx, api, vk.Spec.Alias, "6a0"); err != nil {
		return err
	}
	temporary := vk.Spec.Alias + "-rotating"
	generated, err := generateKey(ctx, api, vk.Spec, value, temporary, "6a")
	if err != nil {
		return err
	}
	return r.finishRotation(ctx, vk, api, value, &generated.KeyInfo)
}

func (r *VirtualKeyReconciler) checkAliases(ctx context.Context, vk *litellmv1alpha1.VirtualKey) error {
	var keys litellmv1alpha1.VirtualKeyList
	if err := r.List(ctx, &keys); err != nil {
		return err
	}
	for _, other := range keys.Items {
		if other.UID == vk.UID {
			continue
		}
		if other.Spec.Alias == vk.Spec.Alias || other.Spec.Alias == vk.Spec.Alias+"-rotating" || other.Spec.Alias+"-rotating" == vk.Spec.Alias {
			return errAliasCollision
		}
	}
	return nil
}

func managedUpdate(spec litellmv1alpha1.VirtualKeySpec, actual *litellm.KeyInfo) (litellm.UpdateKeyRequest, bool) {
	models, desired := slices.Clone(actual.Models), slices.Clone(spec.Models)
	slices.Sort(models)
	slices.Sort(desired)
	equal := slices.Equal(models, desired) && actual.UserID == spec.UserID
	metadata := maps.Clone(actual.Metadata)
	if metadata == nil {
		metadata = map[string]any{}
	}
	for key, wanted := range spec.Metadata {
		equal = equal && metadata[key] == wanted
		metadata[key] = wanted
	}
	return litellm.UpdateKeyRequest{Models: &spec.Models, UserID: &spec.UserID, Metadata: &metadata}, equal
}

func updateManaged(ctx context.Context, api *litellm.Client, spec litellmv1alpha1.VirtualKeySpec, value string, actual *litellm.KeyInfo) error {
	update, equal := managedUpdate(spec, actual)
	if equal {
		return nil
	}
	_, err := api.UpdateKey(ctx, value, update)
	if err == nil {
		logf.FromContext(ctx).Info("Updated LiteLLM key", "alias", spec.Alias, "step", "3")
	}
	return err
}

func generateKey(ctx context.Context, api *litellm.Client, spec litellmv1alpha1.VirtualKeySpec, value, alias, step string) (*litellm.GeneratedKey, error) {
	generated, err := api.GenerateKey(ctx, litellm.GenerateKeyRequest{
		Key: value, KeyFields: litellm.KeyFields{KeyAlias: alias, Models: spec.Models, UserID: spec.UserID, Metadata: spec.Metadata},
	})
	if err == nil {
		logf.FromContext(ctx).Info("Generated LiteLLM key", "alias", alias, "step", step)
	}
	return generated, err
}

func cleanupTemporary(ctx context.Context, api *litellm.Client, alias, step string) error {
	temporary := alias + "-rotating"
	stale, err := api.KeyInfo(ctx, temporary)
	if err != nil || stale == nil {
		return err
	}
	return deleteAlias(ctx, api, temporary, step)
}

func deleteAlias(ctx context.Context, api *litellm.Client, alias, step string) error {
	_, err := api.DeleteKeys(ctx, litellm.DeleteKeysRequest{KeyAliases: []string{alias}})
	var apiError *litellm.APIError
	if errors.As(err, &apiError) && apiError.StatusCode == http.StatusNotFound {
		return nil
	}
	if err == nil {
		logf.FromContext(ctx).Info("Deleted LiteLLM key", "alias", alias, "step", step)
	}
	return err
}

func (r *VirtualKeyReconciler) finishRotation(ctx context.Context, vk *litellmv1alpha1.VirtualKey, api *litellm.Client, value string, actual *litellm.KeyInfo) error {
	if err := r.setReady(ctx, vk, "Rotating"); err != nil {
		return err
	}
	if err := deleteAlias(ctx, api, vk.Spec.Alias, "6b"); err != nil {
		return err
	}
	// A resumed rotation can observe a newer spec than the original generation.
	update, _ := managedUpdate(vk.Spec, actual)
	update.KeyAlias = &vk.Spec.Alias
	_, err := api.UpdateKey(ctx, value, update)
	if err == nil {
		logf.FromContext(ctx).Info("Updated LiteLLM key", "alias", vk.Spec.Alias, "step", "6c")
	}
	return err
}

func (r *VirtualKeyReconciler) setReady(ctx context.Context, vk *litellmv1alpha1.VirtualKey, reason string) error {
	before := vk.DeepCopy()
	status := metav1.ConditionFalse
	var message string
	switch reason {
	case "InSync":
		status, message = metav1.ConditionTrue, "The LiteLLM key matches the desired state"
	case "SecretMissing":
		message = "A required Secret or data key is missing"
	case "InvalidSecret":
		message = "The Secret value must start with sk-"
	case "LiteLLMError":
		message = "A LiteLLM operation failed or returned inconsistent state"
	case "Rotating":
		message = "The LiteLLM key rotation is in progress"
	}
	vk.Status.ObservedGeneration = vk.Generation
	meta.SetStatusCondition(&vk.Status.Conditions, metav1.Condition{
		Type: litellmv1alpha1.ConditionReady, Status: status, Reason: reason,
		Message: message, ObservedGeneration: vk.Generation,
	})
	if reflect.DeepEqual(before.Status, vk.Status) {
		return nil
	}
	return r.Status().Patch(ctx, vk, client.MergeFrom(before))
}

// SetupWithManager sets up the controller with the Manager.
func (r *VirtualKeyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &litellmv1alpha1.VirtualKey{}, secretRefIndex,
		func(obj client.Object) []string {
			return []string{obj.(*litellmv1alpha1.VirtualKey).Spec.SecretRef.Name}
		}); err != nil {
		return err
	}
	// Ignore status-only updates; deleting a finalized custom resource bumps generation.
	return ctrl.NewControllerManagedBy(mgr).
		For(&litellmv1alpha1.VirtualKey{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.keysForSecret), builder.OnlyMetadata).
		Named("virtualkey").
		Complete(r)
}

func (r *VirtualKeyReconciler) keysForSecret(ctx context.Context, secret client.Object) []reconcile.Request {
	options := []client.ListOption{client.InNamespace(secret.GetNamespace())}
	if client.ObjectKeyFromObject(secret) != r.MasterKeySecret {
		options = append(options, client.MatchingFields{secretRefIndex: secret.GetName()})
	}
	var keys litellmv1alpha1.VirtualKeyList
	if err := r.List(ctx, &keys, options...); err != nil {
		logf.FromContext(ctx).Error(err, "Could not list VirtualKeys for Secret event")
		return nil
	}
	requests := make([]reconcile.Request, 0, len(keys.Items))
	for _, vk := range keys.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&vk)})
	}
	return requests
}
