// Copyright 2019 OKN Authors
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
	"bytes"
	"fmt"
	"math/rand"
	"net"
	"regexp"
	"strconv"
	"time"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"

	"okn/test/e2e/providers"
)

const defaultTimeout time.Duration = 90 * time.Second

const OKNDaemonSet string = "okn-agent"

const testNamespace string = "okn-test"

const defaultContainerName string = "busybox"

const podNameSuffixLength int = 8

const OVSContainerName string = "okn-ovs"

// OKNNamespace is the K8s Namespace in which all OKN resources are running.
const OKNNamespace string = "kube-system"

type ClusterNode struct {
	idx  int // 0 for master Node
	name string
}

type ClusterInfo struct {
	numWorkerNodes int
	numNodes       int
	podNetworkCIDR string
	masterNodeName string
	nodes          map[int]ClusterNode
}

var clusterInfo ClusterInfo

type TestOptions struct {
	providerName        string
	providerConfigPath  string
	logsExportDir       string
	logsExportOnSuccess bool
}

var testOptions TestOptions

var provider providers.ProviderInterface

// TestData stores the state required for each test case.
type TestData struct {
	kubeConfig *restclient.Config
	clientset  kubernetes.Interface
}

// workerNodeName returns an empty string if there is no worker Node with the provided idx
// (including if idx is 0, which is reserved for the master Node)
func workerNodeName(idx int) string {
	if idx == 0 { // master Node
		return ""
	}
	if node, ok := clusterInfo.nodes[idx]; !ok {
		return ""
	} else {
		return node.name
	}
}

func masterNodeName() string {
	return clusterInfo.masterNodeName
}

// nodeName returns an empty string if there is no Node with the provided idx. If idx is 0, the name
// of the master Node will be returned.
func nodeName(idx int) string {
	if node, ok := clusterInfo.nodes[idx]; !ok {
		return ""
	} else {
		return node.name
	}
}

func initProvider() error {
	providerFactory := map[string]func(string) (providers.ProviderInterface, error){
		"vagrant": providers.NewVagrantProvider,
	}
	if fn, ok := providerFactory[testOptions.providerName]; ok {
		if newProvider, err := fn(testOptions.providerConfigPath); err != nil {
			return err
		} else {
			provider = newProvider
		}
	} else {
		return fmt.Errorf("unknown provider '%s'", testOptions.providerName)
	}
	return nil
}

// A convenience wrapper around RunSSHCommand which runs the provided command on the node with name
// nodeName.
func RunSSHCommandOnNode(nodeName string, cmd string) (code int, stdout string, stderr string, err error) {
	host, config, err := provider.GetSSHConfig(nodeName)
	if err != nil {
		return 0, "", "", fmt.Errorf("error when retrieving SSH config for node '%s': %v", nodeName, err)
	}
	return RunSSHCommand(host, config, cmd)
}

func collectClusterInfo() error {
	// first create client set
	testData := &TestData{}
	if err := testData.createClient(); err != nil {
		return err
	}

	// retrieve Node information
	nodes, err := testData.clientset.CoreV1().Nodes().List(metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error when listing cluster Nodes: %v", err)
	}
	workerIdx := 1
	clusterInfo.nodes = make(map[int]ClusterNode)
	for _, node := range nodes.Items {
		isMaster := func() bool {
			_, ok := node.Labels["node-role.kubernetes.io/master"]
			return ok
		}()

		var nodeIdx int
		// If multiple master Nodes (HA), we will select the last one in the list
		if isMaster {
			nodeIdx = 0
			clusterInfo.masterNodeName = node.Name
		} else {
			nodeIdx = workerIdx
			workerIdx += 1
		}

		clusterInfo.nodes[nodeIdx] = ClusterNode{
			idx:  nodeIdx,
			name: node.Name,
		}
	}
	if clusterInfo.masterNodeName == "" {
		return fmt.Errorf("error when listing cluster Nodes: master Node not found")
	}
	clusterInfo.numNodes = workerIdx
	clusterInfo.numWorkerNodes = clusterInfo.numNodes - 1

	// retrieve cluster CIDR
	if err := func() error {
		cmd := "kubectl cluster-info dump | grep cluster-cidr"
		rc, stdout, _, err := RunSSHCommandOnNode(clusterInfo.masterNodeName, cmd)
		if err != nil || rc != 0 {
			return fmt.Errorf("error when running the following command on master Node: %s", cmd)
		}
		re := regexp.MustCompile(`cluster-cidr=([^"]+)`)
		if matches := re.FindStringSubmatch(stdout); len(matches) == 0 {
			return fmt.Errorf("cannot retrieve cluster CIDR, unexpected kubectl output: %s", stdout)
		} else {
			clusterInfo.podNetworkCIDR = matches[1]
		}
		return nil
	}(); err != nil {
		return err
	}

	return nil
}

