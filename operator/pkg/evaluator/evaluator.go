package evaluator

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Evaluator defines the interface to evaluate pod health.
// Returns a list of saturated pod names.
type Evaluator interface {
	Evaluate(ctx context.Context, namespace string, selector metav1.LabelSelector, window, threshold string) ([]string, error)
}
