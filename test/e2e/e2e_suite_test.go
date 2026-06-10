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
	"os/exec"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/rybo/nfsZ/test/utils"
)

var (
	// managerImage is the manager image to be built and loaded for testing.
	managerImage = "nfsz/manager:e2e"
	// ganeshaImage is the NFS server image SharedVolume servers run.
	ganeshaImage = "nfsz/ganesha:dev"
)

// TestE2E validates nfsZ against a real kind cluster with NFS-capable nodes
// (created by `make kind-up`; run via `make test-e2e`).
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting nfsz e2e test suite\n")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	if img := os.Getenv("GANESHA_IMG"); img != "" {
		ganeshaImage = img
	}

	By("building the manager image")
	cmd := exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", managerImage))
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to build the manager image")

	By("building the ganesha image")
	cmd = exec.Command("make", "ganesha-image", fmt.Sprintf("GANESHA_IMG=%s", ganeshaImage))
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to build the ganesha image")

	By("loading images into Kind")
	Expect(utils.LoadImageToKindClusterWithName(managerImage)).To(Succeed())
	Expect(utils.LoadImageToKindClusterWithName(ganeshaImage)).To(Succeed())

	By("deploying the operator")
	cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", managerImage))
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to deploy the operator")

	cmd = exec.Command("kubectl", "-n", operatorNamespace, "set", "env",
		"deploy/nfsz-controller-manager", "GANESHA_IMAGE="+ganeshaImage)
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())

	cmd = exec.Command("kubectl", "-n", operatorNamespace, "rollout", "status",
		"deploy/nfsz-controller-manager", "--timeout=180s")
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "operator never became ready")
})

var _ = AfterSuite(func() {
	By("undeploying the operator")
	cmd := exec.Command("make", "undeploy", "ignore-not-found=true")
	_, _ = utils.Run(cmd)
})
