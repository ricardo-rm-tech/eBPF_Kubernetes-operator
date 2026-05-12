package action

import (
	"context"
)

// RemediationAction defines what to do with a saturated pod.
type RemediationAction interface {
	Execute(ctx context.Context, podName string, namespace string) error
}
