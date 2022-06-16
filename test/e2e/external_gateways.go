package e2e

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/onsi/ginkgo"
	ginkgotable "github.com/onsi/ginkgo/extensions/table"
	"github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	"k8s.io/kubernetes/test/e2e/framework/skipper"
)

// This is the image used for the containers acting as externalgateways, built
// out from the e2e/images/Dockerfile.frr dockerfile
const (
	externalContainerImage          = "quay.io/fpaoline/ovnkbfdtest:0.2"
	srcHTTPPort                     = 80
	srcUDPPort                      = 90
	externalGatewayPodIPsAnnotation = "k8s.ovn.org/external-gw-pod-ips"
)

// gatewayTestIPs collects all the addresses required for a external gateway
// test.
type gatewayTestIPs struct {
	gatewayIPs [2]string
	srcPodIP   string
	nodeIP     string
	targetIPs  []string
}

// Validate pods can reach a network running in a container's looback address via
// an external gateway running on eth0 of the container without any tunnel encap.
// The traffic will get proxied through an annotated pod in the default namespace.
var _ = ginkgo.Describe("e2e non-vxlan external gateway through a gateway pod", func() {
	const (
		svcname          string = "externalgw-pod-novxlan"
		gwContainer1     string = "ex-gw-container1"
		gwContainer2     string = "ex-gw-container2"
		ciNetworkName    string = "kind"
		defaultNamespace string = "default"
		srcPingPodName   string = "e2e-exgw-src-ping-pod"
		gatewayPodName1  string = "e2e-gateway-pod1"
		gatewayPodName2  string = "e2e-gateway-pod2"
		externalTCPPort         = 80
		externalUDPPort         = 90
		ecmpRetry        int    = 20
		testTimeout      string = "20"
	)

	var (
		sleepCommand             = []string{"bash", "-c", "sleep 20000"}
		addressesv4, addressesv6 gatewayTestIPs
		clientSet                kubernetes.Interface
	)

	f := framework.NewDefaultFramework(svcname)

	ginkgo.BeforeEach(func() {
		clientSet = f.ClientSet // so it can be used in AfterEach
		// retrieve worker node names
		nodes, err := e2enode.GetBoundedReadySchedulableNodes(f.ClientSet, 3)
		framework.ExpectNoError(err)
		if len(nodes.Items) < 3 {
			framework.Failf(
				"Test requires >= 3 Ready nodes, but there are only %v nodes",
				len(nodes.Items))
		}

		addressesv4, addressesv6 = setupGatewayContainers(f, nodes, gwContainer1, gwContainer2, srcPingPodName, externalUDPPort, externalTCPPort, ecmpRetry)

		_, err = createGenericPod(f, gatewayPodName1, nodes.Items[0].Name, defaultNamespace, sleepCommand)
		framework.ExpectNoError(err)
		_, err = createGenericPod(f, gatewayPodName2, nodes.Items[1].Name, defaultNamespace, sleepCommand)
		framework.ExpectNoError(err)

		for i, gwPod := range []string{gatewayPodName1, gatewayPodName2} {
			networkIPs := fmt.Sprintf("\"%s\"", addressesv4.gatewayIPs[i])
			if addressesv6.srcPodIP != "" && addressesv6.nodeIP != "" {
				networkIPs = fmt.Sprintf("\"%s\", \"%s\"", addressesv4.gatewayIPs[i], addressesv6.gatewayIPs[i])
			}
			annotatePodForGateway(gwPod, f.Namespace.Name, networkIPs, false)
		}
	})

	ginkgo.AfterEach(func() {
		ginkgo.By("Deleting the gateway containers")
		deleteClusterExternalContainer(gwContainer1)
		deleteClusterExternalContainer(gwContainer2)
		ginkgo.By("Deleting the gateway pods")

		deletePodSyncNS(clientSet, defaultNamespace, gatewayPodName1)
		deletePodSyncNS(clientSet, defaultNamespace, gatewayPodName2)
	})

	ginkgotable.DescribeTable("Should validate ICMP connectivity to an external gateway's loopback address via a pod with external gateway annotations enabled",
		func(addresses *gatewayTestIPs, icmpCommand string) {
			if addresses.srcPodIP == "" || addresses.nodeIP == "" {
				skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
			}

			tcpDumpSync := sync.WaitGroup{}
			checkPingOnContainer := func(container string) error {
				defer ginkgo.GinkgoRecover()
				defer tcpDumpSync.Done()
				_, err := runCommand("docker", "exec", container, "timeout", "60", "tcpdump", "-c", "1", icmpCommand)
				framework.ExpectNoError(err, "Failed to detect icmp messages on ", container, srcPingPodName)
				framework.Logf("ICMP packet successfully detected on gateway %s", container)
				return nil
			}

			tcpDumpSync.Add(2)
			go checkPingOnContainer(gwContainer1)
			go checkPingOnContainer(gwContainer2)

			pingSync := sync.WaitGroup{}
			// Verify the external gateway loopback address running on the external container is reachable and
			// that traffic from the source ping pod is proxied through the pod in the default namespace
			ginkgo.By("Verifying connectivity via the gateway namespace to the remote addresses")
			for _, t := range addresses.targetIPs {
				pingSync.Add(1)
				go func(target string) {
					defer ginkgo.GinkgoRecover()
					defer pingSync.Done()
					_, err := framework.RunKubectl(f.Namespace.Name, "exec", srcPingPodName, "--", "ping", "-c", testTimeout, target)
					framework.ExpectNoError(err, "Failed to ping the remote gateway network from pod", target, srcPingPodName)
				}(t)
			}
			pingSync.Wait()
			tcpDumpSync.Wait()

			ginkgo.By(fmt.Sprintf("Verifying connectivity to the pod [%s] from external gateways", addresses.srcPodIP))
			_, err := runCommand("docker", "exec", gwContainer1, "ping", "-c", testTimeout, addresses.srcPodIP)
			framework.ExpectNoError(err, "Failed to ping ", addresses.srcPodIP, gwContainer1)
			_, err = runCommand("docker", "exec", gwContainer2, "ping", "-c", testTimeout, addresses.srcPodIP)
			framework.ExpectNoError(err, "Failed to ping ", addresses.srcPodIP, gwContainer2)
		},
		ginkgotable.Entry("ipv4", &addressesv4, "icmp"),
		ginkgotable.Entry("ipv6", &addressesv6, "icmp6"))

	ginkgotable.DescribeTable("Should validate TCP/UDP connectivity to an external gateway's loopback address via a pod with external gateway annotations enabled",
		func(protocol string, addresses *gatewayTestIPs, destPort, destPortOnPod int) {
			if addresses.srcPodIP == "" || addresses.nodeIP == "" {
				skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
			}

			expectedHostNames := make(map[string]struct{})
			for _, c := range []string{gwContainer1, gwContainer2} {
				res, err := runCommand("docker", "exec", c, "hostname")
				framework.ExpectNoError(err, "failed to run hostname on", c)
				hostname := strings.TrimSuffix(res, "\n")
				framework.Logf("Hostname for %s is %s", c, hostname)
				expectedHostNames[hostname] = struct{}{}
			}
			framework.Logf("Expected hostnames are %v", expectedHostNames)

			ginkgo.By("Checking that external ips are reachable with both gateways")
			returnedHostNames := make(map[string]struct{})
			target := addresses.targetIPs[0]
			success := false
			for i := 0; i < 20; i++ {
				args := []string{"exec", srcPingPodName, "--"}
				if protocol == "tcp" {
					args = append(args, "bash", "-c", fmt.Sprintf("echo | nc -w 1 %s %d", target, destPort))
				} else {
					args = append(args, "bash", "-c", fmt.Sprintf("echo | nc -w 1 -u %s %d", target, destPort))
				}
				res, err := framework.RunKubectl(f.Namespace.Name, args...)
				framework.ExpectNoError(err, "failed to reach ", target, protocol)
				hostname := strings.TrimSuffix(res, "\n")
				if hostname != "" {
					returnedHostNames[hostname] = struct{}{}
				}

				if cmp.Equal(returnedHostNames, expectedHostNames) {
					success = true
					break
				}
			}
			framework.Logf("Received hostnames for protocol %s are %v ", protocol, returnedHostNames)

			if !success {
				framework.Failf("Failed to hit all the external gateways via for protocol %s, diff %s", protocol, cmp.Diff(expectedHostNames, returnedHostNames))
			}

			for _, container := range []string{gwContainer1, gwContainer2} {
				reachPodFromContainer(addresses.srcPodIP, strconv.Itoa(destPortOnPod), srcPingPodName, container, protocol)
			}

		},
		ginkgotable.Entry("UDP ipv4", "udp", &addressesv4, externalUDPPort, srcUDPPort),
		ginkgotable.Entry("TCP ipv4", "tcp", &addressesv4, externalTCPPort, srcHTTPPort),
		ginkgotable.Entry("UDP ipv6", "udp", &addressesv6, externalUDPPort, srcUDPPort),
		ginkgotable.Entry("TCP ipv6", "tcp", &addressesv6, externalTCPPort, srcHTTPPort))
})

