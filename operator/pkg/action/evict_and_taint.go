package action

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const DefaultTaintKey = "autoremediation.tfg.local/saturation"

type EvictAndTaintAction struct {
	Client        client.Client
	TaintKey      string
	TaintDuration *time.Duration
}

func NewEvictAndTaintAction(c client.Client, duration *time.Duration) *EvictAndTaintAction {
	return &EvictAndTaintAction{Client: c, TaintKey: DefaultTaintKey, TaintDuration: duration}
}

func NewEvictAndTaintActionWithKey(c client.Client, key string, duration *time.Duration) *EvictAndTaintAction {
	if key == "" {
		key = DefaultTaintKey
	}
	return &EvictAndTaintAction{Client: c, TaintKey: key, TaintDuration: duration}
}

func (a *EvictAndTaintAction) Execute(ctx context.Context, podName string, namespace string) error {
	log := logf.FromContext(ctx)

	var pod corev1.Pod
	if err := a.Client.Get(ctx, types.NamespacedName{Name: podName, Namespace: namespace}, &pod); err != nil {
		return fmt.Errorf("failed to get pod: %w", err)
	}

	if pod.Spec.NodeName == "" {
		return fmt.Errorf("pod is not scheduled to a node, skipping")
	}

	var node corev1.Node
	if err := a.Client.Get(ctx, types.NamespacedName{Name: pod.Spec.NodeName}, &node); err != nil {
		return fmt.Errorf("failed to get node: %w", err)
	}

	now := time.Now()
	originalNode := node.DeepCopy()

	// Prune expired taints (only ours, identified by key) and check presence.
	taintPresent := false
	pruned := node.Spec.Taints[:0]
	for _, t := range node.Spec.Taints {
		if t.Key == a.TaintKey {
			if a.TaintDuration != nil && t.TimeAdded != nil &&
				now.Sub(t.TimeAdded.Time) >= *a.TaintDuration {
				log.Info("Removing expired taint", "node", node.Name, "key", t.Key)
				continue
			}
			taintPresent = true
		}
		pruned = append(pruned, t)
	}
	node.Spec.Taints = pruned

	if !taintPresent {
		newTaint := corev1.Taint{
			Key:       a.TaintKey,
			Value:     "true",
			Effect:    corev1.TaintEffectNoSchedule,
			TimeAdded: &metav1.Time{Time: now},
		}
		node.Spec.Taints = append(node.Spec.Taints, newTaint)
		log.Info("Adding saturation taint to node", "node", node.Name, "key", a.TaintKey)
	}

	if !equalTaints(originalNode.Spec.Taints, node.Spec.Taints) {
		if err := a.Client.Patch(ctx, &node, client.MergeFrom(originalNode)); err != nil {
			return fmt.Errorf("failed to update node taints: %w", err)
		}
	}

	log.Info("Evicting saturated pod", "pod", pod.Name)
	eviction := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
		},
		DeleteOptions: &metav1.DeleteOptions{},
	}

	if err := a.Client.SubResource("eviction").Create(ctx, &pod, eviction); err != nil {
		return fmt.Errorf("failed to evict pod: %w", err)
	}

	return nil
}

func equalTaints(a, b []corev1.Taint) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Key != b[i].Key || a[i].Value != b[i].Value || a[i].Effect != b[i].Effect {
			return false
		}
	}
	return true
}
