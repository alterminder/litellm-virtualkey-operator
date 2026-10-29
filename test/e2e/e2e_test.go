//go:build e2e

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

package e2e

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	litellmv1alpha1 "github.com/alterminder/litellm-virtualkey-operator/api/v1alpha1"
	"github.com/alterminder/litellm-virtualkey-operator/test/utils"
)

const (
	logsCommand       = "logs"
	secretDataKey     = "API_KEY"
	namespace         = "litellm"
	managerDeployment = "litellm-virtualkey-operator-controller-manager"
	reconcileDone     = "Reconcile done, requeueing after 10m0s"
	// Covers the real image's default 60-second auth cache TTL without shortening it.
	authTimeout = 3 * time.Minute
)

var sensitiveValues []string

var _ = Describe("Manager", Ordered, func() {
	var controllerPodName string
	var v1, v2 string

	BeforeAll(func() {
		v1, v2 = randomKey(), randomKey()
		master, password := randomKey(), randomKey()
		By("creating the namespace before deploying the operator")
		run("kubectl", "create", "ns", namespace)
		createSecret("litellm-credentials", map[string]string{
			"LITELLM_MASTER_KEY": master,
			"POSTGRES_PASSWORD":  password,
			"DATABASE_URL":       "postgresql://litellm:" + password + "@postgres:5432/litellm",
		})
		By("starting real Postgres and LiteLLM with schema-sync")
		run("kubectl", "apply", "-f", "test/e2e/testdata/stack.yaml")
		run("kubectl", "rollout", "status", "deployment/postgres", "-n", namespace, "--timeout=3m")
		run("kubectl", "rollout", "status", "deployment/litellm", "-n", namespace, "--timeout=10m")

		By("installing CRDs and deploying the controller-manager")
		run("make", "install")
		run("make", "deploy", "IMG="+managerImage)
		// Existing controller-runtime V(5) logs acknowledge a completed no-op reconcile.
		inputCommand([]map[string]any{{"op": "add", "path": "/spec/template/spec/containers/0/args/-",
			"value": "--zap-log-level=5"}}, "patch", "deployment", managerDeployment,
			"--type=json", "--patch-file=/dev/stdin")
		run("kubectl", "rollout", "status", "deployment/"+managerDeployment, "-n", namespace, "--timeout=3m")
	})

	AfterEach(func() {
		if !CurrentSpecReport().Failed() {
			return
		}
		// Redact even failure diagnostics; never print Secrets or LiteLLM response bodies.
		for _, args := range [][]string{
			{"get", "pods", "-n", namespace, "-o", "wide"},
			{logsCommand, "-n", namespace, "-l", "control-plane=controller-manager", "--tail=-1"},
			{logsCommand, "-n", namespace, "deployment/litellm", "-c", "schema-sync", "--tail=50"},
			{logsCommand, "-n", namespace, "deployment/litellm", "-c", "litellm", "--tail=50"},
			{"get", "events", "-A", "-o", "yaml"},
		} {
			output, _ := utils.Run(exec.Command("kubectl", args...))
			_, _ = fmt.Fprintln(GinkgoWriter, redact(output))
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	It("should run successfully", func() {
		By("validating that the controller-manager pod is running as expected")
		output := run("kubectl", "get", "pods", "-n", namespace, "-l", "control-plane=controller-manager",
			"-o", `go-template={{range .items}}{{if not .metadata.deletionTimestamp}}{{.metadata.name}}{{"\n"}}{{end}}{{end}}`)
		names := strings.Fields(output)
		Expect(names).To(HaveLen(1))
		controllerPodName = names[0]
		Expect(controllerPodName).To(ContainSubstring("controller-manager"))
		Expect(run("kubectl", "get", "pod", controllerPodName, "-n", namespace,
			"-o", "jsonpath={.status.phase}")).To(Equal("Running"))
	})

	It("keeps real LiteLLM keys available through rotation and removes them on deletion", func() {
		logs := func() string {
			return run("kubectl", logsCommand, controllerPodName, "-n", namespace, "--tail=-1")
		}
		probeLogs := func() string {
			return run("kubectl", logsCommand, "rotation-probe", "-n", namespace)
		}
		By("creating a VirtualKey from a generated Secret")
		createSecret("vk-secret", map[string]string{secretDataKey: v1})
		inputCommand(litellmv1alpha1.VirtualKey{
			TypeMeta:   metav1.TypeMeta{APIVersion: "litellm.alter.lab/v1alpha1", Kind: "VirtualKey"},
			ObjectMeta: metav1.ObjectMeta{Name: "e2e-key", Namespace: namespace},
			Spec: litellmv1alpha1.VirtualKeySpec{
				Alias: "e2e-key", Models: []string{"fake-model"},
				SecretRef: litellmv1alpha1.SecretKeyReference{Name: "vk-secret", Key: secretDataKey},
			},
		}, "create", "-f", "-")
		waitInSync()
		Eventually(func() bool { return strings.Contains(logs(), reconcileDone) }).Should(BeTrue())
		createSecret("probe-credentials", map[string]string{"V1": v1, "V2": v2})
		run("kubectl", "create", "-f", "test/e2e/testdata/probe.yaml")
		run("kubectl", "wait", "pod/rotation-probe", "-n", namespace, "--for=condition=Ready", "--timeout=3m")
		Eventually(probeLogs).Should(ContainSubstring("v1=200 v2=401"))

		By("observing a completed metadata-triggered reconcile with zero writes")
		before := logs()
		run("kubectl", "label", "secret/vk-secret", "-n", namespace, "e2e-touch=1")
		var unchanged string
		Eventually(func() bool {
			unchanged = logs()[len(before):]
			return strings.Contains(unchanged, reconcileDone)
		}).Should(BeTrue())
		Expect(writeLines(redact(unchanged))).To(BeEmpty(), "an unchanged Secret must cause zero LiteLLM writes")
		_, _ = fmt.Fprintln(GinkgoWriter, "Zero-write reconcile: PASS (completed after Secret metadata label)")

		By("rotating while continuously probing both keys every 200ms")
		rotationBefore := logs()
		rotationStarted := time.Now()
		inputCommand(map[string]any{"stringData": map[string]string{secretDataKey: v2}},
			"patch", "secret", "vk-secret", "--type=merge", "--patch-file=/dev/stdin")
		var rounds []probeRound
		Eventually(func() bool {
			rounds = parseProbe(probeLogs())
			last := rounds[len(rounds)-1]
			return last.v1 == 401 && last.v2 == 200
		}, authTimeout).Should(BeTrue(), "the new key must work and the old key must eventually expire")
		revocationDelay := time.Since(rotationStarted)
		waitInSync()
		rounds = parseProbe(probeLogs())
		summary := summarizeProbe(rounds)
		Expect(rounds[0].v1).To(Equal(200), "the probe must start with a working v1")
		Expect(rounds[0].v2).To(Equal(401), "the unregistered v2 is the auth negative control")
		Expect(summary.bothDown).To(BeZero(), "no sampled round may have both keys unavailable")
		Expect(summary.firstV2).To(BeNumerically(">", 0))
		Expect(summary.firstV1Denied).To(BeNumerically(">", 0))
		_, _ = fmt.Fprintf(GinkgoWriter,
			"Probe summary: rounds=%d first_v2_200=%d first_v1_401=%d both_down=%d observed_revocation_delay=%s\n",
			len(rounds), summary.firstV2, summary.firstV1Denied, summary.bothDown, revocationDelay.Round(time.Millisecond))
		writes := writeLines(redact(logs()[len(rotationBefore):]))
		Expect(writes).To(HaveLen(3), "exactly one generate, delete and rename per rotation")
		for i, expected := range []struct{ message, step string }{
			{"Generated LiteLLM key", "6a"}, {"Deleted LiteLLM key", "6b"}, {"Updated LiteLLM key", "6c"},
		} {
			// Check the write audit contract without printing raw logs on failure.
			Expect(strings.Contains(writes[i], expected.message)).To(BeTrue(), "unexpected rotation write at index %d", i)
			Expect(strings.Contains(writes[i], `"step": "`+expected.step+`"`)).To(BeTrue(),
				"unexpected rotation step at index %d", i)
		}

		By("capturing status before deleting the VirtualKey and exercising its finalizer")
		status := run("kubectl", "get", "virtualkey/e2e-key", "-n", namespace, "-o", "jsonpath={.status}")
		run("kubectl", "delete", "virtualkey/e2e-key", "-n", namespace, "--wait=false")
		run("kubectl", "wait", "virtualkey/e2e-key", "-n", namespace, "--for=delete", "--timeout=2m")
		Eventually(func() string {
			return run("kubectl", "exec", "rotation-probe", "-n", namespace, "--", "sh", "-c",
				`curl -s -o /dev/null -w '%{http_code}' --max-time 3 -H "Authorization: Bearer $V2" http://litellm:4000/v1/models`)
		}, authTimeout).Should(Equal("401"))
		run("kubectl", "exec", "rotation-probe", "-n", namespace, "--", "touch", "/tmp/stop")
		run("kubectl", "wait", "pod/rotation-probe", "-n", namespace,
			"--for=jsonpath={.status.phase}=Succeeded", "--timeout=30s")

		By("rejecting raw generated credentials and their SHA-256 hashes in logs, events, or status")
		for location, output := range map[string]string{
			"operator logs":                     logs(),
			"cluster events":                    run("kubectl", "get", "events", "-A", "-o", "yaml"),
			"VirtualKey status before deletion": status,
		} {
			for _, value := range sensitiveValues {
				Expect(strings.Contains(output, value)).To(BeFalse(), "generated secret material found in %s", location)
			}
		}
		_, _ = fmt.Fprintln(GinkgoWriter,
			"Leak grep: PASS (operator logs, all events, pre-delete status; keys and SHA-256 hashes)")
	})

	// +kubebuilder:scaffold:e2e-metrics-webhooks-readiness

	// +kubebuilder:scaffold:e2e-webhooks-checks
})

func run(name string, args ...string) string {
	output, err := utils.Run(exec.Command(name, args...))
	if err != nil {
		Fail("command failed: "+redact(output), 1)
	}
	return output
}

// Credentials stay in stdin, never command arguments, fixture files, or failure messages.
func inputCommand(input any, args ...string) {
	data, err := json.Marshal(input)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	cmd := exec.Command("kubectl", append([]string{"-n", namespace}, args...)...)
	cmd.Stdin = bytes.NewReader(data)
	output, err := utils.Run(cmd)
	if err != nil {
		Fail("kubectl input command failed: "+redact(output), 1)
	}
}

func createSecret(name string, data map[string]string) {
	inputCommand(corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		StringData: data,
	}, "create", "-f", "-")
}

func randomKey() string {
	var data [32]byte
	_, err := rand.Read(data[:])
	Expect(err).NotTo(HaveOccurred())
	value := "sk-" + hex.EncodeToString(data[:])
	hash := sha256.Sum256([]byte(value))
	sensitiveValues = append(sensitiveValues, value, hex.EncodeToString(hash[:]))
	return value
}

func redact(output string) string {
	for _, value := range sensitiveValues {
		output = strings.ReplaceAll(output, value, "[REDACTED]")
	}
	return output
}

func waitInSync() {
	Eventually(func() string {
		return run("kubectl", "get", "virtualkey/e2e-key", "-n", namespace,
			"-o", `jsonpath={.status.conditions[?(@.type=="Ready")].status}/{.status.conditions[?(@.type=="Ready")].reason}`)
	}).Should(Equal("True/InSync"))
}

func writeLines(logs string) []string {
	var writes []string
	for line := range strings.SplitSeq(logs, "\n") {
		for _, message := range []string{"Generated LiteLLM key", "Updated LiteLLM key", "Deleted LiteLLM key"} {
			if strings.Contains(line, message) {
				writes = append(writes, line)
				break
			}
		}
	}
	return writes
}

type probeRound struct{ number, v1, v2 int }

func parseProbe(output string) []probeRound {
	lines := utils.GetNonEmptyLines(output)
	rounds := make([]probeRound, 0, len(lines))
	for _, line := range lines {
		var round probeRound
		n, err := fmt.Sscanf(line, "round=%d v1=%d v2=%d", &round.number, &round.v1, &round.v2)
		Expect(err).NotTo(HaveOccurred(), "malformed probe output")
		Expect(n).To(Equal(3))
		Expect(round.number).To(Equal(len(rounds)+1), "probe rounds must be contiguous")
		rounds = append(rounds, round)
	}
	Expect(rounds).NotTo(BeEmpty())
	return rounds
}

type probeSummary struct{ firstV2, firstV1Denied, bothDown int }

func summarizeProbe(rounds []probeRound) probeSummary {
	var summary probeSummary
	for _, round := range rounds {
		if round.v2 == 200 && summary.firstV2 == 0 {
			summary.firstV2 = round.number
		}
		if round.v1 == 401 && summary.firstV1Denied == 0 {
			summary.firstV1Denied = round.number
		}
		if round.v1 != 200 && round.v2 != 200 {
			summary.bothDown++
		}
	}
	return summary
}
