/*
Licensed to the Apache Software Foundation (ASF) under one or more
contributor license agreements.  See the NOTICE file distributed with
this work for additional information regarding copyright ownership.
The ASF licenses this file to You under the Apache License, Version 2.0
(the "License"); you may not use this file except in compliance with
the License.  You may obtain a copy of the License at

   http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package integration

import (
	"context"
	"strconv"

	"github.com/pkg/errors"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"

	ctrl "sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/apache/camel-k/pkg/apis/camel/v1"
	"github.com/apache/camel-k/pkg/trait"
	"github.com/apache/camel-k/pkg/util/digest"
	"github.com/apache/camel-k/pkg/util/kubernetes"
)

func NewMonitorAction() Action {
	return &monitorAction{}
}

type monitorAction struct {
	baseAction
}

func (action *monitorAction) Name() string {
	return "monitor"
}

func (action *monitorAction) CanHandle(integration *v1.Integration) bool {
	return integration.Status.Phase == v1.IntegrationPhaseDeploying ||
		integration.Status.Phase == v1.IntegrationPhaseRunning ||
		integration.Status.Phase == v1.IntegrationPhaseError
}

func (action *monitorAction) Handle(ctx context.Context, integration *v1.Integration) (*v1.Integration, error) {
	// At that staged the Integration must have a Kit
	if integration.Status.IntegrationKit == nil {
		return nil, errors.Errorf("no kit set on integration %s", integration.Name)
	}

	// Check if the Integration requires a rebuild
	hash, err := digest.ComputeForIntegration(integration)
	if err != nil {
		return nil, err
	}

	if hash != integration.Status.Digest {
		action.L.Info("Integration needs a rebuild")

		integration.Initialize()
		integration.Status.Digest = hash

		return integration, nil
	}

	kit, err := kubernetes.GetIntegrationKit(ctx, action.client, integration.Status.IntegrationKit.Name, integration.Status.IntegrationKit.Namespace)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to find integration kit %s/%s, %s", integration.Status.IntegrationKit.Namespace, integration.Status.IntegrationKit.Name, err)
	}

	// Check if an IntegrationKit with higher priority is ready
	priority, ok := kit.Labels[v1.IntegrationKitPriorityLabel]
	if !ok {
		priority = "0"
	}
	withHigherPriority, err := labels.NewRequirement(v1.IntegrationKitPriorityLabel, selection.GreaterThan, []string{priority})
	if err != nil {
		return nil, err
	}
	kits, err := lookupKitsForIntegration(ctx, action.client, integration, ctrl.MatchingLabelsSelector{
		Selector: labels.NewSelector().Add(*withHigherPriority),
	})
	if err != nil {
		return nil, err
	}
	priorityReadyKit, err := findHighestPriorityReadyKit(kits)
	if err != nil {
		return nil, err
	}
	if priorityReadyKit != nil {
		integration.SetIntegrationKit(priorityReadyKit)
	}

	// Run traits that are enabled for the phase
	_, err = trait.Apply(ctx, action.client, integration, kit)
	if err != nil {
		return nil, err
	}

	// Enforce the scale sub-resource label selector.
	// It is used by the HPA that queries the scale sub-resource endpoint,
	// to list the pods owned by the integration.
	integration.Status.Selector = v1.IntegrationLabel + "=" + integration.Name

	// Update the replicas count
	pendingPods := &corev1.PodList{}
	err = action.client.List(ctx, pendingPods,
		ctrl.InNamespace(integration.Namespace),
		ctrl.MatchingLabels{v1.IntegrationLabel: integration.Name},
		ctrl.MatchingFields{"status.phase": string(corev1.PodPending)})
	if err != nil {
		return nil, err
	}
	runningPods := &corev1.PodList{}
	err = action.client.List(ctx, runningPods,
		ctrl.InNamespace(integration.Namespace),
		ctrl.MatchingLabels{v1.IntegrationLabel: integration.Name},
		ctrl.MatchingFields{"status.phase": string(corev1.PodRunning)})
	if err != nil {
		return nil, err
	}
	nonTerminatingPods := 0
	for _, pod := range runningPods.Items {
		if pod.DeletionTimestamp != nil {
			continue
		}
		nonTerminatingPods++
	}
	podCount := int32(len(pendingPods.Items) + nonTerminatingPods)
	integration.Status.Replicas = &podCount

	// Reconcile Integration phase
	if integration.Status.Phase == v1.IntegrationPhaseDeploying {
		integration.Status.Phase = v1.IntegrationPhaseRunning
	}

	// Mirror ready condition from the owned resource (e.g., Deployment, CronJob, KnativeService ...)
	// into the owning integration
	previous := integration.Status.GetCondition(v1.IntegrationConditionReady)
	kubernetes.MirrorReadyCondition(ctx, action.client, integration)

	if next := integration.Status.GetCondition(v1.IntegrationConditionReady); (previous == nil || previous.FirstTruthyTime == nil || previous.FirstTruthyTime.IsZero()) &&
		next != nil && next.Status == corev1.ConditionTrue && !(next.FirstTruthyTime == nil || next.FirstTruthyTime.IsZero()) {
		// Observe the time to first readiness metric
		duration := next.FirstTruthyTime.Time.Sub(integration.Status.InitializationTimestamp.Time)
		action.L.Infof("First readiness after %s", duration)
		timeToFirstReadiness.Observe(duration.Seconds())
	}

	// the integration pod may be in running phase, but the corresponding container running the integration code
	// may be in error state, in this case we should check the deployment status and set the integration status accordingly.
	if kubernetes.IsConditionTrue(integration, v1.IntegrationConditionDeploymentAvailable) {
		deployment, err := kubernetes.GetDeployment(ctx, action.client, integration.Name, integration.Namespace)
		if err != nil {
			return nil, err
		}

		switch integration.Status.Phase {
		case v1.IntegrationPhaseRunning:
			deployUnavailable := false
			progressingFailing := false
			for _, c := range deployment.Status.Conditions {
				// first, check if the container status is not available
				if c.Type == appsv1.DeploymentAvailable {
					deployUnavailable = c.Status == corev1.ConditionFalse
				}
				// second, check when it is progressing and reason is the replicas are available but the number of replicas are zero
				// in this case, the container integration is failing
				if c.Type == appsv1.DeploymentProgressing {
					progressingFailing = c.Status == corev1.ConditionTrue && c.Reason == "NewReplicaSetAvailable" && deployment.Status.AvailableReplicas < 1
				}
			}
			if deployUnavailable && progressingFailing {
				notAvailableCondition := v1.IntegrationCondition{
					Type:    v1.IntegrationConditionReady,
					Status:  corev1.ConditionFalse,
					Reason:  v1.IntegrationConditionErrorReason,
					Message: "The corresponding pod(s) may be in error state, look at the pod status or log for errors",
				}
				integration.Status.SetConditions(notAvailableCondition)
				integration.Status.Phase = v1.IntegrationPhaseError
				return integration, nil
			}

		case v1.IntegrationPhaseError:
			// if the integration is in error phase, check if the corresponding pod is running ok, the user may have updated the integration.
			deployAvailable := false
			progressingOk := false
			for _, c := range deployment.Status.Conditions {
				// first, check if the container is in available state
				if c.Type == appsv1.DeploymentAvailable {
					deployAvailable = c.Status == corev1.ConditionTrue
				}
				// second, check the progressing and the reasons
				if c.Type == appsv1.DeploymentProgressing {
					progressingOk = c.Status == corev1.ConditionTrue && (c.Reason == "NewReplicaSetAvailable" || c.Reason == "ReplicaSetUpdated")
				}
			}
			if deployAvailable && progressingOk {
				availableCondition := v1.IntegrationCondition{
					Type:   v1.IntegrationConditionReady,
					Status: corev1.ConditionTrue,
					Reason: v1.IntegrationConditionReplicaSetReadyReason,
				}
				integration.Status.SetConditions(availableCondition)
				integration.Status.Phase = v1.IntegrationPhaseRunning
				return integration, nil
			}
		}
	}

	return integration, nil
}

func findHighestPriorityReadyKit(kits []v1.IntegrationKit) (*v1.IntegrationKit, error) {
	if len(kits) == 0 {
		return nil, nil
	}
	var kit *v1.IntegrationKit
	priority := 0
	for i, k := range kits {
		if k.Status.Phase != v1.IntegrationKitPhaseReady {
			continue
		}
		p, err := strconv.Atoi(k.Labels[v1.IntegrationKitPriorityLabel])
		if err != nil {
			return nil, err
		}
		if p > priority {
			kit = &kits[i]
			priority = p
		}
	}
	return kit, nil
}