// Validate pods can reach a network running in multiple container's loopback
// addresses via two external gateways running on eth0 of the container without
// any tunnel encap. This test defines two external gateways and validates ECMP
// functionality to the container loopbacks. To verify traffic reaches the
// gateways, tcpdump is running on the external gateways and will exit successfully
// once an ICMP packet is received from the annotated pod in the k8s cluster.
// Two additional gateways are added to verify the tcp / udp protocols.
// They run the netexec command, and the pod asks to return their hostname.
// The test checks that both hostnames are collected at least once.
var _ = ginkgo.Describe("e2e multiple external gateway validation", func() {
	const (
		svcname         string = "novxlan-externalgw-ecmp"
		gwContainer1    string = "gw-test-container1"
		gwContainer2    string = "gw-test-container2"
		ciNetworkName   string = "kind"
		testTimeout     string = "30"
		ecmpRetry       int    = 20
		srcPodName             = "e2e-exgw-src-pod"
		externalTCPPort        = 80
		externalUDPPort        = 90
	)

	f := framework.NewDefaultFramework(svcname)

	var addressesv4, addressesv6 gatewayTestIPs

	ginkgo.BeforeEach(func() {
		// retrieve worker node names
		nodes, err := e2enode.GetBoundedReadySchedulableNodes(f.ClientSet, 3)
		framework.ExpectNoError(err)
		if len(nodes.Items) < 3 {
			framework.Failf(
				"Test requires >= 3 Ready nodes, but there are only %v nodes",
				len(nodes.Items))
		}
		addressesv4, addressesv6 = setupGatewayContainers(f, nodes, gwContainer1, gwContainer2, srcPodName, externalUDPPort, externalTCPPort, ecmpRetry)

		// remove the routing external annotation
		annotateArgs := []string{
			"annotate",
			"namespace",
			f.Namespace.Name,
			"k8s.ovn.org/routing-external-gws-",
		}
		ginkgo.By("Resetting the gw annotation")
		framework.RunKubectlOrDie(f.Namespace.Name, annotateArgs...)
	})

	ginkgo.AfterEach(func() {
		// tear down the containers simulating the gateways
		deleteClusterExternalContainer(gwContainer1)
		deleteClusterExternalContainer(gwContainer2)
	})

	ginkgotable.DescribeTable("Should validate ICMP connectivity to multiple external gateways for an ECMP scenario", func(addresses *gatewayTestIPs, icmpToDump string) {
		if addresses.srcPodIP == "" || addresses.nodeIP == "" {
			skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
		}

		annotateNamespaceForGateway(f.Namespace.Name, false, addresses.gatewayIPs[0], addresses.gatewayIPs[1])

		// Verify the gateways and remote loopback addresses are reachable from the pod.
		// Iterate checking connectivity to the loopbacks on the gateways until tcpdump see
		// the traffic or 20 attempts fail. Odds of a false negative here is ~ (1/2)^20
		ginkgo.By("Verifying ecmp connectivity to the external gateways by iterating through the targets")

		// Check for egress traffic to both gateway loopback addresses using tcpdump, since
		// /proc/net/dev counters only record the ingress interface traffic is received on.
		// The test will waits until an ICMP packet is matched on the gateways or fail the
		// test if a packet to the loopback is not received within the timer interval.
		// If an ICMP packet is never detected, return the error via the specified chanel.

		tcpDumpSync := sync.WaitGroup{}

		checkPingOnContainer := func(container string) error {
			defer ginkgo.GinkgoRecover()
			defer tcpDumpSync.Done()
			_, err := runCommand("docker", "exec", container, "timeout", "60", "tcpdump", "-c", "1", icmpToDump)
			framework.ExpectNoError(err, "Failed to detect icmp messages on ", container, srcPodName)
			framework.Logf("ICMP packet successfully detected on gateway %s", container)
			return nil
		}

		tcpDumpSync.Add(2)
		go checkPingOnContainer(gwContainer1)
		go checkPingOnContainer(gwContainer2)

		pingSync := sync.WaitGroup{}

		// spawn a goroutine to asynchronously (to speed up the test)
		// to ping the gateway loopbacks on both containers via ECMP.
		for _, address := range addresses.targetIPs {
			pingSync.Add(1)
			go func(target string) {
				defer ginkgo.GinkgoRecover()
				defer pingSync.Done()
				_, err := framework.RunKubectl(f.Namespace.Name, "exec", srcPodName, "--", "ping", "-c", testTimeout, target)
				if err != nil {
					framework.Logf("error generating a ping from the test pod %s: %v", srcPodName, err)
				}
			}(address)
		}
		pingSync.Wait()
		tcpDumpSync.Wait()

		ginkgo.By("Verifying connectivity to the pod from external gateways")
		_, err := runCommand("docker", "exec", gwContainer1, "ping", "-c", testTimeout, addresses.srcPodIP)
		framework.ExpectNoError(err, "Failed to ping ", addresses.srcPodIP, gwContainer1)
		_, err = runCommand("docker", "exec", gwContainer2, "ping", "-c", testTimeout, addresses.srcPodIP)
		framework.ExpectNoError(err, "Failed to ping ", addresses.srcPodIP, gwContainer2)

		ginkgo.By("Verifying connectivity to the pod from external gateways with large packets > pod MTU")
		_, err = runCommand("docker", "exec", gwContainer1, "ping", "-s", "1420", "-c", testTimeout, addresses.srcPodIP)
		framework.ExpectNoError(err, "Failed to ping ", addresses.srcPodIP, gwContainer1)
		_, err = runCommand("docker", "exec", gwContainer2, "ping", "-s", "1420", "-c", testTimeout, addresses.srcPodIP)
		framework.ExpectNoError(err, "Failed to ping ", addresses.srcPodIP, gwContainer2)

	}, ginkgotable.Entry("IPV4", &addressesv4, "icmp"),
		ginkgotable.Entry("IPV6", &addressesv6, "icmp6"))

	// This test runs a listener on the external container, returning the host name both on tcp and udp.
	// The src pod tries to hit the remote address until both the containers are hit.
	ginkgotable.DescribeTable("Should validate TCP/UDP connectivity to multiple external gateways for a UDP / TCP scenario", func(addresses *gatewayTestIPs, protocol string, destPort, destPortOnPod int) {
		if addresses.srcPodIP == "" || addresses.nodeIP == "" {
			skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
		}

		annotateNamespaceForGateway(f.Namespace.Name, false, addresses.gatewayIPs[0], addresses.gatewayIPs[1])

		expectedHostNames := hostNamesForContainers([]string{gwContainer1, gwContainer2})
		framework.Logf("Expected hostnames are %v", expectedHostNames)

		returnedHostNames := make(map[string]struct{})
		success := false

		// Picking only the first address, the one the udp listener is set for
		target := addresses.targetIPs[0]
		for i := 0; i < 20; i++ {
			hostname := pokeHostnameViaNC(srcPodName, f.Namespace.Name, protocol, target, destPort)
			if hostname != "" {
				returnedHostNames[hostname] = struct{}{}
			}
			if cmp.Equal(returnedHostNames, expectedHostNames) {
				success = true
				break
			}
		}

		framework.Logf("Received hostnames for protocol %s are %v ", protocol, returnedHostNames)

		if !success {
			framework.Failf("Failed to hit all the external gateways via for protocol %s, diff %s", protocol, cmp.Diff(expectedHostNames, returnedHostNames))
		}

		for _, container := range []string{gwContainer1, gwContainer2} {
			reachPodFromContainer(addresses.srcPodIP, strconv.Itoa(destPortOnPod), srcPodName, container, protocol)
		}

	}, ginkgotable.Entry("IPV4 udp", &addressesv4, "udp", externalUDPPort, srcUDPPort),
		ginkgotable.Entry("IPV4 tcp", &addressesv4, "tcp", externalTCPPort, srcHTTPPort),
		ginkgotable.Entry("IPV6 udp", &addressesv6, "udp", externalUDPPort, srcUDPPort),
		ginkgotable.Entry("IPV6 tcp", &addressesv6, "tcp", externalTCPPort, srcHTTPPort))
})

