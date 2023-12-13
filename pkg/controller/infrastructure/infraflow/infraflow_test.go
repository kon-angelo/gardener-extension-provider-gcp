package infraflow_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gstruct"

	"github.com/gardener/gardener-extension-provider-gcp/pkg/controller/infrastructure/infraflow"
	"github.com/gardener/gardener-extension-provider-gcp/pkg/gcp/client"
)

var _ = Describe("Infraflow", func() {
	Describe("firewall rules", func() {

		var (
			fwRules      []*client.Firewall
			clusterName  string
			vpcName      string
			otherVPCName string
		)
		BeforeEach(func() {
			clusterName = "shoot--foobar--gcp"
			vpcName = "my-vpc"
			otherVPCName = "other-vpc"
			fwRules = []*client.Firewall{
				{
					Name:    clusterName + "allow-internal-access",
					Network: vpcName,
				},
				{
					Name:    clusterName + "allow-health-checks",
					Network: vpcName,
				},
				{
					Name:    clusterName + "allow-external-access",
					Network: vpcName,
				},
				{
					Name:    "k8s-foo",
					Network: vpcName,
				},
				{
					Name:    "k8s-bar",
					Network: vpcName,
				},
				{
					Name:    "k8s-other-foo",
					Network: otherVPCName,
				},
			}
		})

		It("should correctly list firewall rules that need to be deleted", func() {
			rules := infraflow.FirewallRulesToDelete(fwRules, clusterName, vpcName)

			Expect(rules).NotTo(ContainElement())
		})
	})
})
