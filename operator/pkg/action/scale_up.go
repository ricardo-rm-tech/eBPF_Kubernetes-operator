package action

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

type ScaleUpAction struct {
	Client         client.Client
	CPUStepPercent int32
	MaxCPULimit    string
	FallbackAction RemediationAction
}

func NewScaleUpAction(c client.Client, stepPercent int32, maxLimit string, fallback RemediationAction) *ScaleUpAction {
	return &ScaleUpAction{
		Client:         c,
		CPUStepPercent: stepPercent,
		MaxCPULimit:    maxLimit,
		FallbackAction: fallback,
	}
}

func (a *ScaleUpAction) Execute(ctx context.Context, podName string, namespace string) error {
	log := logf.FromContext(ctx)

	var pod corev1.Pod
	if err := a.Client.Get(ctx, types.NamespacedName{Name: podName, Namespace: namespace}, &pod); err != nil {
		return fmt.Errorf("failed to get pod %s: %w", podName, err)
	}

	maxQuantity, err := resource.ParseQuantity(a.MaxCPULimit)
	if err != nil {
		return fmt.Errorf("failed to parse max CPU limit: %w", err)
	}
	maxMillis := maxQuantity.MilliValue()

	originalPod := pod.DeepCopy()
	changed := false
	anyHeadroom := false

	for i := range pod.Spec.Containers {
		container := &pod.Spec.Containers[i]

		// Baseline: prefer the explicit limit; fall back to the request.
		// If neither is set, the container is effectively uncapped — skipping
		// avoids an accidental downgrade.
		var baseline resource.Quantity
		if q, ok := container.Resources.Limits[corev1.ResourceCPU]; ok {
			baseline = q
		} else if q, ok := container.Resources.Requests[corev1.ResourceCPU]; ok {
			baseline = q
		} else {
			log.Info("container has no CPU limit nor request, skipping to avoid downgrade", "container", container.Name)
			continue
		}

		currentMillis := baseline.MilliValue()
		if currentMillis >= maxMillis {
			continue
		}

		stepMillis := (currentMillis * int64(a.CPUStepPercent)) / 100
		if stepMillis < 10 {
			stepMillis = 10
		}
		newMillis := currentMillis + stepMillis
		if newMillis >= maxMillis {
			newMillis = maxMillis
		}
		anyHeadroom = true

		if container.Resources.Limits == nil {
			container.Resources.Limits = corev1.ResourceList{}
		}
		container.Resources.Limits[corev1.ResourceCPU] = *resource.NewMilliQuantity(newMillis, resource.DecimalSI)
		changed = true
		log.Info("Scaling up container CPU limit",
			"container", container.Name,
			"old", baseline.String(),
			"new", container.Resources.Limits[corev1.ResourceCPU])
	}

	if changed {
		log.Info("Applying In-Place Pod Vertical Scaling patch (resize subresource)", "pod", podName)
		patch := client.MergeFrom(originalPod)
		if err := a.Client.SubResource("resize").Patch(ctx, &pod, patch); err != nil {
			return fmt.Errorf("failed to apply resize patch (requires K8s >= 1.27 with InPlacePodVerticalScaling): %w", err)
		}
		return nil
	}

	if !anyHeadroom {
		log.Info("Pod has reached maximum CPU limit, using fallback action", "pod", podName)
		if a.FallbackAction != nil {
			return a.FallbackAction.Execute(ctx, podName, namespace)
		}
		return fmt.Errorf("max CPU limit reached and no fallback action configured")
	}

	log.Info("No CPU limit changes required for pod", "pod", podName)
	return nil
}