func (data *TestData) createTestNamespace() error {
	ns := v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: testNamespace,
		},
	}
	if ns, err := data.clientset.CoreV1().Namespaces().Create(&ns); err != nil {
		// Ignore error if the namespace already exists
		if !errors.IsAlreadyExists(err) {
			return fmt.Errorf("error when creating '%s' Namespace: %v", testNamespace, err)
		}
		// When namespace already exists, check phase
		if ns.Status.Phase == v1.NamespaceTerminating {
			return fmt.Errorf("error when creating '%s' Namespace: namespace exists but is in 'Terminating' phase", testNamespace)
		}
	}
	return nil
}

// deleteTestNamespace deletes test namespace and waits for deletion to actually complete.
func (data *TestData) deleteTestNamespace(timeout time.Duration) error {
	var gracePeriodSeconds int64 = 0
	var propagationPolicy metav1.DeletionPropagation = metav1.DeletePropagationForeground
	deleteOptions := &metav1.DeleteOptions{
		GracePeriodSeconds: &gracePeriodSeconds,
		PropagationPolicy:  &propagationPolicy,
	}
	if err := data.clientset.CoreV1().Namespaces().Delete(testNamespace, deleteOptions); err != nil {
		if errors.IsNotFound(err) {
			// namespace does not exist, we return right away
			return nil
		}
		return fmt.Errorf("error when deleting '%s' Namespace: %v", testNamespace, err)
	}
	err := wait.Poll(1*time.Second, timeout, func() (bool, error) {
		if ns, err := data.clientset.CoreV1().Namespaces().Get(testNamespace, metav1.GetOptions{}); err != nil {
			if errors.IsNotFound(err) {
				// Success
				return true, nil
			}
			return false, fmt.Errorf("error when getting Namespace '%s' after delete: %v", testNamespace, err)
		} else if ns.Status.Phase != v1.NamespaceTerminating {
			return false, fmt.Errorf("deleted Namespace '%s' should be in 'Terminating' phase", testNamespace)
		}

		// Keep trying
		return false, nil
	})
	return err
}

// deployOKN deploys the OKN DaemonSet using kubectl through an SSH session to the master node.
func (data *TestData) deployOKN() error {
	// TODO: use the K8s apiserver when server side apply is available?
	// See https://kubernetes.io/docs/reference/using-api/api-concepts/#server-side-apply
	host, config, err := provider.GetSSHConfig(masterNodeName())
	if err != nil {
		return fmt.Errorf("error when retrieving SSH config for master: %v", err)
	}
	cmd := fmt.Sprintf("kubectl apply -f ~/okn.yml")
	rc, _, _, err := RunSSHCommand(host, config, cmd)
	if err != nil || rc != 0 {
		return fmt.Errorf("error when deploying OKN; is okn.yml available on the master Node?")
	}
	return nil
}

