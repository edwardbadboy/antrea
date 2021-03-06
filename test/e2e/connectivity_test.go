// Copyright 2019 Antrea Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vmware-tanzu/antrea/pkg/agent/config"
)

const pingCount = 5

func waitForPodIPs(t *testing.T, data *TestData, podNames []string) map[string]string {
	t.Logf("Waiting for Pods to be ready and retrieving IPs")
	podIPs := make(map[string]string)
	for _, podName := range podNames {
		if podIP, err := data.podWaitForIP(defaultTimeout, podName); err != nil {
			t.Fatalf("Error when waiting for IP for Pod '%s': %v", podName, err)
		} else {
			podIPs[podName] = podIP
		}
	}
	t.Logf("Retrieved all Pod IPs: %v", podIPs)
	return podIPs
}

// runPingMesh runs a ping mesh between all the provided Pods after first retrieveing their IP
// addresses.
func (data *TestData) runPingMesh(t *testing.T, podNames []string) {
	podIPs := waitForPodIPs(t, data, podNames)

	t.Logf("Ping mesh test between all Pods")
	for _, podName1 := range podNames {
		for _, podName2 := range podNames {
			if podName1 == podName2 {
				continue
			}
			if err := data.runPingCommandFromTestPod(podName1, podIPs[podName2], pingCount); err != nil {
				t.Errorf("Ping '%s' -> '%s': ERROR (%v)", podName1, podName2, err)
			} else {
				t.Logf("Ping '%s' -> '%s': OK", podName1, podName2)
			}
		}
	}
}

// TestPodConnectivitySameNode checks that Pods running on the same Node can reach each other, by
// creating multiple Pods on the same Node and having them ping each other.
func TestPodConnectivitySameNode(t *testing.T) {
	data, err := setupTest(t)
	if err != nil {
		t.Fatalf("Error when setting up test: %v", err)
	}
	defer teardownTest(t, data)

	numPods := 2 // can be increased
	podNames := make([]string, numPods)
	for idx := range podNames {
		podNames[idx] = randName(fmt.Sprintf("test-pod-%d-", idx))
	}
	workerNode := workerNodeName(1)

	t.Logf("Creating two busybox test Pods on '%s'", workerNode)
	for _, podName := range podNames {
		if err := data.createBusyboxPodOnNode(podName, workerNode); err != nil {
			t.Fatalf("Error when creating busybox test Pod: %v", err)
		}
		defer deletePodWrapper(t, data, podName)
	}

	data.runPingMesh(t, podNames)
}

// createPodsOnDifferentNodes creates numPods busybox test Pods and assign them to all the different
// Nodes in round-robin fashion, then returns the names of the created Pods as well as a function
// which will delete the Pods when called.
func createPodsOnDifferentNodes(t *testing.T, data *TestData, numPods int) (podNames []string, cleanup func()) {
	podNames = make([]string, 0, numPods)

	cleanup = func() {
		for _, podName := range podNames {
			deletePodWrapper(t, data, podName)
		}
	}

	for idx := 0; idx < numPods; idx++ {
		podName := randName(fmt.Sprintf("test-pod-%d-", idx))
		nodeName := nodeName(idx % clusterInfo.numNodes)
		t.Logf("Creating busybox test Pods '%s' on '%s'", podName, nodeName)
		if err := data.createBusyboxPodOnNode(podName, nodeName); err != nil {
			cleanup()
			t.Fatalf("Error when creating busybox test Pod: %v", err)
		}
		podNames = append(podNames, podName)
	}

	return podNames, cleanup
}

func (data *TestData) testPodConnectivityDifferentNodes(t *testing.T) {
	numPods := 2
	encapMode, err := data.GetEncapMode()
	if err != nil {
		t.Errorf("Failed to retrieve encap mode: %v", err)
	}
	if encapMode == config.TrafficEncapModeHybrid {
		// To adequately test hybrid traffic across and within
		// subnet, all Nodes should have a Pod.
		numPods = clusterInfo.numNodes
	}
	podNames, deletePods := createPodsOnDifferentNodes(t, data, numPods)
	defer deletePods()

	data.runPingMesh(t, podNames)
}

// TestPodConnectivityDifferentNodes checks that Pods running on different Nodes can reach each
// other, by creating multiple Pods across distinct Nodes and having them ping each other.
func TestPodConnectivityDifferentNodes(t *testing.T) {
	skipIfNumNodesLessThan(t, 2)
	data, err := setupTest(t)
	if err != nil {
		t.Fatalf("Error when setting up test: %v", err)
	}
	defer teardownTest(t, data)

	data.testPodConnectivityDifferentNodes(t)
}

