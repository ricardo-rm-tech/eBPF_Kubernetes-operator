package evaluator

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/api"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// PrometheusEvaluator implements Evaluator for disk I/O and CPU metrics.
type PrometheusEvaluator struct {
	Client     client.Client
	Endpoint   string
	MetricType string
}

func NewPrometheusEvaluator(c client.Client, endpoint, metricType string) *PrometheusEvaluator {
	if metricType == "" {
		metricType = "IO"
	}
	return &PrometheusEvaluator{Client: c, Endpoint: endpoint, MetricType: metricType}
}

func (e *PrometheusEvaluator) Evaluate(ctx context.Context, namespace string, selector metav1.LabelSelector, window, threshold string) ([]string, error) {
	log := logf.FromContext(ctx)

	if e.Endpoint == "" {
		return nil, fmt.Errorf("prometheus endpoint is empty")
	}

	// Validate the PromQL window string before sending it.
	if _, err := model.ParseDuration(window); err != nil {
		return nil, fmt.Errorf("invalid evaluationWindow %q (use forms like 30s, 5m): %w", window, err)
	}

	thresholdDur, err := time.ParseDuration(threshold)
	if err != nil {
		return nil, fmt.Errorf("invalid latencyThreshold %q (use forms like 50ms, 1s): %w", threshold, err)
	}
	if thresholdDur <= 0 {
		return nil, fmt.Errorf("latencyThreshold must be positive, got %s", threshold)
	}
	thresholdNs := thresholdDur.Nanoseconds()

	promClient, err := api.NewClient(api.Config{Address: e.Endpoint})
	if err != nil {
		return nil, fmt.Errorf("error creating prometheus client: %w", err)
	}
	v1api := v1.NewAPI(promClient)

	var query string
	switch e.MetricType {
	case "CPU":
		query = fmt.Sprintf(
			`(rate(ebpf_cpu_runq_latency_ns_total{kind="kubernetes"}[%s])
			  / ignoring() (rate(ebpf_cpu_runq_events_total{kind="kubernetes"}[%s]) > 0))
			  > %d`,
			window, window, thresholdNs)
	case "IO", "":
		query = fmt.Sprintf(
			`(rate(ebpf_block_io_latency_ns_total{kind="kubernetes"}[%s])
			  / ignoring() (rate(ebpf_block_io_count_total{kind="kubernetes"}[%s]) > 0))
			  > %d`,
			window, window, thresholdNs)
	default:
		return nil, fmt.Errorf("unsupported metricType %q", e.MetricType)
	}

	log.Info("Executing PromQL", "query", query)
	result, warnings, err := v1api.Query(ctx, query, time.Now())
	if err != nil {
		return nil, fmt.Errorf("error querying prometheus: %w", err)
	}
	if len(warnings) > 0 {
		log.Info("Prometheus warnings", "warnings", warnings)
	}

	// La query no filtra por namespace porque el collector no exporta ese
	// label (no es derivable del cgroup path). El filtrado por namespace se
	// hace abajo via List(Namespace=...) y la unión por pod_uid es segura
	// porque los UIDs de Kubernetes son UUIDs globalmente únicos.
	saturatedUIDs := make(map[string]bool)
	if result.Type() != model.ValVector {
		return nil, fmt.Errorf("unexpected prometheus result type: %v", result.Type())
	}
	for _, sample := range result.(model.Vector) {
		if podUID, ok := sample.Metric["pod_uid"]; ok && podUID != "" {
			saturatedUIDs[string(podUID)] = true
		}
	}
	if len(saturatedUIDs) == 0 {
		return nil, nil
	}

	labelSelector, err := metav1.LabelSelectorAsSelector(&selector)
	if err != nil {
		return nil, fmt.Errorf("invalid pod selector: %w", err)
	}

	var podList corev1.PodList
	if err := e.Client.List(ctx, &podList, &client.ListOptions{
		Namespace:     namespace,
		LabelSelector: labelSelector,
	}); err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	var saturatedPodNames []string
	for _, pod := range podList.Items {
		if saturatedUIDs[string(pod.UID)] {
			saturatedPodNames = append(saturatedPodNames, pod.Name)
		}
	}
	return saturatedPodNames, nil
}