// waitForOKNDaemonSetPods waits for the K8s apiserver to report that all the OKN Pods are
// available, i.e. all the Nodes have one or more of the OKN daemon Pod running and available.
func (data *TestData) waitForOKNDaemonSetPods(timeout time.Duration) error {
	err := wait.Poll(1*time.Second, timeout, func() (bool, error) {
		daemonSet, err := data.clientset.AppsV1().DaemonSets(OKNNamespace).Get(OKNDaemonSet, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Errorf("error when getting OKN daemonset: %v", err)
		}

		if daemonSet.Status.NumberAvailable == daemonSet.Status.DesiredNumberScheduled {
			// Success
			return true, nil
		}

		// Keep trying
		return false, nil
	})
	if err == wait.ErrWaitTimeout {
		return fmt.Errorf("okn-agent DaemonSet not ready within %v", defaultTimeout)
	} else if err != nil {
		return err
	}
	return nil
}

// checkCoreDNSPods checks that all the Pods for the CoreDNS deployment are ready. If not, delete
// all the Pods to force them to restart and waits up to timeout for the Pods to become ready.
func (data *TestData) checkCoreDNSPods(timeout time.Duration) error {
	if deployment, err := data.clientset.AppsV1().Deployments(OKNNamespace).Get("coredns", metav1.GetOptions{}); err != nil {
		return fmt.Errorf("error when retrieving CoreDNS deployment: %v", err)
	} else if deployment.Status.UnavailableReplicas == 0 {
		// deployment ready, nothing to do
		return nil
	}

	// restart CoreDNS and wait for all replicas
	var gracePeriodSeconds int64 = 1
	deleteOptions := &metav1.DeleteOptions{
		GracePeriodSeconds: &gracePeriodSeconds,
	}
	listOptions := metav1.ListOptions{
		LabelSelector: "k8s-app=kube-dns",
	}
	if err := data.clientset.CoreV1().Pods(OKNNamespace).DeleteCollection(deleteOptions, listOptions); err != nil {
		return fmt.Errorf("error when deleting all CoreDNS Pods: %v", err)
	}
	err := wait.Poll(1*time.Second, timeout, func() (bool, error) {
		deployment, err := data.clientset.AppsV1().Deployments(OKNNamespace).Get("coredns", metav1.GetOptions{})
		if err != nil {
			return false, fmt.Errorf("error when retrieving CoreDNS deployment: %v", err)
		}
		if deployment.Status.UnavailableReplicas == 0 {
			return true, nil
		}
		// Keep trying
		return false, nil
	})
	if err == wait.ErrWaitTimeout {
		return fmt.Errorf("some CoreDNS replicas are still unavailable after %v", defaultTimeout)
	} else if err != nil {
		return err
	}
	return nil
}

// createClient initializes the K8s clientset in the TestData structure.
func (data *TestData) createClient() error {
	kubeconfigPath, err := provider.GetKubeconfigPath()
	if err != nil {
		return fmt.Errorf("error when getting Kubeconfig path: %v", err)
	}

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	loadingRules.ExplicitPath = kubeconfigPath
	configOverrides := &clientcmd.ConfigOverrides{}

	kubeConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides).ClientConfig()
	if err != nil {
		return fmt.Errorf("error when building kube config: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		return fmt.Errorf("error when creating kubernetes client: %v", err)
	}
	data.kubeConfig = kubeConfig
	data.clientset = clientset
	return nil
}

