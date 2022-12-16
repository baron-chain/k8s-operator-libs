/*
Copyright 2022 NVIDIA CORPORATION & AFFILIATES

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

package upgrade

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/k8s-operator-libs/api"
	"github.com/NVIDIA/k8s-operator-libs/pkg/consts"
	"github.com/NVIDIA/k8s-operator-libs/pkg/utils"
)

// NodeUpgradeState contains a mapping between a node,
// the driver POD running on them and the daemon set, controlling this pod
type NodeUpgradeState struct {
	Node            *v1.Node
	DriverPod       *v1.Pod
	DriverDaemonSet *appsv1.DaemonSet
}

// ClusterUpgradeState contains a snapshot of the driver upgrade state in the cluster
// It contains driver upgrade policy and mappings between nodes and their upgrade state
// Nodes are grouped together with the driver POD running on them and the daemon set, controlling this pod
// This state is then used as an input for the ClusterUpgradeStateManager
type ClusterUpgradeState struct {
	NodeStates map[string][]*NodeUpgradeState
}

// NewClusterUpgradeState creates an empty ClusterUpgradeState object
func NewClusterUpgradeState() ClusterUpgradeState {
	return ClusterUpgradeState{NodeStates: make(map[string][]*NodeUpgradeState)}
}

// ClusterUpgradeStateManager serves as a state machine for the ClusterUpgradeState
// It processes each node and based on its state schedules the required jobs to change their state to the next one
type ClusterUpgradeStateManager struct {
	K8sClient                client.Client
	K8sInterface             kubernetes.Interface
	Log                      logr.Logger
	DrainManager             DrainManager
	PodManager               PodManager
	CordonManager            CordonManager
	NodeUpgradeStateProvider NodeUpgradeStateProvider
	EventRecorder            record.EventRecorder
	Namespace                v1.Namespace
}

// NewClusterUpdateStateManager creates a new instance of ClusterUpgradeStateManager
func NewClusterUpdateStateManager(
	drainManager DrainManager,
	podManager PodManager,
	cordonManager CordonManager,
	nodeUpgradeStateProvider NodeUpgradeStateProvider,
	log logr.Logger,
	k8sClient client.Client,
	k8sInterface kubernetes.Interface,
	eventRecorder record.EventRecorder) *ClusterUpgradeStateManager {
	manager := &ClusterUpgradeStateManager{
		DrainManager:             drainManager,
		PodManager:               podManager,
		CordonManager:            cordonManager,
		NodeUpgradeStateProvider: nodeUpgradeStateProvider,
		Log:                      log,
		K8sClient:                k8sClient,
		K8sInterface:             k8sInterface,
		EventRecorder:            eventRecorder,
	}
	return manager
}

// ApplyState receives a complete cluster upgrade state and, based on upgrade policy, processes each node's state.
// Based on the current state of the node, it is calculated if the node can be moved to the next state right now
// or whether any actions need to be scheduled for the node to move to the next state.
// The function is stateless and idempotent. If the error was returned before all nodes' states were processed,
// ApplyState would be called again and complete the processing - all the decisions are based on the input data.
func (m *ClusterUpgradeStateManager) ApplyState(ctx context.Context,
	currentState *ClusterUpgradeState, upgradePolicy *v1alpha1.DriverUpgradePolicySpec) error {
	m.Log.V(consts.LogLevelInfo).Info("State Manager, got state update")

	if currentState == nil {
		return fmt.Errorf("currentState should not be empty")
	}

	if upgradePolicy == nil || !upgradePolicy.AutoUpgrade {
		m.Log.V(consts.LogLevelInfo).Info("Driver auto upgrade is disabled, skipping")
		return nil
	}

	m.Log.V(consts.LogLevelInfo).Info("Node states:",
		"Unknown", len(currentState.NodeStates[UpgradeStateUnknown]),
		UpgradeStateDone, len(currentState.NodeStates[UpgradeStateDone]),
		UpgradeStateUpgradeRequired, len(currentState.NodeStates[UpgradeStateUpgradeRequired]),
		UpgradeStateCordonRequired, len(currentState.NodeStates[UpgradeStateCordonRequired]),
		UpgradeStateWaitForJobsRequired, len(currentState.NodeStates[UpgradeStateWaitForJobsRequired]),
		UpgradeStatePodDeletionRequired, len(currentState.NodeStates[UpgradeStatePodDeletionRequired]),
		UpgradeStateFailed, len(currentState.NodeStates[UpgradeStateFailed]),
		UpgradeStateDrainRequired, len(currentState.NodeStates[UpgradeStateDrainRequired]),
		UpgradeStatePodRestartRequired, len(currentState.NodeStates[UpgradeStatePodRestartRequired]),
		UpgradeStateUncordonRequired, len(currentState.NodeStates[UpgradeStateUncordonRequired]))

	upgradesInProgress := len(currentState.NodeStates[UpgradeStateCordonRequired]) +
		len(currentState.NodeStates[UpgradeStateDrainRequired]) +
		len(currentState.NodeStates[UpgradeStatePodRestartRequired]) +
		len(currentState.NodeStates[UpgradeStateWaitForJobsRequired]) +
		len(currentState.NodeStates[UpgradeStatePodDeletionRequired]) +
		len(currentState.NodeStates[UpgradeStateFailed]) +
		len(currentState.NodeStates[UpgradeStateUncordonRequired])

	var upgradesAvailable int
	if upgradePolicy.MaxParallelUpgrades == 0 {
		// Only nodes in UpgradeStateUpgradeRequired can start upgrading, so all of them will move to drain stage
		upgradesAvailable = len(currentState.NodeStates[UpgradeStateUpgradeRequired])
	} else {
		upgradesAvailable = upgradePolicy.MaxParallelUpgrades - upgradesInProgress
	}

	m.Log.V(consts.LogLevelInfo).Info("Upgrades in progress",
		"currently in progress", upgradesInProgress,
		"max parallel upgrades", upgradePolicy.MaxParallelUpgrades,
		"upgrade slots available", upgradesAvailable)

	// Determine the object to log this event
	//m.EventRecorder.Eventf(m.Namespace, v1.EventTypeNormal, GetEventReason(), "InProgress: %d, MaxParallelUpgrades: %d, UpgradeSlotsAvailable: %s", upgradesInProgress, upgradePolicy.MaxParallelUpgrades, upgradesAvailable)

	// First, check if unknown or ready nodes need to be upgraded
	err := m.ProcessDoneOrUnknownNodes(ctx, currentState, UpgradeStateUnknown)
	if err != nil {
		m.Log.V(consts.LogLevelError).Error(err, "Failed to process nodes", "state", UpgradeStateUnknown)
		return err
	}
	err = m.ProcessDoneOrUnknownNodes(ctx, currentState, UpgradeStateDone)
	if err != nil {
		m.Log.V(consts.LogLevelError).Error(err, "Failed to process nodes", "state", UpgradeStateDone)
		return err
	}
	// Start upgrade process for upgradesAvailable number of nodes
	err = m.ProcessUpgradeRequiredNodes(ctx, currentState, upgradesAvailable)
	if err != nil {
		m.Log.V(consts.LogLevelError).Error(
			err, "Failed to process nodes", "state", UpgradeStateUpgradeRequired)
		return err
	}

	err = m.ProcessCordonRequiredNodes(ctx, currentState)
	if err != nil {
		m.Log.V(consts.LogLevelError).Error(err, "Failed to cordon nodes")
		return err
	}

	err = m.ProcessWaitForJobsRequiredNodes(ctx, currentState, upgradePolicy.WaitForCompletion)
	if err != nil {
		m.Log.V(consts.LogLevelError).Error(err, "Failed to waiting for required jobs to complete")
		return err
	}

	err = m.ProcessPodDeletionRequiredNodes(ctx, currentState, upgradePolicy.PodDeletion)
	if err != nil {
		m.Log.V(consts.LogLevelError).Error(err, "Failed to delete pods")
		return err
	}

	// Schedule nodes for drain
	err = m.ProcessDrainNodes(ctx, currentState, upgradePolicy.DrainSpec)
	if err != nil {
		m.Log.V(consts.LogLevelError).Error(err, "Failed to schedule nodes drain")
		return err
	}
	err = m.ProcessPodRestartNodes(ctx, currentState)
	if err != nil {
		m.Log.V(consts.LogLevelError).Error(err, "Failed to schedule pods restart")
		return err
	}
	err = m.ProcessUpgradeFailedNodes(ctx, currentState)
	if err != nil {
		m.Log.V(consts.LogLevelError).Error(err, "Failed to process nodes which failed to drain")
		return err
	}
	err = m.ProcessUncordonRequiredNodes(ctx, currentState)
	if err != nil {
		m.Log.V(consts.LogLevelError).Error(err, "Failed to uncordon nodes")
		return err
	}
	m.Log.V(consts.LogLevelInfo).Info("State Manager, finished processing")
	return nil
}

// ProcessDoneOrUnknownNodes iterates over UpgradeStateDone or UpgradeStateUnknown nodes and determines
// whether each specific node should be in UpgradeStateUpgradeRequired or UpgradeStateDone state.
func (m *ClusterUpgradeStateManager) ProcessDoneOrUnknownNodes(
	ctx context.Context, currentClusterState *ClusterUpgradeState, nodeStateName string) error {
	m.Log.V(consts.LogLevelInfo).Info("ProcessDoneOrUnknownNodes")

	for _, nodeState := range currentClusterState.NodeStates[nodeStateName] {
		podTemplateGeneration, err := utils.GetPodTemplateGeneration(nodeState.DriverPod, m.Log)
		if err != nil {
			m.Log.V(consts.LogLevelError).Error(
				err, "Failed to get pod template generation", "pod", nodeState.DriverPod)
			return err
		}
		if podTemplateGeneration != nodeState.DriverDaemonSet.GetGeneration() {
			// If node requires upgrade and is Unschedulable, track this in an
			// annotation and leave node in Unschedulable state when upgrade completes.
			if isNodeUnschedulable(nodeState.Node) {
				annotationKey := GetUpgradeInitialStateAnnotationKey()
				annotationValue := "true"
				m.Log.V(consts.LogLevelInfo).Info("Node is unschedulable, adding annotation to track initial state of the node",
					"node", nodeState.Node.Name, "annotation", annotationKey)
				err = m.NodeUpgradeStateProvider.ChangeNodeUpgradeAnnotation(ctx, nodeState.Node, annotationKey, annotationValue)
				if err != nil {
					return err
				}
			}
			err := m.NodeUpgradeStateProvider.ChangeNodeUpgradeState(ctx, nodeState.Node, UpgradeStateUpgradeRequired)
			if err != nil {
				m.Log.V(consts.LogLevelError).Error(
					err, "Failed to change node upgrade state", "state", UpgradeStateUpgradeRequired)
				return err
			}
			m.Log.V(consts.LogLevelInfo).Info("Node requires upgrade, changed its state to UpgradeRequired",
				"node", nodeState.Node.Name)
			continue
		}

		if nodeStateName == UpgradeStateUnknown {
			err := m.NodeUpgradeStateProvider.ChangeNodeUpgradeState(ctx, nodeState.Node, UpgradeStateDone)
			if err != nil {
				m.Log.V(consts.LogLevelError).Error(
					err, "Failed to change node upgrade state", "state", UpgradeStateDone)
				return err
			}
			m.Log.V(consts.LogLevelInfo).Info("Changed node state to UpgradeDone",
				"node", nodeState.Node.Name)
			continue
		}
		m.Log.V(consts.LogLevelDebug).Info("Node in UpgradeDone state, upgrade not required",
			"node", nodeState.Node.Name)
	}
	return nil
}

// ProcessUpgradeRequiredNodes processes UpgradeStateUpgradeRequired nodes and moves them to UpgradeStateCordonRequired until
// the limit on max parallel upgrades is reached.
func (m *ClusterUpgradeStateManager) ProcessUpgradeRequiredNodes(
	ctx context.Context, currentClusterState *ClusterUpgradeState, limit int) error {
	m.Log.V(consts.LogLevelInfo).Info("ProcessUpgradeRequiredNodes")
	for _, nodeState := range currentClusterState.NodeStates[UpgradeStateUpgradeRequired] {
		if limit <= 0 {
			m.Log.V(consts.LogLevelInfo).Info("Limit for new upgrades is exceeded, skipping the iteration")
			break
		}

		if m.skipNodeUpgrade(nodeState.Node) {
			m.Log.V(consts.LogLevelInfo).Info("Node is marked for skipping upgrades", "node", nodeState.Node.Name)
			continue
		}

		err := m.NodeUpgradeStateProvider.ChangeNodeUpgradeState(ctx, nodeState.Node, UpgradeStateCordonRequired)
		if err == nil {
			limit--
			m.Log.V(consts.LogLevelInfo).Info("Node waiting for cordon",
				"node", nodeState.Node.Name)
		} else {
			m.Log.V(consts.LogLevelError).Error(
				err, "Failed to change node upgrade state", "state", UpgradeStateCordonRequired)
			return err
		}
	}

	return nil
}

// ProcessCordonRequiredNodes processes UpgradeStateCordonRequired nodes,
// cordons them and moves them to UpgradeStateWaitForJobsRequired state
func (m *ClusterUpgradeStateManager) ProcessCordonRequiredNodes(
	ctx context.Context, currentClusterState *ClusterUpgradeState) error {
	m.Log.V(consts.LogLevelInfo).Info("ProcessCordonRequiredNodes")

	for _, nodeState := range currentClusterState.NodeStates[UpgradeStateCordonRequired] {
		err := m.CordonManager.Cordon(ctx, nodeState.Node)
		if err != nil {
			m.Log.V(consts.LogLevelWarning).Error(
				err, "Node cordon failed", "node", nodeState.Node)
			return err
		}
		err = m.NodeUpgradeStateProvider.ChangeNodeUpgradeState(ctx, nodeState.Node, UpgradeStateWaitForJobsRequired)
		if err != nil {
			m.Log.V(consts.LogLevelError).Error(
				err, "Failed to change node upgrade state", "state", UpgradeStateWaitForJobsRequired)
			return err
		}
	}
	return nil
}

// ProcessWaitForJobsRequiredNodes processes UpgradeStateWaitForJobsRequired nodes,
// waits for completion of jobs and moves them to UpgradeStatePodDeletionRequired state.
func (m *ClusterUpgradeStateManager) ProcessWaitForJobsRequiredNodes(
	ctx context.Context, currentClusterState *ClusterUpgradeState, waitForCompletionSpec *v1alpha1.WaitForCompletionSpec) error {
	m.Log.V(consts.LogLevelInfo).Info("ProcessWaitForJobsRequiredNodes")

	var nodes []*v1.Node
	for _, nodeState := range currentClusterState.NodeStates[UpgradeStateWaitForJobsRequired] {
		nodes = append(nodes, nodeState.Node)
		if waitForCompletionSpec == nil || waitForCompletionSpec.PodSelector == "" {
			// update node state to next state as no pod selector is specified for waiting
			_ = m.NodeUpgradeStateProvider.ChangeNodeUpgradeState(ctx, nodeState.Node, UpgradeStatePodDeletionRequired)
			m.Log.V(consts.LogLevelInfo).Info("Updated the node state", "node", nodeState.Node.Name, "state", UpgradeStatePodDeletionRequired)
		}
	}
	// return if no pod selector is provided for waiting
	if waitForCompletionSpec == nil || waitForCompletionSpec.PodSelector == "" {
		return nil
	}

	if len(nodes) == 0 {
		// no nodes to process in this state
		return nil
	}

	podManagerConfig := PodManagerConfig{Selector: waitForCompletionSpec.PodSelector, Nodes: nodes}
	err := m.PodManager.ScheduleCheckOnPodCompletion(ctx, &podManagerConfig)
	if err != nil {
		return err
	}
	return nil
}

// ProcessPodDeletionRequiredNodes processes UpgradeStatePodDeletionRequired nodes,
// deletes select pods on a node, and moves the nodes to UpgradeStateDrainRequiredRequired state.
// Pods selected for deletion are determined via PodManager.PodDeletion
func (m *ClusterUpgradeStateManager) ProcessPodDeletionRequiredNodes(
	ctx context.Context, currentClusterState *ClusterUpgradeState, podDeletionSpec *v1alpha1.PodDeletionSpec) error {
	m.Log.V(consts.LogLevelInfo).Info("ProcessPodDeletionRequiredNodes")

	podManagerConfig := PodManagerConfig{
		DeletionSpec: podDeletionSpec,
		Nodes:        make([]*v1.Node, 0, len(currentClusterState.NodeStates[UpgradeStatePodDeletionRequired])),
	}

	for _, nodeState := range currentClusterState.NodeStates[UpgradeStatePodDeletionRequired] {
		podManagerConfig.Nodes = append(podManagerConfig.Nodes, nodeState.Node)
	}

	if len(podManagerConfig.Nodes) == 0 {
		// no nodes to process in this state
		return nil
	}

	return m.PodManager.SchedulePodEviction(ctx, &podManagerConfig)
}

// ProcessDrainNodes schedules UpgradeStateDrainRequired nodes for drain.
// If drain is disabled by upgrade policy, moves the nodes straight to UpgradeStatePodRestartRequired state.
func (m *ClusterUpgradeStateManager) ProcessDrainNodes(
	ctx context.Context, currentClusterState *ClusterUpgradeState, drainSpec *v1alpha1.DrainSpec) error {
	m.Log.V(consts.LogLevelInfo).Info("ProcessDrainNodes")
	if drainSpec == nil || !drainSpec.Enable {
		// If node drain is disabled, move nodes straight to PodRestart stage
		m.Log.V(consts.LogLevelInfo).Info("Node drain is disabled by policy, skipping this step")
		for _, nodeState := range currentClusterState.NodeStates[UpgradeStateDrainRequired] {
			err := m.NodeUpgradeStateProvider.ChangeNodeUpgradeState(ctx, nodeState.Node, UpgradeStatePodRestartRequired)
			if err != nil {
				m.Log.V(consts.LogLevelError).Error(
					err, "Failed to change node upgrade state", "state", UpgradeStatePodRestartRequired)
				return err
			}
		}
		return nil
	}

	// We want to skip operator itself during the drain because the upgrade process might hang
	// if the operator is evicted and can't be rescheduled to any other node, e.g. in a single-node cluster.
	// It's safe to do because the goal of the node draining during the upgrade is to
	// evict pods that might use driver and operator doesn't use in its own pod.
	skipDrainPodSelector := fmt.Sprintf("%s!=true", GetUpgradeSkipDrainPodLabelKey())
	if drainSpec.PodSelector == "" {
		drainSpec.PodSelector = skipDrainPodSelector
	} else {
		drainSpec.PodSelector = fmt.Sprintf("%s,%s", drainSpec.PodSelector, skipDrainPodSelector)
	}

	drainConfig := DrainConfiguration{
		Spec:  drainSpec,
		Nodes: make([]*v1.Node, 0, len(currentClusterState.NodeStates[UpgradeStateDrainRequired])),
	}
	for _, nodeState := range currentClusterState.NodeStates[UpgradeStateDrainRequired] {
		drainConfig.Nodes = append(drainConfig.Nodes, nodeState.Node)
	}

	return m.DrainManager.ScheduleNodesDrain(ctx, &drainConfig)
}

// ProcessPodRestartNodes processes UpgradeStatePodRestartRequirednodes and schedules driver pod restart for them.
// If the pod has already been restarted and is in Ready state - moves the node to UpgradeStateUncordonRequired state.
func (m *ClusterUpgradeStateManager) ProcessPodRestartNodes(
	ctx context.Context, currentClusterState *ClusterUpgradeState) error {
	m.Log.V(consts.LogLevelInfo).Info("ProcessPodRestartNodes")

	pods := make([]*v1.Pod, 0, len(currentClusterState.NodeStates[UpgradeStatePodRestartRequired]))
	for _, nodeState := range currentClusterState.NodeStates[UpgradeStatePodRestartRequired] {
		podTemplateGeneration, err := utils.GetPodTemplateGeneration(nodeState.DriverPod, m.Log)
		if err != nil {
			m.Log.V(consts.LogLevelError).Error(
				err, "Failed to get pod template generation", "pod", nodeState.DriverPod)
			return err
		}
		if podTemplateGeneration != nodeState.DriverDaemonSet.GetGeneration() {
			// Pods should only be scheduled for restart if they are not terminating or restarting already
			// To determinate terminating state we need to check for deletion timestamp with will be filled
			// one pod termination process started
			if nodeState.DriverPod.ObjectMeta.DeletionTimestamp.IsZero() {
				pods = append(pods, nodeState.DriverPod)
			}
		} else {
			driverPodInSync, err := m.isDriverPodInSync(nodeState)
			if err != nil {
				m.Log.V(consts.LogLevelError).Error(
					err, "Failed to check if driver pod on the node is in sync", "nodeState", nodeState)
				return err
			}
			if driverPodInSync {
				newUpgradeState := UpgradeStateUncordonRequired
				// If node was Unschedulable at beginning of upgrade, skip the
				// uncordon state so that node remains in the same state as
				// when the upgrade started.
				annotationKey := GetUpgradeInitialStateAnnotationKey()
				if _, ok := nodeState.Node.Annotations[annotationKey]; ok {
					m.Log.V(consts.LogLevelInfo).Info("Node was Unschedulable at beginning of upgrade, skipping uncordon",
						"node", nodeState.Node.Name)
					newUpgradeState = UpgradeStateDone
				}

				err = m.NodeUpgradeStateProvider.ChangeNodeUpgradeState(
					ctx, nodeState.Node, newUpgradeState)
				if err != nil {
					m.Log.V(consts.LogLevelError).Error(
						err, "Failed to change node upgrade state", "state", newUpgradeState)
					return err
				}

				if newUpgradeState == UpgradeStateDone {
					m.Log.V(consts.LogLevelDebug).Info("Removing node upgrade annotation",
						"node", nodeState.Node.Name, "annotation", annotationKey)
					err = m.NodeUpgradeStateProvider.ChangeNodeUpgradeAnnotation(ctx, nodeState.Node, annotationKey, "null")
					if err != nil {
						return err
					}
				}
			}
		}
	}

	// Create pod restart manager to handle pod restarts
	return m.PodManager.SchedulePodsRestart(ctx, pods)
}

// ProcessUpgradeFailedNodes processes UpgradeStateFailed nodes and checks whether the driver pod on the node
// has been successfully restarted. If the pod is in Ready state - moves the node to UpgradeStateUncordonRequired state.
func (m *ClusterUpgradeStateManager) ProcessUpgradeFailedNodes(
	ctx context.Context, currentClusterState *ClusterUpgradeState) error {
	m.Log.V(consts.LogLevelInfo).Info("ProcessUpgradeFailedNodes")

	for _, nodeState := range currentClusterState.NodeStates[UpgradeStateFailed] {
		driverPodInSync, err := m.isDriverPodInSync(nodeState)
		if err != nil {
			m.Log.V(consts.LogLevelError).Error(
				err, "Failed to check if driver pod on the node is in sync", "nodeState", nodeState)
			return err
		}
		if driverPodInSync {
			newUpgradeState := UpgradeStateUncordonRequired
			// If node was Unschedulable at beginning of upgrade, skip the
			// uncordon state so that node remains in the same state as
			// when the upgrade started.
			annotationKey := GetUpgradeInitialStateAnnotationKey()
			if _, ok := nodeState.Node.Annotations[annotationKey]; ok {
				m.Log.V(consts.LogLevelInfo).Info("Node was Unschedulable at beginning of upgrade, skipping uncordon",
					"node", nodeState.Node.Name)
				newUpgradeState = UpgradeStateDone
			}

			err = m.NodeUpgradeStateProvider.ChangeNodeUpgradeState(ctx, nodeState.Node, newUpgradeState)
			if err != nil {
				m.Log.V(consts.LogLevelError).Error(
					err, "Failed to change node upgrade state", "state", newUpgradeState)
				return err
			}

			if newUpgradeState == UpgradeStateDone {
				m.Log.V(consts.LogLevelDebug).Info("Removing node upgrade annotation",
					"node", nodeState.Node.Name, "annotation", annotationKey)
				err = m.NodeUpgradeStateProvider.ChangeNodeUpgradeAnnotation(ctx, nodeState.Node, annotationKey, "null")
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

// ProcessUncordonRequiredNodes processes UpgradeStateUncordonRequired nodes,
// uncordons them and moves them to UpgradeStateDone state
func (m *ClusterUpgradeStateManager) ProcessUncordonRequiredNodes(
	ctx context.Context, currentClusterState *ClusterUpgradeState) error {
	m.Log.V(consts.LogLevelInfo).Info("ProcessUncordonRequiredNodes")

	for _, nodeState := range currentClusterState.NodeStates[UpgradeStateUncordonRequired] {
		err := m.CordonManager.Uncordon(ctx, nodeState.Node)
		if err != nil {
			m.Log.V(consts.LogLevelWarning).Error(
				err, "Node uncordon failed", "node", nodeState.Node)
			return err
		}
		err = m.NodeUpgradeStateProvider.ChangeNodeUpgradeState(ctx, nodeState.Node, UpgradeStateDone)
		if err != nil {
			m.Log.V(consts.LogLevelError).Error(
				err, "Failed to change node upgrade state", "state", UpgradeStateDone)
			return err
		}
	}
	return nil
}

func (m *ClusterUpgradeStateManager) isDriverPodInSync(nodeState *NodeUpgradeState) (bool, error) {
	podTemplateGeneration, err := utils.GetPodTemplateGeneration(nodeState.DriverPod, m.Log)
	if err != nil {
		m.Log.V(consts.LogLevelError).Error(
			err, "Failed to get pod template generation", "pod", nodeState.DriverPod)
		return false, err
	}
	// If the pod generation matches the daemonset generation
	if podTemplateGeneration == nodeState.DriverDaemonSet.GetGeneration() &&
		// And the pod is running
		nodeState.DriverPod.Status.Phase == "Running" &&
		// And it has at least 1 container
		len(nodeState.DriverPod.Status.ContainerStatuses) != 0 {
		for i := range nodeState.DriverPod.Status.ContainerStatuses {
			if !nodeState.DriverPod.Status.ContainerStatuses[i].Ready {
				// Return false if at least 1 container isn't ready
				return false, nil
			}
		}

		// And each container is ready
		return true, nil
	}

	return false, nil
}

// skipNodeUpgrade returns true if node is labelled to skip driver upgrades
func (m *ClusterUpgradeStateManager) skipNodeUpgrade(node *v1.Node) bool {
	if node.Labels[GetUpgradeSkipNodeLabelKey()] == "true" {
		return true
	}
	return false
}

func isNodeUnschedulable(node *v1.Node) bool {
	if node.Spec.Unschedulable {
		return true
	}
	return false
}