var _ = ginkgo.Describe("e2e multiple external gateway stale conntrack entry deletion validation", func() {
	const (
		svcname          string = "novxlan-externalgw-ecmp"
		gwContainer1     string = "gw-test-container1"
		gwContainer2     string = "gw-test-container2"
		srcPodName       string = "e2e-exgw-src-pod"
		gatewayPodName1  string = "e2e-gateway-pod1"
		gatewayPodName2  string = "e2e-gateway-pod2"
		defaultNamespace string = "default"
	)

	f := wrappedTestFramework(svcname)

	var (
		addressesv4, addressesv6 gatewayTestIPs
		sleepCommand             []string
		nodes                    *v1.NodeList
		err                      error
		clientSet                kubernetes.Interface
	)

	ginkgo.BeforeEach(func() {
		clientSet = f.ClientSet // so it can be used in AfterEach
		// retrieve worker node names
		nodes, err = e2enode.GetBoundedReadySchedulableNodes(clientSet, 3)
		framework.ExpectNoError(err)
		if len(nodes.Items) < 3 {
			framework.Failf(
				"Test requires >= 3 Ready nodes, but there are only %v nodes",
				len(nodes.Items))
		}

		addressesv4, addressesv6 = setupGatewayContainersForConntrackTest(f, nodes, gwContainer1, gwContainer2, srcPodName)
		sleepCommand = []string{"bash", "-c", "sleep 20000"}
		_, err = createGenericPod(f, gatewayPodName1, nodes.Items[0].Name, defaultNamespace, sleepCommand)
		framework.ExpectNoError(err, "Create and annotate the external gw pods to manage the src app pod namespace, failed: %v", err)
		_, err = createGenericPod(f, gatewayPodName2, nodes.Items[1].Name, defaultNamespace, sleepCommand)
		framework.ExpectNoError(err, "Create and annotate the external gw pods to manage the src app pod namespace, failed: %v", err)

		// remove the routing external annotation
		annotateArgs := []string{
			"annotate",
			"namespace",
			f.Namespace.Name,
			"k8s.ovn.org/routing-external-gws-",
		}
		ginkgo.By("Resetting the gw annotation")
		framework.RunKubectlOrDie(f.Namespace.Name, annotateArgs...)
	})

	ginkgo.AfterEach(func() {
		// tear down the containers and pods simulating the gateways
		ginkgo.By("Deleting the gateway containers")
		deleteClusterExternalContainer(gwContainer1)
		deleteClusterExternalContainer(gwContainer2)
		ginkgo.By("Deleting the gateway pods")
		deletePodSyncNS(clientSet, defaultNamespace, gatewayPodName1)
		deletePodSyncNS(clientSet, defaultNamespace, gatewayPodName2)
	})

	ginkgotable.DescribeTable("Namespace annotation: Should validate conntrack entry deletion for TCP/UDP traffic via multiple external gateways a.k.a ECMP routes", func(addresses *gatewayTestIPs, protocol string) {
		if addresses.srcPodIP == "" || addresses.nodeIP == "" {
			skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
		}
		ginkgo.By("Annotate the app namespace to get managed by external gateways")
		annotateNamespaceForGateway(f.Namespace.Name, false, addresses.gatewayIPs[0], addresses.gatewayIPs[1])

		setupIperf3Client := func(container, address string, port int) {
			// note iperf3 even when using udp also spawns tcp connection first; so we indirectly also have the tcp connection when using "-u" flag
			cmd := []string{"docker", "exec", container, "iperf3", "-u", "-c", address, "-p", fmt.Sprintf("%d", port), "-b", "1M", "-i", "1", "-t", "3", "&"}
			_, err := runCommand(cmd...)
			framework.ExpectNoError(err, "failed to setup iperf3 client for %s", container)
		}
		macAddressGW := make([]string, 2)
		for i, containerName := range []string{gwContainer1, gwContainer2} {
			ginkgo.By("Start iperf3 client from external container to connect to iperf3 server running at the src pod")
			setupIperf3Client(containerName, addresses.srcPodIP, 5201+i)
			macAddressExtGW, err := net.ParseMAC(getMACAddressesForNetwork(containerName, externalContainerNetwork))
			framework.ExpectNoError(err, "failed to parse MAC address for %s", containerName)
			macAddressGW[i] = strings.Replace(macAddressExtGW.String(), ":", "", -1)
		}

		ginkgo.By("Check if conntrack entries for ECMP routes are created for the 2 external gateways")
		nodeName := getPod(f, srcPodName).Spec.NodeName
		podConnEntriesWithMACLabelsSet := pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, macAddressGW)
		gomega.Expect(podConnEntriesWithMACLabelsSet).To(gomega.Equal(2))
		totalPodConnEntries := pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, nil)
		gomega.Expect(totalPodConnEntries).To(gomega.Equal(6)) // total conntrack entries for this pod/protocol

		ginkgo.By("Remove second external gateway IP from the app namespace annotation")
		annotateNamespaceForGateway(f.Namespace.Name, false, addresses.gatewayIPs[0])

		ginkgo.By("Check if conntrack entries for ECMP routes are removed for the deleted external gateway if traffic is UDP")
		podConnEntriesWithMACLabelsSet = pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, macAddressGW)
		totalPodConnEntries = pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, nil)
		if protocol == "udp" {
			gomega.Expect(podConnEntriesWithMACLabelsSet).To(gomega.Equal(1)) // we still have the conntrack entry for the remaining gateway
			gomega.Expect(totalPodConnEntries).To(gomega.Equal(5))            // 6-1
		} else {
			gomega.Expect(podConnEntriesWithMACLabelsSet).To(gomega.Equal(2))
			gomega.Expect(totalPodConnEntries).To(gomega.Equal(6))
		}

		ginkgo.By("Remove first external gateway IP from the app namespace annotation")
		annotateNamespaceForGateway(f.Namespace.Name, false, "")

		ginkgo.By("Check if conntrack entries for ECMP routes are removed for the deleted external gateway if traffic is UDP")
		podConnEntriesWithMACLabelsSet = pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, macAddressGW)
		totalPodConnEntries = pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, nil)
		if protocol == "udp" {
			gomega.Expect(podConnEntriesWithMACLabelsSet).To(gomega.Equal(0)) // we don't have any remaining gateways left
			gomega.Expect(totalPodConnEntries).To(gomega.Equal(4))            // 6-2
		} else {
			gomega.Expect(podConnEntriesWithMACLabelsSet).To(gomega.Equal(2))
			gomega.Expect(totalPodConnEntries).To(gomega.Equal(6))
		}

	},
		ginkgotable.Entry("IPV4 udp", &addressesv4, "udp"),
		ginkgotable.Entry("IPV4 tcp", &addressesv4, "tcp"),
		ginkgotable.Entry("IPV6 udp", &addressesv6, "udp"),
		ginkgotable.Entry("IPV6 tcp", &addressesv6, "tcp"))

	ginkgotable.DescribeTable("ExternalGWPod annotation: Should validate conntrack entry deletion for TCP/UDP traffic via multiple external gateways a.k.a ECMP routes", func(addresses *gatewayTestIPs, protocol string) {
		if addresses.srcPodIP == "" || addresses.nodeIP == "" {
			skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
		}
		ginkgo.By("Annotate the external gw pods to manage the src app pod namespace")
		for i, gwPod := range []string{gatewayPodName1, gatewayPodName2} {
			networkIPs := fmt.Sprintf("\"%s\"", addresses.gatewayIPs[i])
			if addresses.srcPodIP != "" && addresses.nodeIP != "" {
				networkIPs = fmt.Sprintf("\"%s\", \"%s\"", addresses.gatewayIPs[i], addresses.gatewayIPs[i])
			}
			annotatePodForGateway(gwPod, f.Namespace.Name, networkIPs, false)
		}

		// ensure the conntrack deletion tracker annotation is updated
		ginkgo.By("Check if the k8s.ovn.org/external-gw-pod-ips got updated for the app namespace")
		err := wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
			ns := getNamespace(f, f.Namespace.Name)
			return (ns.Annotations[externalGatewayPodIPsAnnotation] == fmt.Sprintf("%s,%s", addresses.gatewayIPs[0], addresses.gatewayIPs[1])), nil
		})
		framework.ExpectNoError(err, "Check if the k8s.ovn.org/external-gw-pod-ips got updated, failed: %v", err)

		setupIperf3Client := func(container, address string, port int) {
			// note iperf3 even when using udp also spawns tcp connection first; so we indirectly also have the tcp connection when using "-u" flag
			cmd := []string{"docker", "exec", container, "iperf3", "-u", "-c", address, "-p", fmt.Sprintf("%d", port), "-b", "1M", "-i", "1", "-t", "3", "&"}
			_, err := runCommand(cmd...)
			framework.ExpectNoError(err, "failed to setup iperf3 client for %s", container)
		}
		macAddressGW := make([]string, 2)
		for i, containerName := range []string{gwContainer1, gwContainer2} {
			ginkgo.By("Start iperf3 client from external container to connect to iperf3 server running at the src pod")
			setupIperf3Client(containerName, addresses.srcPodIP, 5201+i)
			macAddressExtGW, err := net.ParseMAC(getMACAddressesForNetwork(containerName, externalContainerNetwork))
			framework.ExpectNoError(err, "failed to parse MAC address for %s", containerName)
			macAddressGW[i] = strings.Replace(macAddressExtGW.String(), ":", "", -1)
		}

		ginkgo.By("Check if conntrack entries for ECMP routes are created for the 2 external gateways")
		nodeName := getPod(f, srcPodName).Spec.NodeName
		podConnEntriesWithMACLabelsSet := pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, macAddressGW)
		gomega.Expect(podConnEntriesWithMACLabelsSet).To(gomega.Equal(2))
		totalPodConnEntries := pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, nil)
		gomega.Expect(totalPodConnEntries).To(gomega.Equal(6)) // total conntrack entries for this pod/protocol

		ginkgo.By("Remove second external gateway pod's routing-namespace annotation")
		annotatePodForGateway(gatewayPodName2, "", addresses.gatewayIPs[1], false)

		// ensure the conntrack deletion tracker annotation is updated
		ginkgo.By("Check if the k8s.ovn.org/external-gw-pod-ips got updated for the app namespace")
		err = wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
			ns := getNamespace(f, f.Namespace.Name)
			return (ns.Annotations[externalGatewayPodIPsAnnotation] == fmt.Sprintf("%s", addresses.gatewayIPs[0])), nil
		})
		framework.ExpectNoError(err, "Check if the k8s.ovn.org/external-gw-pod-ips got updated, failed: %v", err)

		ginkgo.By("Check if conntrack entries for ECMP routes are removed for the deleted external gateway if traffic is UDP")
		podConnEntriesWithMACLabelsSet = pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, macAddressGW)
		totalPodConnEntries = pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, nil)
		if protocol == "udp" {
			gomega.Expect(podConnEntriesWithMACLabelsSet).To(gomega.Equal(1)) // we still have the conntrack entry for the remaining gateway
			gomega.Expect(totalPodConnEntries).To(gomega.Equal(5))            // 6-1
		} else {
			gomega.Expect(podConnEntriesWithMACLabelsSet).To(gomega.Equal(2))
			gomega.Expect(totalPodConnEntries).To(gomega.Equal(6))
		}

		ginkgo.By("Remove first external gateway pod's routing-namespace annotation")
		annotatePodForGateway(gatewayPodName1, "", addresses.gatewayIPs[0], false)

		// ensure the conntrack deletion tracker annotation is updated
		ginkgo.By("Check if the k8s.ovn.org/external-gw-pod-ips got updated for the app namespace")
		err = wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
			ns := getNamespace(f, f.Namespace.Name)
			return (ns.Annotations[externalGatewayPodIPsAnnotation] == ""), nil
		})
		framework.ExpectNoError(err, "Check if the k8s.ovn.org/external-gw-pod-ips got updated, failed: %v", err)

		ginkgo.By("Check if conntrack entries for ECMP routes are removed for the deleted external gateway if traffic is UDP")
		podConnEntriesWithMACLabelsSet = pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, macAddressGW)
		totalPodConnEntries = pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, nil)
		if protocol == "udp" {
			gomega.Expect(podConnEntriesWithMACLabelsSet).To(gomega.Equal(0)) // we don't have any remaining gateways left
			gomega.Expect(totalPodConnEntries).To(gomega.Equal(4))            // 6-2
		} else {
			gomega.Expect(podConnEntriesWithMACLabelsSet).To(gomega.Equal(2))
			gomega.Expect(totalPodConnEntries).To(gomega.Equal(6))
		}

	},
		ginkgotable.Entry("IPV4 udp", &addressesv4, "udp"),
		ginkgotable.Entry("IPV4 tcp", &addressesv4, "tcp"),
		ginkgotable.Entry("IPV6 udp", &addressesv6, "udp"),
		ginkgotable.Entry("IPV6 tcp", &addressesv6, "tcp"))
})

