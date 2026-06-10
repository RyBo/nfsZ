//go:build e2e
// +build e2e

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
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/rybo/nfsZ/test/utils"
)

// operatorNamespace is where the operator and NFS server stacks run.
const operatorNamespace = "nfsz-system"

func kubectl(args ...string) (string, error) {
	return utils.Run(exec.Command("kubectl", args...))
}

func mustKubectl(args ...string) string {
	GinkgoHelper()
	out, err := kubectl(args...)
	Expect(err).NotTo(HaveOccurred(), "kubectl %s failed: %s", strings.Join(args, " "), out)
	return out
}

func applyStdin(manifest string) {
	GinkgoHelper()
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	out, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "apply failed: %s", out)
}

// mountPod returns a pod manifest mounting the SharedVolume PVC.
func mountPod(name, ns, pvc, command string) string {
	return fmt.Sprintf(`
apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
spec:
  terminationGracePeriodSeconds: 1
  restartPolicy: Never
  containers:
    - name: main
      image: busybox:1.36
      command: ["sh", "-c", %q]
      volumeMounts:
        - { name: v, mountPath: /mnt }
  volumes:
    - name: v
      persistentVolumeClaim:
        claimName: %s
`, name, ns, command, pvc)
}

var _ = Describe("SharedVolume", Ordered, func() {
	const sv = "e2e-vol"

	BeforeAll(func() {
		mustKubectl("create", "ns", "e2e-a")
		mustKubectl("create", "ns", "e2e-b")
		applyStdin(fmt.Sprintf(`
apiVersion: nfsz.dev/v1alpha1
kind: SharedVolume
metadata:
  name: %s
spec:
  capacity: 1Gi
  namespaces:
    names: [e2e-a, e2e-b]
    selector:
      matchLabels:
        nfsz.dev/e2e: "true"
`, sv))
	})

	AfterAll(func() {
		_, _ = kubectl("delete", "pod", "-n", "e2e-a", "--all", "--grace-period=1")
		_, _ = kubectl("delete", "pod", "-n", "e2e-b", "--all", "--grace-period=1")
		_, _ = kubectl("delete", "pod", "-n", "e2e-c", "--all", "--grace-period=1")
		_, _ = kubectl("delete", "sharedvolume", sv, "--timeout=120s")
		_, _ = kubectl("delete", "ns", "e2e-a", "e2e-b", "e2e-c", "--wait=false")
	})

	It("reaches Ready with all target namespaces bound", func() {
		Eventually(func() string {
			out, _ := kubectl("get", "sharedvolume", sv, "-o", "jsonpath={.status.phase} {.status.boundSummary}")
			return out
		}, 2*time.Minute, 2*time.Second).Should(Equal("Ready 2/2"))
	})

	It("shares live writes across namespaces", func() {
		applyStdin(mountPod("writer", "e2e-a", sv, "while true; do date >> /mnt/log || exit 1; sleep 1; done"))
		applyStdin(mountPod("reader", "e2e-b", sv, "touch /mnt/log; tail -f /mnt/log"))
		mustKubectl("wait", "-n", "e2e-a", "--for=condition=Ready", "pod/writer", "--timeout=180s")
		mustKubectl("wait", "-n", "e2e-b", "--for=condition=Ready", "pod/reader", "--timeout=180s")

		Eventually(func() int {
			out, _ := kubectl("exec", "-n", "e2e-b", "reader", "--", "sh", "-c", "wc -l < /mnt/log")
			n := 0
			_, _ = fmt.Sscanf(strings.TrimSpace(out), "%d", &n)
			return n
		}, time.Minute, 2*time.Second).Should(BeNumerically(">=", 2),
			"reader in e2e-b should see lines written from e2e-a")
	})

	It("provisions a newly labeled namespace automatically", func() {
		mustKubectl("create", "ns", "e2e-c")
		mustKubectl("label", "ns", "e2e-c", "nfsz.dev/e2e=true")
		Eventually(func() string {
			out, _ := kubectl("get", "pvc", "-n", "e2e-c", sv, "-o", "jsonpath={.status.phase}")
			return out
		}, time.Minute, 2*time.Second).Should(Equal("Bound"))
	})

	It("recovers clients after the NFS server pod is killed", func() {
		before, _ := kubectl("exec", "-n", "e2e-b", "reader", "--", "sh", "-c", "wc -l < /mnt/log")
		mustKubectl("delete", "pod", "-n", operatorNamespace,
			"-l", "nfsz.dev/shared-volume="+sv, "--wait=false")

		var beforeN int
		_, _ = fmt.Sscanf(strings.TrimSpace(before), "%d", &beforeN)
		Eventually(func() int {
			out, _ := kubectl("exec", "-n", "e2e-b", "reader", "--", "sh", "-c", "wc -l < /mnt/log")
			n := 0
			_, _ = fmt.Sscanf(strings.TrimSpace(out), "%d", &n)
			return n
		}, 3*time.Minute, 5*time.Second).Should(BeNumerically(">", beforeN),
			"writes should resume after server restart (grace period + remount)")
	})

	It("cleans up everything on deletion", func() {
		_, _ = kubectl("delete", "pod", "-n", "e2e-a", "writer", "--grace-period=1")
		_, _ = kubectl("delete", "pod", "-n", "e2e-b", "reader", "--grace-period=1")
		mustKubectl("delete", "sharedvolume", sv, "--timeout=120s")

		Eventually(func() string {
			out, _ := kubectl("get", "pv", "-l", "nfsz.dev/shared-volume="+sv, "-o", "name")
			return strings.TrimSpace(out)
		}, 2*time.Minute, 2*time.Second).Should(BeEmpty(), "no orphaned PVs")

		out, _ := kubectl("get", "deploy", "-n", operatorNamespace, "-l", "nfsz.dev/shared-volume="+sv, "-o", "name")
		Expect(strings.TrimSpace(out)).To(BeEmpty(), "no orphaned server deployment")
	})
})
