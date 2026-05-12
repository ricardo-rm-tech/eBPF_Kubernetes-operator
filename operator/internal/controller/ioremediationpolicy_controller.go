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

package controller

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	autoremediationv1alpha1 "github.com/richi/tfg/operator/api/v1alpha1"
	"github.com/richi/tfg/operator/pkg/action"
	"github.com/richi/tfg/operator/pkg/evaluator"
)

const (
	defaultRequeue = time.Minute
	minRequeue     = 15 * time.Second
)

// IORemediationPolicyReconciler reconciles a IORemediationPolicy object
type IORemediationPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=autoremediation.tfg.local,resources=ioremediationpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoremediation.tfg.local,resources=ioremediationpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=autoremediation.tfg.local,resources=ioremediationpolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments;replicasets,verbs=get;list;watch;update;patch

func (r *IORemediationPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var policy autoremediationv1alpha1.IORemediationPolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.Info("Reconciling IORemediationPolicy", "policy", policy.Name, "action", policy.Spec.Action)

	requeueAfter := requeueFromSpec(policy.Spec.EvaluationWindow)

	eval := evaluator.NewPrometheusEvaluator(r.Client, policy.Spec.PrometheusEndpoint, policy.Spec.MetricType)
	saturatedPods, err := eval.Evaluate(ctx, req.Namespace, policy.Spec.TargetPodSelector, policy.Spec.EvaluationWindow, policy.Spec.LatencyThreshold)
	if err != nil {
		log.Error(err, "Failed to evaluate metrics")
		r.setCondition(ctx, &policy, "Degraded", metav1.ConditionTrue, "EvaluationFailed", err.Error())
		return ctrl.Result{RequeueAfter: requeueAfter}, err
	}

	remediationAction, err := r.buildAction(&policy)
	if err != nil {
		log.Info("Invalid policy", "err", err)
		r.setCondition(ctx, &policy, "Degraded", metav1.ConditionTrue, "InvalidAction", err.Error())
		return ctrl.Result{}, nil
	}

	var failures int
	for _, podName := range saturatedPods {
		log.Info("Executing action for saturated pod", "pod", podName, "action", policy.Spec.Action)
		if err := remediationAction.Execute(ctx, podName, req.Namespace); err != nil {
			log.Error(err, "Failed to execute remediation action", "pod", podName)
			failures++
		}
	}

	switch {
	case len(saturatedPods) == 0:
		r.setCondition(ctx, &policy, "Available", metav1.ConditionTrue, "NoSaturation", "no pods exceeded latency threshold")
	case failures == 0:
		r.setCondition(ctx, &policy, "Progressing", metav1.ConditionTrue, "Remediating",
			fmt.Sprintf("remediated %d pod(s)", len(saturatedPods)))
	default:
		r.setCondition(ctx, &policy, "Degraded", metav1.ConditionTrue, "PartialFailure",
			fmt.Sprintf("%d/%d remediation actions failed", failures, len(saturatedPods)))
	}

	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

func (r *IORemediationPolicyReconciler) buildAction(policy *autoremediationv1alpha1.IORemediationPolicy) (action.RemediationAction, error) {
	switch policy.Spec.Action {
	case "EvictAndTaint":
		return action.NewEvictAndTaintAction(r.Client, taintDuration(policy)), nil
	case "MigrateStorageClass":
		targetSC := ""
		if policy.Spec.MigrateStorageConfig != nil {
			targetSC = policy.Spec.MigrateStorageConfig.TargetStorageClass
		}
		return action.NewMigrateStorageAction(r.Client, targetSC), nil
	case "ScaleUp":
		var stepPercent int32 = 20
		maxLimit := "2000m"
		if policy.Spec.ScaleUpConfig != nil {
			if policy.Spec.ScaleUpConfig.CPUStepPercent > 0 {
				stepPercent = policy.Spec.ScaleUpConfig.CPUStepPercent
			}
			if policy.Spec.ScaleUpConfig.MaxCPULimit != "" {
				maxLimit = policy.Spec.ScaleUpConfig.MaxCPULimit
			}
		}
		fallback := action.NewEvictAndTaintAction(r.Client, taintDuration(policy))
		return action.NewScaleUpAction(r.Client, stepPercent, maxLimit, fallback), nil
	default:
		return nil, fmt.Errorf("unknown action %q", policy.Spec.Action)
	}
}

func taintDuration(policy *autoremediationv1alpha1.IORemediationPolicy) *time.Duration {
	if policy.Spec.EvictAndTaintConfig == nil || policy.Spec.EvictAndTaintConfig.TaintDuration == nil {
		return nil
	}
	d := policy.Spec.EvictAndTaintConfig.TaintDuration.Duration
	return &d
}

func requeueFromSpec(window string) time.Duration {
	d, err := time.ParseDuration(window)
	if err != nil || d < minRequeue {
		return defaultRequeue
	}
	return d
}

func (r *IORemediationPolicyReconciler) setCondition(ctx context.Context, policy *autoremediationv1alpha1.IORemediationPolicy, condType string, status metav1.ConditionStatus, reason, message string) {
	log := logf.FromContext(ctx)
	meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: policy.Generation,
	})
	if err := r.Status().Update(ctx, policy); err != nil {
		log.V(1).Info("failed to update status", "err", err)
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *IORemediationPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&autoremediationv1alpha1.IORemediationPolicy{}).
		Named("ioremediationpolicy").
		Complete(r)
}