// BFD Tests are dual of external gateway. The only difference is that they enable BFD on ovn and
// on the external containers, and after doing one round veryfing that the traffic reaches both containers,
// they delete one and verify that the traffic is always reaching the only alive container.
var _ = ginkgo.Context("BFD", func() {
	var _ = ginkgo.Describe("e2e non-vxlan external gateway through a gateway pod", func() {
		const (
			svcname          string = "externalgw-pod-novxlan"
			gwContainer1     string = "ex-gw-container1"
			gwContainer2     string = "ex-gw-container2"
			ciNetworkName    string = "kind"
			defaultNamespace string = "default"
			srcPingPodName   string = "e2e-exgw-src-ping-pod"
			gatewayPodName1  string = "e2e-gateway-pod1"
			gatewayPodName2  string = "e2e-gateway-pod2"
			externalTCPPort         = 80
			externalUDPPort         = 90
			ecmpRetry        int    = 20
			testTimeout      string = "20"
		)

		var (
			sleepCommand             = []string{"bash", "-c", "sleep 20000"}
			addressesv4, addressesv6 gatewayTestIPs
			clientSet                kubernetes.Interface
		)

		f := framework.NewDefaultFramework(svcname)

		ginkgo.BeforeEach(func() {
			clientSet = f.ClientSet // so it can be used in AfterEach
			// retrieve worker node names
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(f.ClientSet, 3)
			framework.ExpectNoError(err)
			if len(nodes.Items) < 3 {
				framework.Failf(
					"Test requires >= 3 Ready nodes, but there are only %v nodes",
					len(nodes.Items))
			}

			setupBFD := setupBFDOnContainer(nodes.Items)
			addressesv4, addressesv6 = setupGatewayContainers(f, nodes, gwContainer1, gwContainer2, srcPingPodName, externalUDPPort, externalTCPPort, ecmpRetry, setupBFD)

			_, err = createGenericPod(f, gatewayPodName1, nodes.Items[0].Name, defaultNamespace, sleepCommand)
			framework.ExpectNoError(err)
			_, err = createGenericPod(f, gatewayPodName2, nodes.Items[1].Name, defaultNamespace, sleepCommand)
			framework.ExpectNoError(err)

			for i, gwPod := range []string{gatewayPodName1, gatewayPodName2} {
				networkIPs := fmt.Sprintf("\"%s\"", addressesv4.gatewayIPs[i])
				if addressesv6.srcPodIP != "" && addressesv6.nodeIP != "" {
					networkIPs = fmt.Sprintf("\"%s\", \"%s\"", addressesv4.gatewayIPs[i], addressesv6.gatewayIPs[i])
				}
				annotatePodForGateway(gwPod, f.Namespace.Name, networkIPs, true)
			}
			// This is needed for bfd to sync up
			time.Sleep(3 * time.Second)
		})

		ginkgo.AfterEach(func() {
			ginkgo.By("Deleting the gateway containers")
			deleteClusterExternalContainer(gwContainer1)
			deleteClusterExternalContainer(gwContainer2)
			ginkgo.By("Deleting the gateway pods")

			deletePodSyncNS(clientSet, defaultNamespace, gatewayPodName1)
			deletePodSyncNS(clientSet, defaultNamespace, gatewayPodName2)
		})

		ginkgotable.DescribeTable("Should validate ICMP connectivity to an external gateway's loopback address via a pod with external gateway annotations enabled",
			func(addresses *gatewayTestIPs, icmpCommand string) {
				if addresses.srcPodIP == "" || addresses.nodeIP == "" {
					skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
				}

				framework.ExpectEqual(isBFDPaired(gwContainer1, addresses.nodeIP), true, "Bfd not paired")
				framework.ExpectEqual(isBFDPaired(gwContainer2, addresses.nodeIP), true, "Bfd not paired")

				tcpDumpSync := sync.WaitGroup{}
				checkPingOnContainer := func(container string, wg *sync.WaitGroup) error {
					defer ginkgo.GinkgoRecover()
					defer tcpDumpSync.Done()
					_, err := runCommand("docker", "exec", container, "timeout", "60", "tcpdump", "-c", "1", icmpCommand)
					framework.ExpectNoError(err, "Failed to detect icmp messages on ", container, srcPingPodName)
					framework.Logf("ICMP packet successfully detected on gateway %s", container)
					return nil
				}

				tcpDumpSync.Add(2)
				go checkPingOnContainer(gwContainer1, &tcpDumpSync)
				go checkPingOnContainer(gwContainer2, &tcpDumpSync)

				// Verify the external gateway loopback address running on the external container is reachable and
				// that traffic from the source ping pod is proxied through the pod in the default namespace
				ginkgo.By("Verifying connectivity via the gateway namespace to the remote addresses")

				pingSync := sync.WaitGroup{}
				// spawn a goroutine to asynchronously (to speed up the test)
				// to ping the gateway loopbacks on both containers via ECMP.
				for _, address := range addresses.targetIPs {
					pingSync.Add(1)
					go func(target string) {
						defer ginkgo.GinkgoRecover()
						defer pingSync.Done()
						_, err := framework.RunKubectl(f.Namespace.Name, "exec", srcPingPodName, "--", "ping", "-c", testTimeout, target)
						if err != nil {
							framework.Logf("error generating a ping from the test pod %s: %v", srcPingPodName, err)
						}
					}(address)
				}

				pingSync.Wait()
				tcpDumpSync.Wait()

				ginkgo.By("Deleting one container")
				deleteClusterExternalContainer(gwContainer2)
				time.Sleep(3 * time.Second) // bfd timeout

				tcpDumpSync = sync.WaitGroup{}
				tcpDumpSync.Add(1)
				go checkPingOnContainer(gwContainer1, &tcpDumpSync)

				// Verify the external gateway loopback address running on the external container is reachable and
				// that traffic from the source ping pod is proxied through the pod in the default namespace
				ginkgo.By("Verifying connectivity via the gateway namespace to the remote addresses")
				pingSync = sync.WaitGroup{}

				for _, t := range addresses.targetIPs {
					pingSync.Add(1)
					go func(target string) {
						defer ginkgo.GinkgoRecover()
						defer pingSync.Done()
						_, err := framework.RunKubectl(f.Namespace.Name, "exec", srcPingPodName, "--", "ping", "-c", testTimeout, target)
						framework.ExpectNoError(err, "Failed to ping the remote gateway network from pod", target, srcPingPodName)
					}(t)
				}
				pingSync.Wait()
				tcpDumpSync.Wait()
			},
			ginkgotable.Entry("ipv4", &addressesv4, "icmp"),
			ginkgotable.Entry("ipv6", &addressesv6, "icmp6"))

		ginkgotable.DescribeTable("Should validate TCP/UDP connectivity to an external gateway's loopback address via a pod with external gateway annotations enabled",
			func(protocol string, addresses *gatewayTestIPs, destPort int) {
				if addresses.srcPodIP == "" || addresses.nodeIP == "" {
					skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
				}

				framework.ExpectEqual(isBFDPaired(gwContainer1, addresses.nodeIP), true, "Bfd not paired")
				framework.ExpectEqual(isBFDPaired(gwContainer2, addresses.nodeIP), true, "Bfd not paired")

				expectedHostNames := hostNamesForContainers([]string{gwContainer1, gwContainer2})
				framework.Logf("Expected hostnames are %v", expectedHostNames)

				returnedHostNames := make(map[string]struct{})
				target := addresses.targetIPs[0]
				success := false
				for i := 0; i < 20; i++ {
					hostname := pokeHostnameViaNC(srcPingPodName, f.Namespace.Name, protocol, target, destPort)
					if hostname != "" {
						returnedHostNames[hostname] = struct{}{}
					}

					if cmp.Equal(returnedHostNames, expectedHostNames) {
						success = true
						break
					}
				}
				framework.Logf("Received hostnames for protocol %s are %v ", protocol, returnedHostNames)

				if !success {
					framework.Failf("Failed to hit all the external gateways via for protocol %s, diff %s", protocol, cmp.Diff(expectedHostNames, returnedHostNames))
				}

				ginkgo.By("Deleting one container")
				deleteClusterExternalContainer(gwContainer2)
				ginkgo.By("Waiting for BFD to sync")
				time.Sleep(3 * time.Second) // bfd timeout

				// ECMP should direct all the traffic to the only container
				expectedHostName := hostNameForContainer(gwContainer1)

				ginkgo.By("Checking hostname multiple times")
				for i := 0; i < 20; i++ {
					hostname := pokeHostnameViaNC(srcPingPodName, f.Namespace.Name, protocol, target, destPort)
					framework.ExpectEqual(expectedHostName, hostname, "Hostname returned by nc not as expected")
				}

			},
			ginkgotable.Entry("UDP ipv4", "udp", &addressesv4, externalUDPPort),
			ginkgotable.Entry("TCP ipv4", "tcp", &addressesv4, externalTCPPort),
			ginkgotable.Entry("UDP ipv6", "udp", &addressesv6, externalUDPPort),
			ginkgotable.Entry("TCP ipv6", "tcp", &addressesv6, externalTCPPort))
	})

	// Validate pods can reach a network running in multiple container's loopback
	// addresses via two external gateways running on eth0 of the container without
	// any tunnel encap. This test defines two external gateways and validates ECMP
	// functionality to the container loopbacks. To verify traffic reaches the
	// gateways, tcpdump is running on the external gateways and will exit successfully
	// once an ICMP packet is received from the annotated pod in the k8s cluster.
	// Two additional gateways are added to verify the tcp / udp protocols.
	// They run the netexec command, and the pod asks to return their hostname.
	// The test checks that both hostnames are collected at least once.
	var _ = ginkgo.Describe("e2e multiple external gateway validation", func() {
		const (
			svcname         string = "novxlan-externalgw-ecmp"
			gwContainer1    string = "gw-test-container1"
			gwContainer2    string = "gw-test-container2"
			ciNetworkName   string = "kind"
			testTimeout     string = "30"
			ecmpRetry       int    = 20
			srcPodName             = "e2e-exgw-src-pod"
			externalTCPPort        = 80
			externalUDPPort        = 90
		)

		testContainer := fmt.Sprintf("%s-container", srcPodName)
		testContainerFlag := fmt.Sprintf("--container=%s", testContainer)

		f := framework.NewDefaultFramework(svcname)

		var addressesv4, addressesv6 gatewayTestIPs

		ginkgo.BeforeEach(func() {
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(f.ClientSet, 3)
			framework.ExpectNoError(err)
			if len(nodes.Items) < 3 {
				framework.Failf(
					"Test requires >= 3 Ready nodes, but there are only %v nodes",
					len(nodes.Items))
			}

			setupBFD := setupBFDOnContainer(nodes.Items)
			addressesv4, addressesv6 = setupGatewayContainers(f, nodes, gwContainer1, gwContainer2, srcPodName, externalUDPPort, externalTCPPort, ecmpRetry, setupBFD)

			// remove the routing external annotation
			annotateArgs := []string{
				"annotate",
				"namespace",
				f.Namespace.Name,
				"k8s.ovn.org/routing-external-gws-",
			}
			ginkgo.By("Resetting the gw annotation")
			framework.RunKubectlOrDie(f.Namespace.Name, annotateArgs...)
		})

		ginkgo.AfterEach(func() {
			// tear down the containers simulating the gateways
			deleteClusterExternalContainer(gwContainer1)
			deleteClusterExternalContainer(gwContainer2)
		})

		ginkgotable.DescribeTable("Should validate ICMP connectivity to multiple external gateways for an ECMP scenario", func(addresses *gatewayTestIPs, icmpToDump string) {
			if addresses.srcPodIP == "" || addresses.nodeIP == "" {
				skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
			}

			annotateNamespaceForGateway(f.Namespace.Name, true, addresses.gatewayIPs[0], addresses.gatewayIPs[1])
			// This is needed for bfd to sync up
			time.Sleep(3 * time.Second)
			framework.ExpectEqual(isBFDPaired(gwContainer1, addresses.nodeIP), true, "Bfd not paired")
			framework.ExpectEqual(isBFDPaired(gwContainer2, addresses.nodeIP), true, "Bfd not paired")

			// Verify the gateways and remote loopback addresses are reachable from the pod.
			// Iterate checking connectivity to the loopbacks on the gateways until tcpdump see
			// the traffic or 20 attempts fail. Odds of a false negative here is ~ (1/2)^20
			ginkgo.By("Verifying ecmp connectivity to the external gateways by iterating through the targets")

			// Check for egress traffic to both gateway loopback addresses using tcpdump, since
			// /proc/net/dev counters only record the ingress interface traffic is received on.
			// The test will waits until an ICMP packet is matched on the gateways or fail the
			// test if a packet to the loopback is not received within the timer interval.
			// If an ICMP packet is never detected, return the error via the specified chanel.

			tcpDumpSync := sync.WaitGroup{}
			checkPingOnContainer := func(container string, wg *sync.WaitGroup) error {
				defer ginkgo.GinkgoRecover()
				defer tcpDumpSync.Done()
				_, err := runCommand("docker", "exec", container, "timeout", "60", "tcpdump", "-c", "1", icmpToDump)
				framework.ExpectNoError(err, "Failed to detect icmp messages on ", container, srcPodName)
				framework.Logf("ICMP packet successfully detected on gateway %s", container)
				return nil
			}

			tcpDumpSync.Add(2)
			go checkPingOnContainer(gwContainer1, &tcpDumpSync)
			go checkPingOnContainer(gwContainer2, &tcpDumpSync)

			// spawn a goroutine to asynchronously (to speed up the test)
			// to ping the gateway loopbacks on both containers via ECMP.

			pingSync := sync.WaitGroup{}

			// spawn a goroutine to asynchronously (to speed up the test)
			// to ping the gateway loopbacks on both containers via ECMP.
			for _, address := range addresses.targetIPs {
				pingSync.Add(1)
				go func(target string) {
					defer ginkgo.GinkgoRecover()
					defer pingSync.Done()
					_, err := framework.RunKubectl(f.Namespace.Name, "exec", srcPodName, testContainerFlag, "--", "ping", "-c", testTimeout, target)
					if err != nil {
						framework.Logf("error generating a ping from the test pod %s: %v", srcPodName, err)
					}
				}(address)
			}

			pingSync.Wait()
			tcpDumpSync.Wait()

			ginkgo.By("Deleting one container")
			deleteClusterExternalContainer(gwContainer2)
			time.Sleep(3 * time.Second) // bfd timeout

			pingSync = sync.WaitGroup{}
			tcpDumpSync = sync.WaitGroup{}

			tcpDumpSync.Add(1)
			go checkPingOnContainer(gwContainer1, &tcpDumpSync)

			// spawn a goroutine to asynchronously (to speed up the test)
			// to ping the gateway loopbacks on both containers via ECMP.
			for _, address := range addresses.targetIPs {
				pingSync.Add(1)
				go func(target string) {
					defer ginkgo.GinkgoRecover()
					defer pingSync.Done()
					_, err := framework.RunKubectl(f.Namespace.Name, "exec", srcPodName, testContainerFlag, "--", "ping", "-c", testTimeout, target)
					if err != nil {
						framework.Logf("error generating a ping from the test pod %s: %v", srcPodName, err)
					}
				}(address)
			}

			pingSync.Wait()
			tcpDumpSync.Wait()
		}, ginkgotable.Entry("IPV4", &addressesv4, "icmp"),
			ginkgotable.Entry("IPV6", &addressesv6, "icmp6"))

		// This test runs a listener on the external container, returning the host name both on tcp and udp.
		// The src pod tries to hit the remote address until both the containers are hit.
		ginkgotable.DescribeTable("Should validate TCP/UDP connectivity to multiple external gateways for a UDP / TCP scenario", func(addresses *gatewayTestIPs, protocol string, destPort int) {
			if addresses.srcPodIP == "" || addresses.nodeIP == "" {
				skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
			}

			annotateNamespaceForGateway(f.Namespace.Name, true, addresses.gatewayIPs[0], addresses.gatewayIPs[1])
			// This is needed for bfd to sync up
			time.Sleep(3 * time.Second)
			framework.ExpectEqual(isBFDPaired(gwContainer1, addresses.nodeIP), true, "Bfd not paired")
			framework.ExpectEqual(isBFDPaired(gwContainer2, addresses.nodeIP), true, "Bfd not paired")

			expectedHostNames := hostNamesForContainers([]string{gwContainer1, gwContainer2})
			framework.Logf("Expected hostnames are %v", expectedHostNames)

			returnedHostNames := make(map[string]struct{})
			success := false

			// Picking only the first address, the one the udp listener is set for
			target := addresses.targetIPs[0]
			for i := 0; i < 20; i++ {
				hostname := pokeHostnameViaNC(srcPodName, f.Namespace.Name, protocol, target, destPort)
				if hostname != "" {
					returnedHostNames[hostname] = struct{}{}
				}
				if cmp.Equal(returnedHostNames, expectedHostNames) {
					success = true
					break
				}
			}

			framework.Logf("Received hostnames for protocol %s are %v ", protocol, returnedHostNames)

			if !success {
				framework.Failf("Failed to hit all the external gateways via for protocol %s, diff %s", protocol, cmp.Diff(expectedHostNames, returnedHostNames))
			}

			ginkgo.By("Deleting one container")
			deleteClusterExternalContainer(gwContainer2)
			ginkgo.By("Waiting for BFD to sync")
			time.Sleep(3 * time.Second) // bfd timeout

			// ECMP should direct all the traffic to the only container
			expectedHostName := hostNameForContainer(gwContainer1)

			ginkgo.By("Checking hostname multiple times")
			for i := 0; i < 20; i++ {
				hostname := pokeHostnameViaNC(srcPodName, f.Namespace.Name, protocol, target, destPort)
				framework.ExpectEqual(expectedHostName, hostname, "Hostname returned by nc not as expected")
			}

		}, ginkgotable.Entry("IPV4 udp", &addressesv4, "udp", externalUDPPort),
			ginkgotable.Entry("IPV4 tcp", &addressesv4, "tcp", externalTCPPort),
			ginkgotable.Entry("IPV6 udp", &addressesv6, "udp", externalUDPPort),
			ginkgotable.Entry("IPV6 tcp", &addressesv6, "tcp", externalTCPPort))
	})
})