func (data *TestData) redeployAntrea(t *testing.T, enableIPSec bool) {
	var err error

	t.Logf("Deleting Antrea Agent DaemonSet")
	if err = data.deleteAntrea(defaultTimeout); err != nil {
		t.Fatalf("Error when deleting Antrea DaemonSet: %v", err)
	}

	t.Logf("Applying Antrea YAML")
	if enableIPSec {
		err = data.deployAntreaIPSec()
	} else {
		err = data.deployAntrea()
	}
	if err != nil {
		t.Fatalf("Error when applying Antrea YAML: %v", err)
	}

	t.Logf("Waiting for all Antrea DaemonSet Pods")
	if err := data.waitForAntreaDaemonSetPods(defaultTimeout); err != nil {
		t.Fatalf("Error when restarting Antrea: %v", err)
	}
	t.Logf("Checking CoreDNS deployment")
	if err := data.checkCoreDNSPods(defaultTimeout); err != nil {
		t.Fatalf("Error when checking CoreDNS deployment: %v", err)
	}
}

// TestPodConnectivityAfterAntreaRestart checks that restarting antrea-agent does not create
// connectivity issues between Pods.
func TestPodConnectivityAfterAntreaRestart(t *testing.T) {
	// See https://github.com/vmware-tanzu/antrea/issues/244
	skipIfProviderIs(t, "kind", "test may cause subsequent tests to fail in Kind clusters")
	data, err := setupTest(t)
	if err != nil {
		t.Fatalf("Error when setting up test: %v", err)
	}
	defer teardownTest(t, data)

	numPods := 2 // can be increased
	podNames, deletePods := createPodsOnDifferentNodes(t, data, numPods)
	defer deletePods()

	data.runPingMesh(t, podNames)

	data.redeployAntrea(t, false)

	data.runPingMesh(t, podNames)
}

// TestOVSRestartSameNode verifies that datapath flows are not removed when the Antrea Agent Pod is
// stopped gracefully (e.g. as part of a RollingUpdate). The test sends ARP requests every 1s and
// checks that there is no packet loss during the restart. This test does not apply to the userspace
// ndetdev datapath, since in this case the datapath functionality is implemented by the
// ovs-vswitchd daemon itself. When ovs-vswitchd restarts, datapath flows are flushed and it may
// take some time for the Agent to replay the flows. This will not impact this test, since we are
// just testing L2 connectivity betwwen 2 Pods on the same Node, and the default behavior of the
// br-int bridge is to implement normal L2 forwarding.
func TestOVSRestartSameNode(t *testing.T) {
	skipIfProviderIs(t, "kind", "test not valid for the netdev datapath type")
	data, err := setupTest(t)
	if err != nil {
		t.Fatalf("Error when setting up test: %v", err)
	}
	defer teardownTest(t, data)

	workerNode := workerNodeName(1)
	t.Logf("Creating two busybox test Pods on '%s'", workerNode)
	podNames, podIPs, cleanupFn := createTestBusyboxPods(t, data, 2, workerNode)
	defer cleanupFn()

	resCh := make(chan error, 1)

	runArping := func() error {
		// we send arp pings for 25 seconds; this duration is a bit arbitrary and we assume
		// that restarting Antrea takes less than that time. Unfortunately, the arping
		// utility in busybox does not let us choose a smaller interval than 1 second.
		count := 25
		cmd := fmt.Sprintf("arping -c %d %s", count, podIPs[1])
		stdout, stderr, err := data.runCommandFromPod(testNamespace, podNames[0], busyboxContainerName, strings.Fields(cmd))
		if err != nil {
			return fmt.Errorf("error when running arping command: %v - stdout: %s - stderr: %s", err, stdout, stderr)
		}
		// if the datapath flows have been flushed, there will be some unanswered ARP
		// requests.
		_, _, lossRate, err := parseArpingStdout(stdout)
		if err != nil {
			return err
		}
		t.Logf("Arping loss rate: %f%%", lossRate)
		if lossRate > 0 {
			t.Logf(stdout)
			return fmt.Errorf("arping loss rate is %f%%", lossRate)
		}
		return nil
	}
	go func() {
		resCh <- runArping()
	}()
	// make sure that by the time we delete the Antrea agent, at least one unicast ARP has been
	// sent (and cached in the OVS kernel datapath).
	time.Sleep(3 * time.Second)

	t.Logf("Restarting antrea-agent on Node '%s'", workerNode)
	if _, err := data.deleteAntreaAgentOnNode(workerNode, 30 /* grace period in seconds */, defaultTimeout); err != nil {
		t.Fatalf("Error when restarting antrea-agent on Node '%s': %v", workerNode, err)
	}

	if err := <-resCh; err != nil {
		t.Errorf("Arping test failed: %v", err)
	}
}

