/*
Copyright 2022 The Arbiter Authors.

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

package manager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	gocache "github.com/patrickmn/go-cache"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	informerv1 "k8s.io/client-go/informers/core/v1"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	schedv1alpha1 "github.com/kube-arbiter/arbiter/pkg/apis/v1alpha1"
	clientset "github.com/kube-arbiter/arbiter/pkg/generated/clientset/versioned"
)

const (
	ManagerLogPrefix = "[Arbiter-Manager] "
)

var (
	ErrNotFoundInCache = errors.New("not Found In Memory Cache")
	ErrTypeAssertion   = errors.New("type assertion err")
	ErrNoData          = errors.New("obi have no data")
)

type ScoreResult struct {
	NameKey string
	schedv1alpha1.ScoreSpec
	Result int64
	Err    error
}

type Manager interface {
	GetScore(ctx context.Context, namespace string) (scoreResults []ScoreResult, totalWeight int64)
	GetPodOBI(ctx context.Context, pod *v1.Pod) (obi map[string]OBI, err error)
	GetNodeOBI(ctx context.Context, nodeName string) (obi map[string]OBI, err error)
}

type manager struct {
	client clientset.Interface

	// TODO(Abirdcfly): should benchmark gocache or replace with other struct
	podMetric  map[string]*gocache.Cache
	nodeMetric map[string]*gocache.Cache
	score      map[string]*gocache.Cache

	// snapshotSharedLister is pod shared list
	snapshotSharedLister framework.SharedLister
	// podLister is pod lister
	podLister listerv1.PodLister

	sync.RWMutex
	nodeLister listerv1.NodeLister
}

func (mgr *manager) GetPodOBI(ctx context.Context, pod *v1.Pod) (obi map[string]OBI, err error) {
	// TODO(Abirdcfly): pod metric support will in v0.2.0
	return
}

func (mgr *manager) GetNodeOBI(ctx context.Context, nodeName string) (obi map[string]OBI, err error) {
	nodeCache, ok := mgr.nodeMetric[nodeName]
	if !ok {
		err = ErrNotFoundInCache
		klog.V(4).ErrorS(err, "Failed to get node OBI", "node", nodeName)
		return
	}
	obi = make(map[string]OBI, nodeCache.ItemCount())
	for k, v := range nodeCache.Items() {
		data, ok := v.Object.(OBI)
		if ok {
			obi[k] = data
		} else {
			err = ErrNotFoundInCache
			klog.V(4).ErrorS(err, "Failed to get node OBI", "node", nodeName)
			return
		}
	}
	return
}

func NewManager(client clientset.Interface, snapshotSharedLister framework.SharedLister, podInformer informerv1.PodInformer, nodeInformer informerv1.NodeInformer) *manager {
	pgMgr := &manager{
		client:               client,
		podMetric:            make(map[string]*gocache.Cache),
		nodeMetric:           make(map[string]*gocache.Cache),
		score:                make(map[string]*gocache.Cache),
		snapshotSharedLister: snapshotSharedLister,
		podLister:            podInformer.Lister(),
		nodeLister:           nodeInformer.Lister(),
		RWMutex:              sync.RWMutex{},
	}
	return pgMgr
}

func (mgr *manager) ScoreAdd(obj interface{}) {
	klog.V(5).Infof("%s get new Score", ManagerLogPrefix)
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		runtime.HandleError(err)
		return
	}
	ns, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		runtime.HandleError(err)
		return
	}
	score, ok := obj.(*schedv1alpha1.Score)
	if !ok {
		klog.V(4).ErrorS(ErrTypeAssertion, "Failed to get score", "score", key)
		return
	}
	if _, ok := mgr.score[ns]; !ok {
		mgr.Lock()
		mgr.score[ns] = gocache.New(gocache.NoExpiration, gocache.NoExpiration)
		mgr.Unlock()
	}
	scoreCache := mgr.score[ns]
	scoreCache.Set(name, score.Spec, gocache.NoExpiration)
}

func (mgr *manager) ScoreUpdate(old interface{}, new interface{}) {
	klog.V(5).Infof("%s get update Score", ManagerLogPrefix)
	mgr.ScoreAdd(new)
}

func (mgr *manager) ScoreDelete(obj interface{}) {
	klog.V(5).Infof("%s get delete Score", ManagerLogPrefix)
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		runtime.HandleError(err)
		return
	}
	ns, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		runtime.HandleError(err)
		return
	}
	_, ok := obj.(*schedv1alpha1.Score)
	if !ok {
		klog.V(4).ErrorS(ErrTypeAssertion, "Failed to get score", "score", key)
		return
	}
	scoreCache, exist := mgr.score[ns]
	if !exist {
		klog.V(4).ErrorS(ErrNotFoundInCache, "cant delete score, score not in cache", "score", key)
	}
	scoreCache.Delete(name)
	if scoreCache.ItemCount() == 0 {
		mgr.Lock()
		delete(mgr.score, ns)
		mgr.Unlock()
	}
}

// GetScore get all Score in the specified namespace.
// If the return is empty, then get all Score in the namespace which arbiter-Scheduler pod is located.
// If the return is also empty, fallback to get the Score in the kube-system namespace.
func (mgr *manager) GetScore(ctx context.Context, namespace string) (res []ScoreResult, totalWeight int64) {
	if namespace == "" {
		namespace = SchedulerNamespace()
	}
	scoreCache, exist := mgr.score[namespace]
	count := 0
	if exist {
		count = scoreCache.ItemCount()
	}
	if !exist || count == 0 {
		klog.V(4).InfoS(namespace+" has no score", "namespace", namespace)
		if namespace == metav1.NamespaceSystem {
			// final fallback. just exit.
			return nil, 0
		}
		fallbackNamespace := SchedulerNamespace()
		if namespace == fallbackNamespace {
			fallbackNamespace = metav1.NamespaceSystem
		}
		klog.V(2).InfoS(fmt.Sprintf("ns:%s has no Score CR, try to get Score CR in ns:%s instead", namespace, fallbackNamespace), "namespace", namespace)
		return mgr.GetScore(ctx, fallbackNamespace)
	}
	res = make([]ScoreResult, 0)
	for name, v := range scoreCache.Items() {
		scoreSpec, ok := v.Object.(schedv1alpha1.ScoreSpec)
		if ok {
			if strings.TrimSpace(scoreSpec.Logic) == "" {
				continue
			}
			if scoreSpec.Weight <= 0 {
				continue
			}
			res = append(res, ScoreResult{
				NameKey:   namespace + "/" + name,
				ScoreSpec: scoreSpec,
				Result:    0,
			})
			totalWeight += scoreSpec.Weight
		}
	}
	return
}

func (mgr *manager) ObservabilityIndicantAdd(obj interface{}) {
	klog.V(5).Infoln(ManagerLogPrefix + "get new ObservabilityIndicant")
	_, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		klog.V(4).ErrorS(err, ManagerLogPrefix+"Failed to obj in cache when add", "obj", obj)
		runtime.HandleError(err)
		return
	}
	obi, ok := obj.(*schedv1alpha1.ObservabilityIndicant)
	if !ok {
		klog.V(4).ErrorS(ErrTypeAssertion, ManagerLogPrefix+"Failed to get observability indicant", "obj", obj)
		return
	}
	klog.V(5).Infoln(ManagerLogPrefix+"get new ObservabilityIndicant", "obi", klog.KObj(obi))
	expiration := gocache.NoExpiration
	if len(obi.Status.Metrics) == 0 {
		klog.V(4).ErrorS(ErrNoData, ManagerLogPrefix+"obi have no data", "obi", klog.KObj(obi))
		return
	}
	var cacheName *gocache.Cache
	switch {
	case IsResourceNode(obi.Spec.TargetRef):
		nodeName := obi.Spec.TargetRef.Name
		if nodeName == "" {
			for _, m := range obi.Status.Metrics {
				if len(m) == 0 {
					return
				}
				nodeName = m[0].TargetItem
				break
			}
			if nodeName == "" {
				return
			}
		}
		if _, ok := mgr.nodeMetric[nodeName]; !ok {
			mgr.nodeMetric[nodeName] = gocache.New(gocache.NoExpiration, gocache.NoExpiration)
		}
		cacheName = mgr.nodeMetric[nodeName]
	// case IsResourcePod(obi.Spec.TargetRef):
	//	cacheName = mgr.podMetric
	default:
		klog.V(4).ErrorS(ErrNotFoundInCache, ManagerLogPrefix+"Failed to get cacheName", "TargetRef", obi.Spec.TargetRef)
		return
	}
	cacheKey := getMetricCacheKey(obi)
	/*
		Structure of a typical obi:
			{
				"metrics": {
					"cpu": [
						{
							"endTime": "2022-09-01T10:36:57Z",
							"records": [
								{
									"timestamp": 1662024960000,
									"value": "0.470097"
								},
								{
									"timestamp": 1662025020000,
									"value": "0.466142"
								}
							],
							"startTime": "2022-09-01T09:36:57Z",
							"targetItem": "cpu-cost-1-7db7f54cdd-fnh8k",
							"unit": "C"
						}
					]
			    }
			}
		Extending the structure of obi:
			{
			    "metrics": {
			        "cpu": [
			            {
			                "endTime": "2022-10-31T01:31:27Z",
			                "records": [
			                    {
			                        "timestamp": 1666949661719,
			                        "value": "[{\"metric\":{},\"values\":[[1666949631.719,\"14.25\"]]}]"
			                    },
			                    {
			                        "timestamp": 1667177547324,
			                        "value": "[]"
			                    }
			                ],
			                "startTime": "2022-10-31T01:30:57Z",
			                "unit": "m"
			            }
			        ]
			    }
			}
	*/
	var data OBI
	if d, ok := cacheName.Get(cacheKey); ok {
		data, ok = d.(OBI)
		if !ok {
			klog.V(5).ErrorS(errors.New("get data err"), ManagerLogPrefix+"get data err")
			data = OBI{Metric: make(map[string]FullMetrics)}
		}
	} else {
		data = OBI{Metric: make(map[string]FullMetrics)}
	}
	metrics := obi.Status.Metrics
	for metricType, metricInfo := range metrics {
		// metricType cpu mem ...
		if _, exist := (data.Metric)[metricType]; !exist {
			(data.Metric)[metricType] = FullMetrics{}
		}
		v := (data.Metric)[metricType]
		if len(metricInfo) == 0 {
			continue
		}
		v.ObservabilityIndicantStatusMetricInfo = *metricInfo[0].DeepCopy()
		if len(v.ObservabilityIndicantStatusMetricInfo.Records) == 0 {
			continue
		}
		v.Max, v.Min, v.Avg = 0, 0, 0
		var i int
		var sum float64
		for _, r := range v.Records {
			val, err := strconv.ParseFloat(r.Value, 64)
			if err != nil {
				klog.V(5).ErrorS(err, ManagerLogPrefix+"Failed to parse float", "Value", r.Value, "obi", klog.KObj(obi))
				continue
			}
			if val > v.Max {
				v.Max = val
			}
			if val < v.Min {
				v.Min = val
			}
			sum += val
			i++
		}
		v.Avg = sum / float64(i)
		(data.Metric)[metricType] = v
	}
	klog.V(5).InfoS("add obi to cache", "obi", klog.KObj(obi), "cacheKey", cacheKey)
	cacheName.Set(cacheKey, data, expiration)
}