// setupGatewayContainers sets up external containers, adds routes to the nodes, sets up udp / tcp listeners
// that return the container's hostname.
// All its needed for namespace / pod gateway tests.
func setupGatewayContainers(f *framework.Framework, nodes *v1.NodeList, gwContainer1, gwContainer2, srcPodName string, updPort, tcpPort, numOfIPs int, postCreations ...func(string)) (gatewayTestIPs, gatewayTestIPs) {
	addressesv4 := gatewayTestIPs{targetIPs: make([]string, 0)}
	addressesv6 := gatewayTestIPs{targetIPs: make([]string, 0)}

	ginkgo.By("Creating the gateway containers for the icmp test")
	addressesv4.gatewayIPs[0], addressesv6.gatewayIPs[0] = createClusterExternalContainer(gwContainer1, externalContainerImage, []string{"-itd", "--privileged", "--network", ciNetworkName}, []string{})
	addressesv4.gatewayIPs[1], addressesv6.gatewayIPs[1] = createClusterExternalContainer(gwContainer2, externalContainerImage, []string{"-itd", "--privileged", "--network", ciNetworkName}, []string{})

	// Set up the destination ips to reach via the gw
	for lastOctet := 1; lastOctet <= numOfIPs; lastOctet++ {
		destIP := fmt.Sprintf("10.249.10.%d", lastOctet)
		addressesv4.targetIPs = append(addressesv4.targetIPs, destIP)
	}
	for lastGroup := 1; lastGroup <= numOfIPs; lastGroup++ {
		destIP := fmt.Sprintf("fc00:f853:ccd:e794::%d", lastGroup)
		addressesv6.targetIPs = append(addressesv6.targetIPs, destIP)
	}
	framework.Logf("target ips are %v", addressesv4.targetIPs)
	framework.Logf("target ipsv6 are %v", addressesv6.targetIPs)

	node := nodes.Items[0]
	addressesv4.nodeIP, addressesv6.nodeIP = getNodeAddresses(&node)
	framework.Logf("the pod side node is %s and the source node ip is %s - %s", node.Name, addressesv4.nodeIP, addressesv6.nodeIP)

	ginkgo.By("Creating the source pod to reach the destination ips from")

	args := []string{
		"netexec",
		fmt.Sprintf("--http-port=%d", srcHTTPPort),
		fmt.Sprintf("--udp-port=%d", srcUDPPort),
	}
	clientPod, err := createPod(f, srcPodName, node.Name, f.Namespace.Name, []string{}, map[string]string{}, func(p *v1.Pod) {
		p.Spec.Containers[0].Args = args
	})

	framework.ExpectNoError(err)

	addressesv4.srcPodIP, addressesv6.srcPodIP = getPodAddresses(clientPod)
	framework.Logf("the pod source pod ip(s) are %s - %s", addressesv4.srcPodIP, addressesv6.srcPodIP)

	testIPv6 := false
	testIPv4 := false

	if addressesv6.srcPodIP != "" && addressesv6.nodeIP != "" {
		testIPv6 = true
	}
	if addressesv4.srcPodIP != "" && addressesv4.nodeIP != "" {
		testIPv4 = true
	}
	if !testIPv4 && !testIPv6 {
		framework.Fail("No ipv4 nor ipv6 addresses found in nodes and src pod")
	}

	// This sets up a listener that replies with the hostname, both on tcp and on udp
	setupListenersOrDie := func(container, address string) {
		cmd := []string{"docker", "exec", container, "bash", "-c", fmt.Sprintf("while true; do echo $(hostname) | nc -l -u %s %d; done &", address, updPort)}
		_, err = runCommand(cmd...)
		framework.ExpectNoError(err, "failed to setup listener on ", address, container)

		cmd = []string{"docker", "exec", container, "bash", "-c", fmt.Sprintf("while true; do echo $(hostname) | nc -l %s %d; done &", address, tcpPort)}
		_, err = runCommand(cmd...)
		framework.ExpectNoError(err, "failed to setup listener on ", address, container)
	}

	// The target ips are addresses added to the lo of each container.
	// By setting the gateway annotation and using them as destination, we verify that
	// the routing is able to reach the containers.
	// A route back to the src pod must be set in order for the ping reply to work.
	for _, containerName := range []string{gwContainer1, gwContainer2} {
		if testIPv4 {
			ginkgo.By(fmt.Sprintf("Setting up the destination ips to %s", containerName))
			for _, address := range addressesv4.targetIPs {
				_, err = runCommand("docker", "exec", containerName, "ip", "address", "add", address+"/32", "dev", "lo")
				framework.ExpectNoError(err, "failed to add the loopback ip to dev lo on the test container %s", containerName)
			}

			ginkgo.By(fmt.Sprintf("Adding a route from %s to the src pod", containerName))
			_, err = runCommand("docker", "exec", containerName, "ip", "route", "add", addressesv4.srcPodIP, "via", addressesv4.nodeIP)
			framework.ExpectNoError(err, "failed to add the pod host route on the test container %s", containerName)

			ginkgo.By("Setting up the listeners on the gateway")
			setupListenersOrDie(containerName, addressesv4.targetIPs[0])
		}
		if testIPv6 {
			ginkgo.By(fmt.Sprintf("Setting up the destination ips to %s (ipv6)", containerName))
			for _, address := range addressesv6.targetIPs {
				_, err = runCommand("docker", "exec", containerName, "ip", "address", "add", address+"/128", "dev", "lo")
				framework.ExpectNoError(err, "ipv6: failed to add the loopback ip to dev lo on the test container %s", containerName)
			}
			ginkgo.By(fmt.Sprintf("Adding a route from %s to the src pod (ipv6)", containerName))
			_, err = runCommand("docker", "exec", containerName, "ip", "-6", "route", "add", addressesv6.srcPodIP, "via", addressesv6.nodeIP)
			framework.ExpectNoError(err, "ipv6: failed to add the pod host route on the test container %s", containerName)

			ginkgo.By("Setting up the listeners on the gateway (v6)")
			setupListenersOrDie(containerName, addressesv6.targetIPs[0])
		}
	}

	for _, containerName := range []string{gwContainer1, gwContainer2} {
		for _, postCreation := range postCreations {
			postCreation(containerName)
		}
	}
	return addressesv4, addressesv6
}

