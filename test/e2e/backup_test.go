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
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// hostBackupDir is where kind-up mounted the backup directory on the host
// running this test; nodes see the same directory at /var/nfsz-backups.
func hostBackupDir() string {
	if dir := os.Getenv("NFSZ_BACKUP_HOST_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	Expect(err).NotTo(HaveOccurred())
	return filepath.Join(home, ".nfsz", "backups")
}

var _ = Describe("SharedVolume backup and restore", Ordered, func() {
	const (
		sv = "e2e-bk"
		ns = "e2e-bk"
	)
	// Schedule that never fires during the test; backups are triggered
	// manually via `kubectl create job --from=cronjob/...`.
	svManifest := fmt.Sprintf(`
apiVersion: nfsz.dev/v1alpha1
kind: SharedVolume
metadata:
  name: %s
spec:
  capacity: 1Gi
  namespaces:
    names: [%s]
  backup:
    schedule: "0 0 1 1 *"
    destination:
      hostPath:
        path: /var/nfsz-backups
`, sv, ns)

	BeforeAll(func() {
		// Stale mirrors from earlier runs would make the fresh volume seed
		// unexpectedly (and pass assertions vacuously).
		Expect(os.RemoveAll(filepath.Join(hostBackupDir(), sv))).To(Succeed())
		mustKubectl("create", "ns", ns)
		applyStdin(svManifest)
	})

	AfterAll(func() {
		_, _ = kubectl("delete", "pod", "-n", ns, "--all", "--grace-period=1")
		_, _ = kubectl("delete", "sharedvolume", sv, "--timeout=120s", "--ignore-not-found")
		_, _ = kubectl("delete", "ns", ns, "--wait=false")
		_ = os.RemoveAll(filepath.Join(hostBackupDir(), sv))
	})

	It("reaches Ready (no existing backup: seed is a no-op)", func() {
		Eventually(func() string {
			out, _ := kubectl("get", "sharedvolume", sv, "-o", "jsonpath={.status.phase} {.status.boundSummary}")
			return out
		}, 2*time.Minute, 2*time.Second).Should(Equal("Ready 1/1"))
	})

	It("mirrors volume data to the host directory", func() {
		By("writing a file through a consumer pod")
		applyStdin(mountPod("bk-writer", ns, sv, "echo precious-media > /mnt/movie.txt && sync"))
		mustKubectl("wait", "-n", ns, "--for=jsonpath={.status.phase}=Succeeded", "pod/bk-writer", "--timeout=180s")

		By("triggering the backup CronJob manually")
		mustKubectl("create", "job", "-n", operatorNamespace,
			"--from=cronjob/nfsz-"+sv+"-backup", "backup-now")
		mustKubectl("wait", "-n", operatorNamespace, "--for=condition=complete",
			"job/backup-now", "--timeout=180s")

		By("the mirror is plain readable files on the host")
		data, err := os.ReadFile(filepath.Join(hostBackupDir(), sv, "movie.txt"))
		Expect(err).NotTo(HaveOccurred(),
			"mirror should appear on the host (cluster created by a kind-up with the backup mount?)")
		Expect(string(data)).To(Equal("precious-media\n"))

		mustKubectl("delete", "job", "-n", operatorNamespace, "backup-now")
	})

	It("auto-seeds a recreated volume from the host mirror", func() {
		// Even a Succeeded pod blocks its PVC's deletion (pvc-protection
		// counts any scheduled pod referencing the claim), which would stall
		// the SharedVolume finalizer on the consumer PVC.
		mustKubectl("delete", "pod", "-n", ns, "bk-writer", "--grace-period=1")

		By("destroying the volume and its data (reclaimPolicy Delete)")
		mustKubectl("delete", "sharedvolume", sv, "--timeout=120s")
		Eventually(func() bool {
			_, err := kubectl("get", "pvc", "-n", operatorNamespace, "nfsz-"+sv)
			return err != nil
		}, 2*time.Minute, 2*time.Second).Should(BeTrue(), "backing PVC should be deleted")

		By("recreating the same SharedVolume")
		applyStdin(svManifest)
		Eventually(func() string {
			out, _ := kubectl("get", "sharedvolume", sv, "-o", "jsonpath={.status.phase} {.status.boundSummary}")
			return out
		}, 3*time.Minute, 2*time.Second).Should(Equal("Ready 1/1"))

		By("the Restored condition reports a completed seed")
		out := mustKubectl("get", "sharedvolume", sv,
			"-o", `jsonpath={.status.conditions[?(@.type=="Restored")].reason}`)
		Expect(out).To(Equal("Seeded"))

		By("the data is back, readable through a consumer pod")
		applyStdin(mountPod("bk-reader", ns, sv, "cat /mnt/movie.txt"))
		mustKubectl("wait", "-n", ns, "--for=jsonpath={.status.phase}=Succeeded", "pod/bk-reader", "--timeout=180s")
		logs := mustKubectl("logs", "-n", ns, "bk-reader")
		Expect(logs).To(ContainSubstring("precious-media"))
	})
})
