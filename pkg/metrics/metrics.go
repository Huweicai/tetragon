// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Tetragon

package metrics

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"golang.org/x/exp/slices"

	"github.com/cilium/tetragon/pkg/logger"
	"github.com/cilium/tetragon/pkg/metrics/consts"
	"github.com/cilium/tetragon/pkg/option"
	"github.com/cilium/tetragon/pkg/podhooks"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

var (
	registry            *prometheus.Registry
	registryOnce        sync.Once
	metricsWithPod      []*prometheus.MetricVec
	metricsWithPodMutex sync.RWMutex
	podQueue            workqueue.DelayingInterface
	podQueueOnce        sync.Once
	deleteDelay         = 1 * time.Minute
)

type GranularCounter struct {
	counter     *prometheus.CounterVec
	CounterOpts prometheus.CounterOpts
	labels      []string
	register    sync.Once
}

func MustNewGranularCounter(opts prometheus.CounterOpts, labels []string) *GranularCounter {
	for _, label := range labels {
		if slices.Contains(consts.KnownMetricLabelFilters, label) {
			panic(fmt.Sprintf("labels passed to GranularCounter can't contain any of the following: %v. These labels are added by Tetragon.", consts.KnownMetricLabelFilters))
		}
	}
	return &GranularCounter{
		CounterOpts: opts,
		labels:      append(labels, consts.KnownMetricLabelFilters...),
	}
}

func (m *GranularCounter) ToProm() *prometheus.CounterVec {
	m.register.Do(func() {
		m.labels = FilterMetricLabels(m.labels...)
		m.counter = NewCounterVecWithPod(m.CounterOpts, m.labels)
	})
	return m.counter
}

// NewCounterVecWithPod is a wrapper around prometheus.NewCounterVec that also registers the metric
// to be cleaned up when a pod is deleted. It should be used only to register metrics that have
// "pod" and "namespace" labels.
func NewCounterVecWithPod(opts prometheus.CounterOpts, labels []string) *prometheus.CounterVec {
	metric := prometheus.NewCounterVec(opts, labels)
	metricsWithPodMutex.Lock()
	metricsWithPod = append(metricsWithPod, metric.MetricVec)
	metricsWithPodMutex.Unlock()
	return metric
}

// NewGaugeVecWithPod is a wrapper around prometheus.NewGaugeVec that also registers the metric
// to be cleaned up when a pod is deleted. It should be used only to register metrics that have
// "pod" and "namespace" labels.
func NewGaugeVecWithPod(opts prometheus.GaugeOpts, labels []string) *prometheus.GaugeVec {
	metric := prometheus.NewGaugeVec(opts, labels)
	metricsWithPodMutex.Lock()
	metricsWithPod = append(metricsWithPod, metric.MetricVec)
	metricsWithPodMutex.Unlock()
	return metric
}

// NewHistogramVecWithPod is a wrapper around prometheus.NewHistogramVec that also registers the metric
// to be cleaned up when a pod is deleted. It should be used only to register metrics that have
// "pod" and "namespace" labels.
func NewHistogramVecWithPod(opts prometheus.HistogramOpts, labels []string) *prometheus.HistogramVec {
	metric := prometheus.NewHistogramVec(opts, labels)
	metricsWithPodMutex.Lock()
	metricsWithPod = append(metricsWithPod, metric.MetricVec)
	metricsWithPodMutex.Unlock()
	return metric
}

// RegisterPodDeleteHandler registers handler for deleting metrics associated
// with deleted pods. Without it, Tetragon kept exposing stale metrics for
// deleted pods. This was causing continuous increase in memory usage in
// Tetragon agent as well as in the metrics scraper.
func RegisterPodDeleteHandler() {
	logger.GetLogger().Info("Registering pod delete handler for metrics")
	podhooks.RegisterCallbacksAtInit(podhooks.Callbacks{
		PodCallbacks: func(podInformer cache.SharedIndexInformer) {
			podInformer.AddEventHandler(
				cache.ResourceEventHandlerFuncs{
					DeleteFunc: func(obj interface{}) {
						var pod *corev1.Pod
						switch concreteObj := obj.(type) {
						case *corev1.Pod:
							pod = concreteObj
						case cache.DeletedFinalStateUnknown:
							// Handle the case when the watcher missed the deletion event
							// (e.g. due to a lost apiserver connection).
							deletedObj, ok := concreteObj.Obj.(*corev1.Pod)
							if !ok {
								return
							}
							pod = deletedObj
						default:
							return
						}
						queue := GetPodQueue()
						queue.AddAfter(pod, deleteDelay)
					},
				},
			)
		},
	})
}

func GetPodQueue() workqueue.DelayingInterface {
	podQueueOnce.Do(func() {
		podQueue = workqueue.NewDelayingQueue()
	})
	return podQueue
}

func DeleteMetricsForPod(pod *corev1.Pod) {
	for _, metric := range ListMetricsWithPod() {
		metric.DeletePartialMatch(prometheus.Labels{
			"pod":       pod.Name,
			"namespace": pod.Namespace,
		})
	}
}

func ListMetricsWithPod() []*prometheus.MetricVec {
	// NB: All additions to the list happen when registering metrics, so it's safe to just return
	// the list here.
	return metricsWithPod
}

func GetRegistry() *prometheus.Registry {
	registryOnce.Do(func() {
		registry = prometheus.NewRegistry()
	})
	return registry
}

func StartPodDeleteHandler() {
	queue := GetPodQueue()
	for {
		pod, quit := queue.Get()
		if quit {
			return
		}
		DeleteMetricsForPod(pod.(*corev1.Pod))
	}
}

func EnableMetrics(address string) {
	reg := GetRegistry()

	logger.GetLogger().WithField("addr", address).Info("Starting metrics server")
	http.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))
	http.ListenAndServe(address, nil)
}

// The FilterMetricLabels func takes in string arguments and returns a slice of those strings omitting the labels it is not configured for.
// IMPORTANT! The filtered metric labels must be passed last and in the exact order of consts.KnownMetricLabelFilters.
func FilterMetricLabels(labels ...string) []string {
	offset := len(labels) - len(consts.KnownMetricLabelFilters)
	if offset < 0 {
		logger.GetLogger().WithField("labels", labels).Debug("Not enough labels provided to metrics.FilterMetricLabels.")
		return labels
	}
	result := labels[:offset]
	for i, label := range consts.KnownMetricLabelFilters {
		if _, ok := option.Config.MetricsLabelFilter[label]; ok {
			result = append(result, labels[offset+i])
		}
	}
	return result
}