// setupGatewayContainersForConntrackTest sets up iperf3 external containers, adds routes to src
// pods via the nodes, starts up iperf3 server on src-pod
func setupGatewayContainersForConntrackTest(f *framework.Framework, nodes *v1.NodeList, gwContainer1, gwContainer2, srcPodName string) (gatewayTestIPs, gatewayTestIPs) {
	addressesv4 := gatewayTestIPs{}
	addressesv6 := gatewayTestIPs{}

	ginkgo.By("Creating the gateway containers for the UDP test")
	addressesv4.gatewayIPs[0], addressesv6.gatewayIPs[0] = createClusterExternalContainer(gwContainer1, iperf3Image, []string{"-itd", "--privileged", "--network", externalContainerNetwork}, []string{})
	addressesv4.gatewayIPs[1], addressesv6.gatewayIPs[1] = createClusterExternalContainer(gwContainer2, iperf3Image, []string{"-itd", "--privileged", "--network", externalContainerNetwork}, []string{})

	node := nodes.Items[0]
	addressesv4.nodeIP, addressesv6.nodeIP = getContainerAddressesForNetwork(node.Name, externalContainerNetwork)
	framework.Logf("the pod side node is %s and the source node ip is %s - %s", node.Name, addressesv4.nodeIP, addressesv6.nodeIP)

	ginkgo.By("Creating the source pod to reach the destination ips from")
	clientPod, err := createPod(f, srcPodName, node.Name, f.Namespace.Name, []string{}, map[string]string{}, func(p *v1.Pod) {
		p.Spec.Containers[0].Image = iperf3Image
	})
	framework.ExpectNoError(err)

	// start iperf3 servers at ports 5201 and 5202 on the src app pod
	args := []string{"exec", srcPodName, "--", "iperf3", "-s", "--daemon", "-V", fmt.Sprintf("-p %d", 5201)}
	_, err = framework.RunKubectl(f.Namespace.Name, args...)
	framework.ExpectNoError(err, "failed to start iperf3 server on pod %s at port 5201", srcPodName)

	args = []string{"exec", srcPodName, "--", "iperf3", "-s", "--daemon", "-V", fmt.Sprintf("-p %d", 5202)}
	_, err = framework.RunKubectl(f.Namespace.Name, args...)
	framework.ExpectNoError(err, "failed to start iperf3 server on pod %s at port 5202", srcPodName)

	addressesv4.srcPodIP, addressesv6.srcPodIP = getPodAddresses(clientPod)
	framework.Logf("the pod source pod ip(s) are %s - %s", addressesv4.srcPodIP, addressesv6.srcPodIP)

	testIPv6 := false
	testIPv4 := false

	if addressesv6.srcPodIP != "" && addressesv6.nodeIP != "" {
		testIPv6 = true
	}
	if addressesv4.srcPodIP != "" && addressesv4.nodeIP != "" {
		testIPv4 = true
	}
	if !testIPv4 && !testIPv6 {
		framework.Fail("No ipv4 nor ipv6 addresses found in nodes and src pod")
	}

	// A route back to the src pod must be set in order for the ping reply to work.
	for _, containerName := range []string{gwContainer1, gwContainer2} {
		ginkgo.By(fmt.Sprintf("Install iproute in %s", containerName))
		_, err = runCommand("docker", "exec", containerName, "dnf", "install", "-y", "iproute")
		framework.ExpectNoError(err, "failed to install iproute package on the test container %s", containerName)
		if testIPv4 {
			ginkgo.By(fmt.Sprintf("Adding a route from %s to the src pod with IP %s", containerName, addressesv4.srcPodIP))
			_, err = runCommand("docker", "exec", containerName, "ip", "route", "add", addressesv4.srcPodIP, "via", addressesv4.nodeIP, "dev", "eth0")
			framework.ExpectNoError(err, "failed to add the pod host route on the test container %s", containerName)
		}
		if testIPv6 {
			ginkgo.By(fmt.Sprintf("Adding a route from %s to the src pod (ipv6)", containerName))
			_, err = runCommand("docker", "exec", containerName, "ip", "-6", "route", "add", addressesv6.srcPodIP, "via", addressesv6.nodeIP)
			framework.ExpectNoError(err, "ipv6: failed to add the pod host route on the test container %s", containerName)
		}
	}
	return addressesv4, addressesv6
}