// TestOVSFlowReplay checks that when OVS restarts unexpectedly the Antrea agent takes care of
// replaying flows. More precisely this test checks that Pod connectivity still works after deleting
// the flows and force-restarting the OVS dameons.
func TestOVSFlowReplay(t *testing.T) {
	skipIfProviderIs(t, "kind", "stopping OVS daemons create connectivity issues")
	data, err := setupTest(t)
	if err != nil {
		t.Fatalf("Error when setting up test: %v", err)
	}
	defer teardownTest(t, data)

	numPods := 2
	podNames := make([]string, numPods)
	for idx := range podNames {
		podNames[idx] = randName(fmt.Sprintf("test-pod-%d-", idx))
	}
	workerNode := workerNodeName(1)

	t.Logf("Creating two busybox test Pods on '%s'", workerNode)
	for _, podName := range podNames {
		if err := data.createBusyboxPodOnNode(podName, workerNode); err != nil {
			t.Fatalf("Error when creating busybox test Pod: %v", err)
		}
		defer deletePodWrapper(t, data, podName)
	}

	data.runPingMesh(t, podNames)

	var antreaPodName string
	if antreaPodName, err = data.getAntreaPodOnNode(workerNode); err != nil {
		t.Fatalf("Error when retrieving the name of the Antrea Pod running on Node '%s': %v", workerNode, err)
	}
	t.Logf("The Antrea Pod for Node '%s' is '%s'", workerNode, antreaPodName)

	t.Logf("Deleting flows and restarting OVS daemons on Node '%s'", workerNode)
	delFlows := func() {
		cmd := []string{"ovs-ofctl", "del-flows", defaultBridgeName}
		_, stderr, err := data.runCommandFromPod(antreaNamespace, antreaPodName, ovsContainerName, cmd)
		if err != nil {
			t.Fatalf("error when deleting flows: <%v>, err: <%v>", stderr, err)
		}
	}
	delFlows()
	restartCmd := []string{"/usr/share/openvswitch/scripts/ovs-ctl", "--system-id=random", "restart", "--db-file=/var/run/openvswitch/conf.db"}
	if stdout, stderr, err := data.runCommandFromPod(antreaNamespace, antreaPodName, ovsContainerName, restartCmd); err != nil {
		t.Fatalf("Error when restarting OVS with ovs-ctl: %v - stdout: %s - stderr: %s", err, stdout, stderr)
	}

	// This should give Antrea ~10s to restore flows, since we generate 10 "pings" with a 1s
	// interval.
	t.Logf("Running second ping mesh to check that flows have been restored")
	data.runPingMesh(t, podNames)
}

// TestPingLargeMTU verifies that fragmented ICMP packets are handled correctly. Until OVS 2.12.0,
// the conntrack implementation of the OVS userspace datapath did not support v4/v6 fragmentation
// and this test was failing when Antrea was running on a Kind cluster.
func TestPingLargeMTU(t *testing.T) {
	skipIfNumNodesLessThan(t, 2)
	data, err := setupTest(t)
	if err != nil {
		t.Fatalf("Error when setting up test: %v", err)
	}
	defer teardownTest(t, data)

	podNames, deletePods := createPodsOnDifferentNodes(t, data, 2)
	defer deletePods()
	podName0 := podNames[0]
	podName1 := podNames[1]
	podIPs := waitForPodIPs(t, data, podNames)

	pingSize := 2000
	cmd := fmt.Sprintf("ping -c %d -s %d %s", pingCount, pingSize, podIPs[podName1])
	t.Logf("Running ping with size %d between Pods %s and %s", pingSize, podName0, podName1)
	stdout, stderr, err := data.runCommandFromPod(testNamespace, podName0, busyboxContainerName, strings.Fields(cmd))
	if err != nil {
		t.Errorf("Error when running ping command: %v - stdout: %s - stderr: %s", err, stdout, stderr)
	}
}