func (mgr *manager) ObservabilityIndicantUpdate(old interface{}, new interface{}) {
	klog.V(5).Infoln(ManagerLogPrefix + "get update ObservabilityIndicant")
	mgr.ObservabilityIndicantAdd(new)
}

func (mgr *manager) ObservabilityIndicantDelete(obj interface{}) {
	klog.V(5).Infoln(ManagerLogPrefix + "get delete ObservabilityIndicant")
	obi, ok := obj.(*schedv1alpha1.ObservabilityIndicant)
	if !ok {
		klog.V(4).ErrorS(errors.New("cant convert to observability indicant"), ManagerLogPrefix+"cant convert to observability indicant", "obj", obj)
		return
	}
	switch {
	case IsResourceNode(obi.Spec.TargetRef):
		nodeName := obi.Spec.TargetRef.Name
		if nodeName == "" {
			return
		}
		delete(mgr.nodeMetric, nodeName)
	case IsResourcePod(obi.Spec.TargetRef):
		return
	default:
		return
	}
}

func getMetricCacheKey(obi *schedv1alpha1.ObservabilityIndicant) string {
	ns := obi.Namespace
	name := obi.Name
	return ns + "-" + name
}

func IsResourceNode(o schedv1alpha1.ObservabilityIndicantSpecTargetRef) bool {
	return o.Kind == "Node" && o.Group == v1.GroupName && o.Version == "v1"
}

func IsResourcePod(o schedv1alpha1.ObservabilityIndicantSpecTargetRef) bool {
	return o.Kind == "Pod" && o.Group == v1.GroupName && o.Version == "v1"
}

func SchedulerNamespace() string {
	// Assumes have set the POD_NAMESPACE environment variable using the downward API.
	if ns, ok := os.LookupEnv("POD_NAMESPACE"); ok {
		return ns
	}
	// Fall back to the namespace associated with the service account token, if available
	if data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		if ns := strings.TrimSpace(string(data)); len(ns) > 0 {
			return ns
		}
	}
	// Fall back to scheduler most likely to exist ns system-ns
	return metav1.NamespaceSystem
}