// deleteOKN deletes the OKN DaemonSet; we use cascading deletion, which means all the Pods created
// by OKN will be deleted. After issuing the deletion request, we poll the K8s apiserver to ensure
// that the DaemonSet does not exist any more. This function is a no-op if the OKN DaemonSet does
// not exist at the time the function is called.
func (data *TestData) deleteOKN(timeout time.Duration) error {
	var gracePeriodSeconds int64 = 5
	// Foreground deletion policy ensures that by the time the DaemonSet is deleted, there are
	// no OKN Pods left.
	var propagationPolicy metav1.DeletionPropagation = metav1.DeletePropagationForeground
	deleteOptions := &metav1.DeleteOptions{
		GracePeriodSeconds: &gracePeriodSeconds,
		PropagationPolicy:  &propagationPolicy,
	}
	if err := data.clientset.AppsV1().DaemonSets(OKNNamespace).Delete("okn-agent", deleteOptions); err != nil {
		if errors.IsNotFound(err) {
			// no OKN DaemonSet running, we return right away
			return nil
		}
		return fmt.Errorf("error when trying to delete OKN DaemonSet: %v", err)
	}
	err := wait.Poll(1*time.Second, timeout, func() (bool, error) {
		if _, err := data.clientset.AppsV1().DaemonSets(OKNNamespace).Get(OKNDaemonSet, metav1.GetOptions{}); err != nil {
			if errors.IsNotFound(err) {
				// OKN DaemonSet does not exist any more, success
				return true, nil
			}
			return false, fmt.Errorf("error when trying to get OKN DaemonSet after deletion: %v", err)
		}

		// Keep trying
		return false, nil
	})
	return err
}

// createBusyboxPodOnNode creates a Pod in the test namespace with a single busybox container. The
// Pod will be scheduled on the specified Node (if nodeName is not empty).
func (data *TestData) createBusyboxPodOnNode(name string, nodeName string) error {
	sleepDuration := 3600 // seconds
	podSpec := v1.PodSpec{
		Containers: []v1.Container{
			{
				Name:            defaultContainerName,
				Image:           "busybox",
				ImagePullPolicy: v1.PullIfNotPresent,
				Command:         []string{"sleep", strconv.Itoa(sleepDuration)},
			},
		},
		RestartPolicy: v1.RestartPolicyNever,
	}
	if nodeName != "" {
		podSpec.NodeSelector = map[string]string{
			"kubernetes.io/hostname": nodeName,
		}
	}
	if nodeName == masterNodeName() {
		// tolerate NoSchedule taint if we want Pod to run on master node
		noScheduleToleration := v1.Toleration{
			Key:      "node-role.kubernetes.io/master",
			Operator: v1.TolerationOpExists,
			Effect:   v1.TaintEffectNoSchedule,
		}
		podSpec.Tolerations = []v1.Toleration{noScheduleToleration}
	}
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       podSpec,
	}
	if _, err := data.clientset.CoreV1().Pods(testNamespace).Create(pod); err != nil {
		return err
	}
	return nil
}

// createBusyboxPod creates a Pod in the test namespace with a single busybox container.
func (data *TestData) createBusyboxPod(name string) error {
	return data.createBusyboxPodOnNode(name, "")
}

