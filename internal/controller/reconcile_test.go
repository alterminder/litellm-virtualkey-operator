package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	litellmv1alpha1 "github.com/alterminder/litellm-virtualkey-operator/api/v1alpha1"
	"github.com/alterminder/litellm-virtualkey-operator/internal/litellm"
)

var _ = Describe("Reconcile option A", func() {
	const namespace = "default"
	const valueDataKey = "value"
	const masterDataKey = "master"
	const guardrailPII = "pii"
	const oldValue = "sk-test-original"
	const newValue = "sk-test-rotation-one"
	const nextValue = "sk-test-rotation-two"
	const masterValue = "sk-test-master"
	var vk *litellmv1alpha1.VirtualKey
	var secret, master *corev1.Secret
	var fake *fakeLiteLLM
	var logs bytes.Buffer
	var testCtx context.Context
	var r *VirtualKeyReconciler
	var cleanup []client.Object
	var forbidden []string

	create := func(obj client.Object) {
		Expect(k8sClient.Create(context.Background(), obj)).To(Succeed())
		cleanup = append(cleanup, obj)
	}
	checkLeaks := func(text string) {
		for _, value := range forbidden {
			Expect(text).NotTo(ContainSubstring(value))
			Expect(text).NotTo(ContainSubstring(litellm.TokenHash(value)))
		}
	}
	refresh := func() {
		Expect(k8sClient.Get(testCtx, client.ObjectKeyFromObject(vk), vk)).To(Succeed())
	}
	reconcile := func() (ctrl.Result, error) {
		copy := *r // A fresh reconciler must recover from server state alone.
		result, err := copy.Reconcile(testCtx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(vk)})
		refresh()
		encoded, encodeErr := json.Marshal(vk.Status)
		Expect(encodeErr).NotTo(HaveOccurred())
		checkLeaks(string(encoded))
		if err != nil {
			checkLeaks(err.Error())
		}
		checkLeaks(logs.String())
		return result, err
	}
	ready := func(reason string) {
		condition := meta.FindStatusCondition(vk.Status.Conditions, litellmv1alpha1.ConditionReady)
		Expect(condition).NotTo(BeNil())
		Expect(condition.Reason).To(Equal(reason))
		status := metav1.ConditionFalse
		if reason == "InSync" {
			status = metav1.ConditionTrue
		}
		Expect(condition.Status).To(Equal(status))
		Expect(condition.ObservedGeneration).To(Equal(vk.Generation))
		Expect(vk.Status.ObservedGeneration).To(Equal(vk.Generation))
	}
	writes := func(generate, update, deleteCount int) {
		Expect(fake.counts()).To(Equal(map[string]int{
			generatePath: generate, updatePath: update, deletePath: deleteCount,
		}))
	}
	rotate := func(value string) {
		secret.Data[valueDataKey] = []byte(value)
		Expect(k8sClient.Update(testCtx, secret)).To(Succeed())
	}
	initial := func() {
		_, err := reconcile()
		Expect(err).NotTo(HaveOccurred())
		ready("InSync")
		fake.resetCalls()
	}
	assertAvailable := func() {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		for _, state := range fake.history {
			_, oldOK := state[litellm.TokenHash(oldValue)]
			_, newOK := state[litellm.TokenHash(newValue)]
			_, nextOK := state[litellm.TokenHash(nextValue)]
			Expect(oldOK || newOK || nextOK).To(BeTrue(), "at least one key must work after every write")
		}
	}

	BeforeEach(func() {
		cleanup = nil
		forbidden = []string{oldValue, newValue, nextValue, masterValue}
		logs.Reset()
		testCtx = logf.IntoContext(context.Background(), zap.New(zap.WriteTo(&logs), zap.UseDevMode(true)))
		fake = newFakeLiteLLM(masterValue)
		secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{GenerateName: "virtual-key-", Namespace: namespace}, Data: map[string][]byte{valueDataKey: []byte(oldValue)}}
		master = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{GenerateName: "master-key-", Namespace: namespace}, Data: map[string][]byte{masterDataKey: []byte(masterValue)}}
		create(secret)
		create(master)
		vk = &litellmv1alpha1.VirtualKey{ObjectMeta: metav1.ObjectMeta{GenerateName: "reconcile-", Namespace: namespace}, Spec: validSpec()}
		vk.Spec.Alias = "reconcile-key-rotating"
		vk.Spec.SecretRef = litellmv1alpha1.SecretKeyReference{Name: secret.Name, Key: valueDataKey}
		create(vk)
		r = &VirtualKeyReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient,
			LiteLLMURL: fake.server.URL, MasterKeySecret: client.ObjectKeyFromObject(master), MasterKeyKey: masterDataKey,
		}
	})
	AfterEach(func() {
		checkLeaks(logs.String())
		fake.server.Close()
		for _, obj := range cleanup {
			if key, ok := obj.(*litellmv1alpha1.VirtualKey); ok {
				if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(key), key); err == nil {
					before := key.DeepCopy()
					key.Finalizers = nil
					Expect(k8sClient.Patch(context.Background(), key, client.MergeFrom(before))).To(Succeed())
				}
			}
			Expect(client.IgnoreNotFound(k8sClient.Delete(context.Background(), obj))).To(Succeed())
		}
	})

	It("generates once and leaves an unchanged key and status untouched", func() {
		result, err := reconcile()
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(10 * time.Minute))
		ready("InSync")
		writes(1, 0, 0)
		Expect(fake.state()[litellm.TokenHash(oldValue)].KeyAlias).To(Equal(vk.Spec.Alias))
		Expect(logs.String()).To(ContainSubstring("Generated LiteLLM key"))
		version := vk.ResourceVersion
		fake.resetCalls()
		logs.Reset()
		_, err = reconcile()
		Expect(err).NotTo(HaveOccurred())
		writes(0, 0, 0)
		Expect(vk.ResourceVersion).To(Equal(version))
		Expect(logs.String()).NotTo(ContainSubstring("INFO"))
	})

	It("adds the finalizer on first sync and deletion bumps generation", func() {
		initial()
		Expect(vk.Finalizers).To(ContainElement("litellm.alter.lab/finalizer"))
		generation := vk.Generation
		Expect(k8sClient.Delete(testCtx, vk)).To(Succeed())
		refresh()
		Expect(vk.DeletionTimestamp.IsZero()).To(BeFalse())
		Expect(vk.Generation).To(BeNumerically(">", generation))
	})

	DescribeTable("finalizes without the value Secret", func(failure string) {
		initial()
		fake.seed(newValue, litellm.KeyInfo{KeyAlias: vk.Spec.Alias + "-rotating"})
		Expect(k8sClient.Delete(testCtx, secret)).To(Succeed())
		Expect(k8sClient.Delete(testCtx, vk)).To(Succeed())
		if failure == "delete" {
			fake.fail(deletePath, 1, strings.Join(forbidden, " "))
		}
		if failure == "master" {
			delete(master.Data, masterDataKey)
			Expect(k8sClient.Update(testCtx, master)).To(Succeed())
		}
		request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(vk)}
		result, err := r.Reconcile(testCtx, request)
		if failure != "" {
			refresh()
			Expect(vk.Finalizers).To(ContainElement("litellm.alter.lab/finalizer"))
			if failure == "delete" {
				Expect(err).To(HaveOccurred())
				checkLeaks(err.Error())
				ready("LiteLLMError")
			} else {
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(time.Minute))
				ready("SecretMissing")
				Expect(fake.totalCalls()).To(BeZero())
				master.Data = map[string][]byte{masterDataKey: []byte(masterValue)}
				Expect(k8sClient.Update(testCtx, master)).To(Succeed())
			}
			encoded, encodeErr := json.Marshal(vk.Status)
			Expect(encodeErr).NotTo(HaveOccurred())
			checkLeaks(string(encoded))
			fake.fail("", 0, "")
			_, err = r.Reconcile(testCtx, request)
		}
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.state()).To(BeEmpty())
		Expect(apierrors.IsNotFound(k8sClient.Get(testCtx, request.NamespacedName, vk))).To(BeTrue())
	}, Entry("deletes both aliases", ""), Entry("retries a LiteLLM 500", "delete"), Entry("waits for the master key", "master"))

	It("removes a colliding VirtualKey finalizer without deleting LiteLLM keys", func() {
		initial()
		other := &litellmv1alpha1.VirtualKey{ObjectMeta: metav1.ObjectMeta{GenerateName: "collision-", Namespace: namespace}, Spec: vk.Spec}
		create(other)
		Expect(vk.Finalizers).To(ContainElement("litellm.alter.lab/finalizer"))
		Expect(k8sClient.Delete(testCtx, vk)).To(Succeed())
		_, err := r.Reconcile(testCtx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(vk)})
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.totalCalls()).To(BeZero())
		Expect(fake.state()).To(HaveKey(litellm.TokenHash(oldValue)))
		Expect(apierrors.IsNotFound(k8sClient.Get(testCtx, client.ObjectKeyFromObject(vk), vk))).To(BeTrue())
	})

	It("updates managed drift once while preserving structured unmanaged metadata", func() {
		initial()
		actual := fake.state()[litellm.TokenHash(oldValue)]
		actual.Models = []string{"model-b", modelA} // Model order is immaterial.
		actual.Metadata["guardrails"] = []any{guardrailPII}
		actual.Metadata["nested"] = map[string]any{"limit": json.Number("9007199254740993")}
		fake.seed(oldValue, actual)
		_, err := reconcile()
		Expect(err).NotTo(HaveOccurred())
		writes(0, 0, 0)
		vk.Spec.Models = []string{"new-model"}
		vk.Spec.UserID = ""
		vk.Spec.Metadata["owner"] = "new-owner"
		Expect(k8sClient.Update(testCtx, vk)).To(Succeed())
		_, err = reconcile()
		Expect(err).NotTo(HaveOccurred())
		ready("InSync")
		writes(0, 1, 0)
		updated := fake.state()[litellm.TokenHash(oldValue)]
		Expect(updated.Models).To(Equal(vk.Spec.Models))
		Expect(updated.UserID).To(BeEmpty())
		Expect(updated.Metadata["owner"]).To(Equal("new-owner"))
		Expect(updated.Metadata["guardrails"]).To(Equal(actual.Metadata["guardrails"]))
		Expect(updated.Metadata["nested"]).To(Equal(actual.Metadata["nested"]))
	})

	It("rotates with one generate, delete, and rename with continuous availability", func() {
		initial()
		rotate(newValue)
		_, err := reconcile()
		Expect(err).NotTo(HaveOccurred())
		ready("InSync")
		writes(1, 1, 1)
		state := fake.state()
		Expect(state).NotTo(HaveKey(litellm.TokenHash(oldValue)))
		Expect(state[litellm.TokenHash(newValue)].KeyAlias).To(Equal(vk.Spec.Alias))
		assertAvailable()
		Expect(strings.Count(logs.String(), "Generated LiteLLM key")).To(Equal(2))
		Expect(strings.Count(logs.String(), "Deleted LiteLLM key")).To(Equal(1))
		Expect(strings.Count(logs.String(), "Updated LiteLLM key")).To(Equal(1))
	})

	DescribeTable("resumes a crash", func(path string, primaryRemains bool) {
		initial()
		rotate(newValue)
		fake.fail(path, 1, strings.Join(forbidden, " "))
		_, err := reconcile()
		Expect(err).To(HaveOccurred())
		ready("LiteLLMError")
		state := fake.state()
		_, oldOK := state[litellm.TokenHash(oldValue)]
		Expect(oldOK).To(Equal(primaryRemains))
		Expect(state[litellm.TokenHash(newValue)].KeyAlias).To(Equal(vk.Spec.Alias + "-rotating"))
		assertAvailable()
		temporary := state[litellm.TokenHash(newValue)]
		temporary.Metadata["guardrails"] = []any{guardrailPII}
		fake.seed(newValue, temporary)
		vk.Spec.Models = []string{"resumed-model"}
		vk.Spec.UserID = "resumed-user"
		vk.Spec.Metadata["owner"] = "resumed-owner"
		Expect(k8sClient.Update(testCtx, vk)).To(Succeed())
		_, err = reconcile()
		Expect(err).NotTo(HaveOccurred())
		ready("InSync")
		state = fake.state()
		resumed := state[litellm.TokenHash(newValue)]
		Expect(resumed.Models).To(Equal(vk.Spec.Models))
		Expect(resumed.UserID).To(Equal(vk.Spec.UserID))
		Expect(resumed.Metadata["owner"]).To(Equal("resumed-owner"))
		Expect(resumed.Metadata["guardrails"]).To(Equal([]any{guardrailPII}))
		Expect(state).NotTo(HaveKey(litellm.TokenHash(oldValue)))
		Expect(state[litellm.TokenHash(newValue)].KeyAlias).To(Equal(vk.Spec.Alias))
		Expect(fake.counts()[generatePath]).To(Equal(1))
		assertAvailable()
	}, Entry("after 6a", deletePath, true), Entry("after 6b (delete 404 is gone)", updatePath, false))

	It("removes superseded temporary v1 before generating v2", func() {
		initial()
		rotate(newValue)
		fake.fail(deletePath, 1, "injected crash")
		_, err := reconcile()
		Expect(err).To(HaveOccurred())
		rotate(nextValue)
		fake.resetCalls()
		fake.fail("", 0, "")
		_, err = reconcile()
		Expect(err).NotTo(HaveOccurred())
		ready("InSync")
		writes(1, 1, 2)
		state := fake.state()
		Expect(state).To(HaveLen(1))
		Expect(state[litellm.TokenHash(nextValue)].KeyAlias).To(Equal(vk.Spec.Alias))
		assertAvailable()
	})

	DescribeTable("cleans a superseded temporary key after 6b and another rotation", func(failCleanup bool) {
		initial()
		rotate(newValue)
		fake.fail(updatePath, 1, "injected rename failure")
		_, err := reconcile()
		Expect(err).To(HaveOccurred())
		state := fake.state()
		Expect(state).To(HaveLen(1))
		Expect(state[litellm.TokenHash(newValue)].KeyAlias).To(Equal(vk.Spec.Alias + "-rotating"))
		rotate(nextValue)
		fake.resetCalls()
		fake.fail("", 0, "")
		if failCleanup {
			fake.fail(deletePath, 1, "injected cleanup failure")
		}
		_, err = reconcile()
		// The replacement primary must work before any temporary-key deletion.
		Expect(fake.state()[litellm.TokenHash(nextValue)].KeyAlias).To(Equal(vk.Spec.Alias))
		if failCleanup {
			Expect(err).To(HaveOccurred())
			ready("LiteLLMError")
			Expect(fake.state()).To(HaveKey(litellm.TokenHash(newValue)))
			fake.resetCalls()
			fake.fail("", 0, "")
			_, err = reconcile()
			Expect(err).NotTo(HaveOccurred())
			writes(0, 0, 1) // Retry cleans up through the existing-primary path (step 3).
		} else {
			Expect(err).NotTo(HaveOccurred())
			writes(1, 0, 1) // Step 5 generates the primary before cleanup.
		}
		ready("InSync")
		state = fake.state()
		Expect(state).To(HaveLen(1))
		Expect(state[litellm.TokenHash(nextValue)].KeyAlias).To(Equal(vk.Spec.Alias))
		func() {
			fake.mu.Lock()
			defer fake.mu.Unlock()
			for _, snapshot := range fake.history {
				Expect(snapshot[litellm.TokenHash(nextValue)].KeyAlias).To(Equal(vk.Spec.Alias))
			}
		}()
		fake.resetCalls()
		_, err = reconcile()
		Expect(err).NotTo(HaveOccurred())
		writes(0, 0, 0)
	}, Entry("after creating the replacement primary", false), Entry("on retry with the current value already primary", true))

	startManager := func() (ctrl.Manager, func()) {
		skipNameValidation := true
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Controller: config.Controller{SkipNameValidation: &skipNameValidation},
			Scheme:     k8sClient.Scheme(), Logger: logf.FromContext(testCtx),
			Cache:   cache.Options{DefaultNamespaces: map[string]cache.Config{namespace: {}}, ReaderFailOnMissingInformer: true},
			Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
		})
		Expect(err).NotTo(HaveOccurred())
		watched := *r
		watched.Client, watched.APIReader = mgr.GetClient(), mgr.GetAPIReader()
		Expect(watched.SetupWithManager(mgr)).To(Succeed())
		managerCtx, stop := context.WithCancel(testCtx)
		done := make(chan error, 1)
		go func() { done <- mgr.Start(managerCtx) }()
		Expect(mgr.GetCache().WaitForCacheSync(managerCtx)).To(BeTrue())
		return mgr, func() { stop(); Expect(<-done).To(Succeed()) }
	}

	It("does not reconcile status-only patches through the manager watch", func() {
		_, stop := startManager()
		defer stop()
		Eventually(func() bool {
			refresh()
			return meta.IsStatusConditionTrue(vk.Status.Conditions, litellmv1alpha1.ConditionReady)
		}, 5*time.Second, 10*time.Millisecond).Should(BeTrue())
		before := fake.totalCalls()
		original := vk.DeepCopy()
		meta.SetStatusCondition(&vk.Status.Conditions, metav1.Condition{
			Type: "Probe", Status: metav1.ConditionTrue, Reason: "Probe", Message: "Status-only patch",
		})
		Expect(k8sClient.Status().Patch(testCtx, vk, client.MergeFrom(original))).To(Succeed())
		Consistently(fake.totalCalls, time.Second, 10*time.Millisecond).Should(Equal(before))
		vk.Spec.Models = []string{"watched-model"}
		Expect(k8sClient.Update(testCtx, vk)).To(Succeed())
		Eventually(func() []string {
			return fake.state()[litellm.TokenHash(oldValue)].Models
		}, 5*time.Second, 10*time.Millisecond).Should(Equal(vk.Spec.Models))
	})

	It("rotates from Secret metadata events and ignores unrelated Secrets", func() {
		mgr, stop := startManager()
		defer stop()
		Eventually(func() string {
			return fake.state()[litellm.TokenHash(oldValue)].KeyAlias
		}, 5*time.Second, 10*time.Millisecond).Should(Equal(vk.Spec.Alias))
		metadata := &metav1.PartialObjectMetadata{}
		metadata.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "Secret"})
		Expect(mgr.GetClient().Get(testCtx, client.ObjectKeyFromObject(secret), metadata)).To(Succeed())
		Expect(mgr.GetClient().Get(testCtx, client.ObjectKeyFromObject(secret), &corev1.Secret{})).To(MatchError(ContainSubstring("not cached")))
		rotate(newValue)
		Eventually(func() string {
			return fake.state()[litellm.TokenHash(newValue)].KeyAlias
		}, 5*time.Second, 10*time.Millisecond).Should(Equal(vk.Spec.Alias))
		Expect(fake.state()).NotTo(HaveKey(litellm.TokenHash(oldValue)))
		// Wait for Ready so no rotation work can overlap the negative control.
		Eventually(func() bool {
			refresh()
			return meta.IsStatusConditionTrue(vk.Status.Conditions, litellmv1alpha1.ConditionReady)
		}, 5*time.Second, 10*time.Millisecond).Should(BeTrue())
		fake.resetCalls()
		unrelated := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{GenerateName: "unrelated-", Namespace: namespace}}
		create(unrelated)
		unrelated.Data = map[string][]byte{valueDataKey: []byte(newValue)}
		Expect(k8sClient.Update(testCtx, unrelated)).To(Succeed())
		Consistently(fake.totalCalls, time.Second, 10*time.Millisecond).Should(BeZero())
		Expect(k8sClient.Delete(testCtx, vk)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(testCtx, client.ObjectKeyFromObject(vk), vk))
		}, 5*time.Second, 10*time.Millisecond).Should(BeTrue())
		Expect(fake.state()).To(BeEmpty())
	})

	It("requeues every VirtualKey when the master Secret changes", func() {
		otherSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{GenerateName: "other-value-", Namespace: namespace}, Data: map[string][]byte{valueDataKey: []byte(newValue)}}
		create(otherSecret)
		other := &litellmv1alpha1.VirtualKey{ObjectMeta: metav1.ObjectMeta{GenerateName: "other-key-", Namespace: namespace}, Spec: validSpec()}
		other.Spec.SecretRef = litellmv1alpha1.SecretKeyReference{Name: otherSecret.Name, Key: valueDataKey}
		create(other)
		_, stop := startManager()
		defer stop()
		Eventually(func() int { return len(fake.state()) }, 5*time.Second, 10*time.Millisecond).Should(Equal(2))
		// Drift both keys without changing their CRs or referenced Secrets.
		for value, alias := range map[string]string{oldValue: vk.Spec.Alias, newValue: other.Spec.Alias} {
			fake.seed(value, litellm.KeyInfo{KeyAlias: alias, Models: []string{"drift"}})
		}
		const rotatedMaster = "sk-test-watch-master"
		forbidden = append(forbidden, rotatedMaster)
		fake.mu.Lock()
		fake.master = rotatedMaster
		fake.mu.Unlock()
		master.Data[masterDataKey] = []byte(rotatedMaster)
		Expect(k8sClient.Update(testCtx, master)).To(Succeed())
		Eventually(func(g Gomega) {
			state := fake.state()
			g.Expect(state[litellm.TokenHash(oldValue)].Models).To(ConsistOf(vk.Spec.Models))
			g.Expect(state[litellm.TokenHash(newValue)].Models).To(ConsistOf(other.Spec.Models))
		}, 5*time.Second, 10*time.Millisecond).Should(Succeed())
	})

	DescribeTable("leaves existing keys untouched for an unusable Secret", func(mode, reason string) {
		initial()
		switch mode {
		case "absent":
			Expect(k8sClient.Delete(testCtx, secret)).To(Succeed())
		case "key":
			delete(secret.Data, valueDataKey)
			Expect(k8sClient.Update(testCtx, secret)).To(Succeed())
		case "empty":
			rotate("")
		case "invalid":
			forbidden = append(forbidden, "invalid-test-value")
			rotate("invalid-test-value")
		}
		result, err := reconcile()
		Expect(err).NotTo(HaveOccurred())
		ready(reason)
		Expect(result.RequeueAfter).To(Equal(time.Minute))
		Expect(fake.totalCalls()).To(BeZero())
		Expect(fake.state()).To(HaveKey(litellm.TokenHash(oldValue)))
	}, Entry("missing Secret", "absent", "SecretMissing"), Entry("missing data key", "key", "SecretMissing"), Entry("invalid value", "invalid", "InvalidSecret"), Entry("empty value", "empty", "InvalidSecret"))

	It("refuses a value found under a foreign alias without writes", func() {
		fake.seed(oldValue, litellm.KeyInfo{KeyAlias: "foreign", Models: []string{modelA}})
		_, err := reconcile()
		Expect(err).To(HaveOccurred())
		ready("LiteLLMError")
		writes(0, 0, 0)
		Expect(fake.state()[litellm.TokenHash(oldValue)].KeyAlias).To(Equal("foreign"))
	})

	It("reports LiteLLM 500 without leaking its response body", func() {
		fake.fail(infoPath, 1, strings.Join(forbidden, " ")+" "+litellm.TokenHash(oldValue))
		_, err := reconcile()
		Expect(err).To(HaveOccurred())
		ready("LiteLLMError")
		writes(0, 0, 0)
	})

	It("reads a rotated master key on the next reconcile", func() {
		initial()
		const rotatedMaster = "sk-test-rotated-master"
		forbidden = append(forbidden, rotatedMaster)
		master.Data[masterDataKey] = []byte(rotatedMaster)
		Expect(k8sClient.Update(testCtx, master)).To(Succeed())
		fake.mu.Lock()
		fake.master = rotatedMaster
		fake.mu.Unlock()
		_, err := reconcile()
		Expect(err).NotTo(HaveOccurred())
		ready("InSync")
		writes(0, 0, 0)
	})

	DescribeTable("rejects alias collisions before deletion", func(alias func(string) string) {
		initial()
		other := &litellmv1alpha1.VirtualKey{ObjectMeta: metav1.ObjectMeta{GenerateName: "collision-", Namespace: namespace}, Spec: validSpec()}
		other.Spec.Alias = alias(vk.Spec.Alias)
		create(other)
		rotate(newValue)
		_, err := reconcile()
		Expect(err).To(HaveOccurred())
		ready("LiteLLMError")
		writes(0, 0, 0)
		Expect(fake.state()).To(HaveKey(litellm.TokenHash(oldValue)))
	}, Entry("duplicate primary", func(a string) string { return a }),
		Entry("another primary uses our temporary alias", func(a string) string { return a + "-rotating" }),
		Entry("our primary uses another temporary alias", func(a string) string { return strings.TrimSuffix(a, "-rotating") }))
})