func reachPodFromContainer(targetAddress, targetPort, targetPodName, srcContainer, protocol string) {
	ginkgo.By(fmt.Sprintf("Checking that %s can reach the pod", srcContainer))
	dockerCmd := []string{"docker", "exec", srcContainer, "bash", "-c"}
	if protocol == "tcp" {
		dockerCmd = append(dockerCmd, fmt.Sprintf("curl -s http://%s/hostname", net.JoinHostPort(targetAddress, targetPort)))
	} else {
		dockerCmd = append(dockerCmd, fmt.Sprintf("cat <(echo hostname) <(sleep 1) | nc -u %s %s", targetAddress, targetPort))
	}

	res, err := runCommand(dockerCmd...)
	framework.ExpectNoError(err, "Failed to reach pod from external container ", targetAddress, srcContainer, protocol)
	framework.ExpectEqual(strings.Trim(res, "\n"), targetPodName)
}

func annotatePodForGateway(podName, namespace, networkIPs string, bfd bool) {
	// add the annotations to the pod to enable the gateway forwarding.
	// this fakes out the multus annotation so that the pod IP is
	// actually an IP of an external container for testing purposes
	annotateArgs := []string{
		"annotate",
		"pods",
		podName,
		fmt.Sprintf("k8s.v1.cni.cncf.io/network-status=[{\"name\":\"%s\",\"interface\":"+
			"\"net1\",\"ips\":[%s],\"mac\":\"%s\"}]", "foo", networkIPs, "01:23:45:67:89:10"),
		fmt.Sprintf("k8s.ovn.org/routing-namespaces=%s", namespace),
		fmt.Sprintf("k8s.ovn.org/routing-network=%s", "foo"),
		"--overwrite",
	}
	if bfd {
		annotateArgs = append(annotateArgs, "k8s.ovn.org/bfd-enabled=\"\"")
	}
	framework.Logf("Annotating the external gateway pod with annotation %s", annotateArgs)
	framework.RunKubectlOrDie("default", annotateArgs...)
}