// deletePod deletes a Pod in the test namespace.
func (data *TestData) deletePod(name string) error {
	var gracePeriodSeconds int64 = 5
	deleteOptions := &metav1.DeleteOptions{
		GracePeriodSeconds: &gracePeriodSeconds,
	}
	if err := data.clientset.CoreV1().Pods(testNamespace).Delete(name, deleteOptions); err != nil {
		if !errors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// Deletes a Pod in the test namespace then waits us to timeout for the Pod not to be visible to the
// client any more.
func (data *TestData) deletePodAndWait(timeout time.Duration, name string) error {
	if err := data.deletePod(name); err != nil {
		return err
	}

	if err := wait.Poll(1*time.Second, timeout, func() (bool, error) {
		if _, err := data.clientset.CoreV1().Pods(testNamespace).Get(name, metav1.GetOptions{}); err != nil {
			if errors.IsNotFound(err) {
				return true, nil
			}
			return false, fmt.Errorf("error when getting Pod: %v", err)
		}
		// Keep trying
		return false, nil
	}); err == wait.ErrWaitTimeout {
		return fmt.Errorf("Pod '%s' still visible to client after %v", name, timeout)
	} else {
		return err
	}
}

type PodCondition func(*v1.Pod) (bool, error)

// podWaitFor polls the K8s apiserver until the specified Pod is found (in the test Namespace) and
// the condition predicate is met (or until the provided timeout expires).
func (data *TestData) podWaitFor(timeout time.Duration, name string, condition PodCondition) (*v1.Pod, error) {
	err := wait.Poll(1*time.Second, timeout, func() (bool, error) {
		if pod, err := data.clientset.CoreV1().Pods(testNamespace).Get(name, metav1.GetOptions{}); err != nil {
			if errors.IsNotFound(err) {
				return false, nil
			}
			return false, fmt.Errorf("error when getting Pod '%s': %v", name, err)
		} else {
			return condition(pod)
		}
	})
	if err != nil {
		return nil, err
	}
	return data.clientset.CoreV1().Pods(testNamespace).Get(name, metav1.GetOptions{})
}

// podWaitForRunning polls the k8s apiserver until the specified Pod is in the "running" state (or
// until the provided timeout expires).
func (data *TestData) podWaitForRunning(timeout time.Duration, name string) error {
	_, err := data.podWaitFor(timeout, name, func(pod *v1.Pod) (bool, error) {
		return pod.Status.Phase == v1.PodRunning, nil
	})
	return err
}

// podWaitForIP polls the K8s apiserver until the specified Pod is in the "running" state (or until
// the provided timeout expires). The function then returns the IP address assigned to the Pod.
func (data *TestData) podWaitForIP(timeout time.Duration, name string) (string, error) {
	pod, err := data.podWaitFor(timeout, name, func(pod *v1.Pod) (bool, error) {
		return pod.Status.Phase == v1.PodRunning, nil
	})
	if err != nil {
		return "", err
	}
	// According to the K8s API documentation (https://godoc.org/k8s.io/api/core/v1#PodStatus),
	// the PodIP field should only be empty if the Pod has not yet been scheduled, and "running"
	// implies scheduled.
	if pod.Status.PodIP == "" {
		return "", fmt.Errorf("pod is running but has no assigned IP, which should never happen")
	}
	return pod.Status.PodIP, nil
}

// deleteOneOKNPod deletes one "random" Pod belonging to the OKN DaemonSet and measure how long it
// takes for the Pod not to be visible to the client any more.
func (data *TestData) deleteOneOKNAgentPod(gracePeriodSeconds int64, timeout time.Duration) (time.Duration, error) {
	listOptions := metav1.ListOptions{
		LabelSelector: "app=okn,component=okn-agent",
	}
	pods, err := data.clientset.CoreV1().Pods(OKNNamespace).List(listOptions)
	if err != nil {
		return 0, fmt.Errorf("failed to list OKN Pods: %v", err)
	}
	if len(pods.Items) == 0 {
		return 0, fmt.Errorf("no available Pods")
	}
	onePod := pods.Items[0].Name

	deleteOptions := &metav1.DeleteOptions{
		GracePeriodSeconds: &gracePeriodSeconds,
	}

	start := time.Now()
	if err := data.clientset.CoreV1().Pods(OKNNamespace).Delete(onePod, deleteOptions); err != nil {
		return 0, fmt.Errorf("error when deleting Pod: %v", err)
	}

	if err := wait.Poll(1*time.Second, timeout, func() (bool, error) {
		if _, err := data.clientset.CoreV1().Pods(OKNNamespace).Get(onePod, metav1.GetOptions{}); err != nil {
			if errors.IsNotFound(err) {
				return true, nil
			}
			return false, fmt.Errorf("error when getting Pod: %v", err)
		}
		// Keep trying
		return false, nil
	}); err != nil {
		return 0, err
	}

	return time.Since(start), nil
}

// getOKNPodOnNode retrieves the name of the OKN Pod (okn-agent-*) running on a specific Node.
func (data *TestData) getOKNPodOnNode(nodeName string) (podName string, err error) {
	listOptions := metav1.ListOptions{
		LabelSelector: "app=okn,component=okn-agent",
		FieldSelector: fmt.Sprintf("spec.nodeName=%s", nodeName),
	}
	pods, err := data.clientset.CoreV1().Pods(OKNNamespace).List(listOptions)
	if err != nil {
		return "", fmt.Errorf("failed to list OKN Pods: %v", err)
	}
	if len(pods.Items) != 1 {
		return "", fmt.Errorf("expected *exactly* one Pod")
	}
	return pods.Items[0].Name, nil
}

// validatePodIP checks that the provided IP address is in the Pod Network CIDR for the cluster.
func validatePodIP(podNetworkCIDR, podIP string) (bool, error) {
	ip := net.ParseIP(podIP)
	if ip == nil {
		return false, fmt.Errorf("'%s' is not a valid IP address", podIP)
	}
	_, cidr, err := net.ParseCIDR(podNetworkCIDR)
	if err != nil {
		return false, fmt.Errorf("podNetworkCIDR '%s' is not a valid CIDR", podNetworkCIDR)
	}
	return cidr.Contains(ip), nil
}

// A DNS-1123 subdomain must consist of lower case alphanumeric characters
var lettersAndDigits = []rune("abcdefghijklmnopqrstuvwxyz0123456789")

func randSeq(n int) string {
	b := make([]rune, n)
	for i := range b {
		randIdx := rand.Intn(len(lettersAndDigits))
		b[i] = lettersAndDigits[randIdx]
	}
	return string(b)
}

func randPodName(prefix string) string {
	return prefix + randSeq(podNameSuffixLength)
}

// Run the provided command in the specified Container for the give Pod and returns the contents of
// stdout and stderr as strings. An error either indicates that the command couldn't be run or that
// the command returned a non-zero error code.
func (data *TestData) runCommandFromPod(podNamespace string, podName string, containerName string, cmd []string) (stdout string, stderr string, err error) {
	request := data.clientset.CoreV1().RESTClient().Post().
		Namespace(podNamespace).
		Resource("pods").
		Name(podName).
		SubResource("exec").
		Param("container", containerName).
		VersionedParams(&v1.PodExecOptions{
			Command: cmd,
			Stdin:   false,
			Stdout:  true,
			Stderr:  true,
			TTY:     false,
		}, scheme.ParameterCodec)
	exec, err := remotecommand.NewSPDYExecutor(data.kubeConfig, "POST", request.URL())
	if err != nil {
		return "", "", err
	}
	var stdoutB, stderrB bytes.Buffer
	if err := exec.Stream(remotecommand.StreamOptions{
		Stdout: &stdoutB,
		Stderr: &stderrB,
	}); err != nil {
		return stdoutB.String(), stderrB.String(), err
	}
	return stdoutB.String(), stderrB.String(), nil
}

func forAllNodes(fn func(nodeName string) error) error {
	for idx := 0; idx < clusterInfo.numNodes; idx++ {
		name := nodeName(idx)
		if name == "" {
			return fmt.Errorf("unexpected empty name for Node %d", idx)
		}
		if err := fn(name); err != nil {
			return err
		}
	}
	return nil
}

// forAllOKNPods invokes the provided function for every OKN Pod currently running on every Node.
func (data *TestData) forAllOKNPods(fn func(nodeName, podName string) error) error {
	for _, node := range clusterInfo.nodes {
		listOptions := metav1.ListOptions{
			LabelSelector: "app=okn",
			FieldSelector: fmt.Sprintf("spec.nodeName=%s", node.name),
		}
		pods, err := data.clientset.CoreV1().Pods(OKNNamespace).List(listOptions)
		if err != nil {
			return fmt.Errorf("failed to list OKN Pods on Node '%s': %v", node.name, err)
		}
		for _, pod := range pods.Items {
			if err := fn(node.name, pod.Name); err != nil {
				return err
			}
		}
	}
	return nil
}

func (data *TestData) runPingCommandFromTestPod(podName string, targetIP string, count int) error {
	cmd := []string{"ping", "-c", strconv.Itoa(count), targetIP}
	_, _, err := data.runCommandFromPod(testNamespace, podName, defaultContainerName, cmd)
	return err
}