func annotateNamespaceForGateway(namespace string, bfd bool, gateways ...string) {

	externalGateways := strings.Join(gateways, ",")
	// annotate the test namespace with multiple gateways defined
	annotateArgs := []string{
		"annotate",
		"namespace",
		namespace,
		fmt.Sprintf("k8s.ovn.org/routing-external-gws=%s", externalGateways),
		"--overwrite",
	}
	if bfd {
		annotateArgs = append(annotateArgs, "k8s.ovn.org/bfd-enabled=\"\"")
	}
	framework.Logf("Annotating the external gateway test namespace to container gateways: %s", externalGateways)
	framework.RunKubectlOrDie(namespace, annotateArgs...)
}

func hostNamesForContainers(containers []string) map[string]struct{} {
	res := make(map[string]struct{})
	for _, c := range containers {
		hostName := hostNameForContainer(c)
		res[hostName] = struct{}{}
	}
	return res
}

func hostNameForContainer(container string) string {
	res, err := runCommand("docker", "exec", container, "hostname")
	framework.ExpectNoError(err, "failed to run hostname on", container)
	framework.Logf("Hostname for %s is %s", container, res)
	return strings.TrimSuffix(res, "\n")
}

func pokeHostnameViaNC(podName, namespace, protocol, target string, port int) string {
	args := []string{"exec", podName, "--"}
	if protocol == "tcp" {
		args = append(args, "bash", "-c", fmt.Sprintf("echo | nc -w 1 %s %d", target, port))
	} else {
		args = append(args, "bash", "-c", fmt.Sprintf("echo | nc -w 1 -u %s %d", target, port))
	}
	res, err := framework.RunKubectl(namespace, args...)
	framework.ExpectNoError(err, "failed to reach ", target, protocol)
	hostname := strings.TrimSuffix(res, "\n")
	return hostname
}

// pokeConntrackEntries returns the number of conntrack entries that match the provided pattern, protocol and podIP
func pokeConntrackEntries(nodeName, podIP, protocol string, patterns []string) int {
	args := []string{"get", "pods", "--selector=app=ovs-node", "--field-selector", fmt.Sprintf("spec.nodeName=%s", nodeName), "-o", "jsonpath={.items..metadata.name}"}
	ovsPodName, err := framework.RunKubectl("ovn-kubernetes", args...)
	framework.ExpectNoError(err, "failed to get the ovs pod on node %s", nodeName)
	args = []string{"exec", ovsPodName, "--", "ovs-dpctl", "dump-conntrack"}
	conntrackEntries, err := framework.RunKubectl("ovn-kubernetes", args...)
	framework.ExpectNoError(err, "failed to get the conntrack entries from node %s", nodeName)
	numOfConnEntries := 0
	for _, connEntry := range strings.Split(conntrackEntries, "\n") {
		match := strings.Contains(connEntry, protocol) && strings.Contains(connEntry, podIP)
		for _, pattern := range patterns {
			if strings.Contains(connEntry, pattern) && match {
				numOfConnEntries++
			}
		}
		if len(patterns) == 0 && match {
			numOfConnEntries++
		}
	}

	return numOfConnEntries
}

func setupBFDOnContainer(nodes []v1.Node) func(string) {
	return func(containerName string) {
		// we set a bfd peer for each address of each node
		for _, node := range nodes {
			ipv4, ipv6 := getNodeAddresses(&node)
			for _, a := range []string{ipv4, ipv6} {
				if a == "" {
					continue
				}
				// Configure the node as a bfd peer on the frr side
				cmd := []string{"docker", "exec", containerName, "bash", "-c",
					fmt.Sprintf(`cat << EOF >> /etc/frr/frr.conf

bfd
 peer %s
   no shutdown
 !
!
EOF
`, a)}
				_, err := runCommand(cmd...)
				framework.ExpectNoError(err, "failed to setup listener on ", "", containerName)
			}
		}
		cmd := []string{"docker", "exec", containerName, "/usr/lib/frr/frrinit.sh", "start"}
		_, err := runCommand(cmd...)
		framework.ExpectNoError(err, "failed to start frr on", containerName)
	}
}

func isBFDPaired(container, peer string) bool {
	res, err := runCommand("docker", "exec", container, "bash", "-c", fmt.Sprintf("vtysh -c \"show bfd peer %s\"", peer))
	framework.ExpectNoError(err, "failed to check bfd status on", container)
	if strings.Contains(res, "Status: up") {
		return true
	}
	return false
}
