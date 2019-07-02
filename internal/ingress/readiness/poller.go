package readiness

import (
	"context"

	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/aws"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog"

	"strconv"
	// "strings"
	"sync"
)

const (
	healthyState = "HEALTHY"
)

// tgMeta references a AWS target group resource
type tgMeta struct {
	SyncerKey TGSyncKey
	// ARN is the arn of the tg
	ARN string
}

// func (n tgMeta) String() string {
// 	return fmt.Sprintf("%s-%s-%s", n.SyncerKey.String(), n.Name, n.Zone)
// }

// podStatusPatcher interface allows patching pod status
type podStatusPatcher interface {
	// syncPod patches the target condition in the pod status to be True.
	// key is the key to the pod. It is the namespaced name in the format of "namespace/name"
	// tgArn is the name of the Target Group resource
	syncPod(key, tgArn string) error
}

// pollTarget is the target for polling
type pollTarget struct {
	// endpointMap maps network endpoint to namespaced name of pod
	endpointMap EndpointPodMap
	// polling indicates if the tg is being polled
	polling bool
}

// poller tracks the tgs and corresponding targets needed to be polled.
type poller struct {
	lock sync.Mutex
	// pollMap contains tgs and corresponding targets needed to be polled.
	// all operations(read, write) to the pollMap are lock protected.
	pollMap map[tgMeta]*pollTarget

	podLister cache.Indexer
	lookup    TGLookup
	patcher   podStatusPatcher
	cloud     aws.CloudAPI
}

func NewPoller(podLister cache.Indexer, lookup TGLookup, patcher podStatusPatcher, cloud aws.CloudAPI) *poller {
	return &poller{
		pollMap:   make(map[tgMeta]*pollTarget),
		podLister: podLister,
		lookup:    lookup,
		patcher:   patcher,
		cloud:     cloud,
	}
}

// RegisterEndpoints registered the endpoints that needed to be poll for the TG with lock
func (p *poller) RegisterEndpoints(key tgMeta, endpointMap EndpointPodMap) {
	p.lock.Lock()
	defer p.lock.Unlock()
	p.registerEndpoints(key, endpointMap)
}

// registerEndpoints registered the endpoints that needed to be poll for the TG
// It returns false if there is no endpoints needed to be polled, returns true if otherwise.
// Assumes p.lock is held when calling this method.
func (p *poller) registerEndpoints(key tgMeta, endpointMap EndpointPodMap) bool {
	endpointsToPoll := needToPoll(key.SyncerKey, endpointMap, p.lookup, p.podLister)
	if len(endpointsToPoll) == 0 {
		delete(p.pollMap, key)
		return false
	}

	if v, ok := p.pollMap[key]; ok {
		v.endpointMap = endpointsToPoll
	} else {
		p.pollMap[key] = &pollTarget{endpointMap: endpointsToPoll}
	}
	return true
}

// ScanForWork returns the list of TGs that should be polled
func (p *poller) ScanForWork() []tgMeta {
	p.lock.Lock()
	defer p.lock.Unlock()
	var ret []tgMeta
	for key, target := range p.pollMap {
		if target.polling {
			continue
		}
		if p.registerEndpoints(key, target.endpointMap) {
			ret = append(ret, key)
		}
	}
	return ret
}

// Poll polls a TG and returns error plus whether retry is needed
// This function is threadsafe.
func (p *poller) Poll(key tgMeta) (retry bool, err error) {
	if !p.markPolling(key) {
		klog.V(4).Infof("TG %q as is already being polled or no longer needed to be polled.", key.ARN)
		return true, nil
	}
	defer p.unMarkPolling(key)

	var errList []error
	klog.V(2).Infof("polling TG %q", key.ARN)

	targets, err := p.cloud.GetTargetsForTargetGroupArn(context.TODO(), key.ARN)
	if err != nil {
		return true, err
	}

	// p.cloud

	// Traverse the response and check if the endpoints in interest are HEALTHY
	func() {
		p.lock.Lock()
		defer p.lock.Unlock()
		var healthyCount int
		for _, r := range targets {
			healthy, err := p.processHealthStatus(key, r)
			if healthy && err == nil {
				healthyCount++
			}
			if err != nil {
				errList = append(errList, err)
			}
		}
		if healthyCount != len(p.pollMap[key].endpointMap) {
			retry = true
		}
	}()
	return retry, utilerrors.NewAggregate(errList)
}

// processHealthStatus evaluates the health status of the input network endpoint.
// Assumes p.lock is held when calling this method.
func (p *poller) processHealthStatus(key tgMeta, healthState *elbv2.TargetHealthDescription) (healthy bool, err error) {
	ne := NetworkEndpoint{
		IP:   aws.StringValue(healthState.Target.Id),
		Port: strconv.FormatInt(aws.Int64Value(healthState.Target.Port), 10),
	}
	podName, ok := p.getPod(key, ne)
	if !ok {
		return false, nil
	}
	if aws.StringValue(healthState.TargetHealth.State) == elbv2.TargetHealthStateEnumHealthy {
		err := p.patcher.syncPod(keyFunc(podName.Namespace, podName.Name), key.ARN)
		return true, err
	}
	return false, nil
}

// getPod returns the namespaced name of a pod corresponds to an endpoint and whether the pod is registered
// Assumes p.lock is held when calling this method.
func (p *poller) getPod(key tgMeta, endpoint NetworkEndpoint) (namespacedName types.NamespacedName, exists bool) {
	t, ok := p.pollMap[key]
	if !ok {
		return types.NamespacedName{}, false
	}
	ret, ok := t.endpointMap[endpoint]
	return ret, ok
}

// markPolling returns true if the TG is successfully marked as polling
func (p *poller) markPolling(key tgMeta) bool {
	p.lock.Lock()
	defer p.lock.Unlock()
	t, ok := p.pollMap[key]
	if !ok {
		return false
	}
	if t.polling {
		return false
	}
	t.polling = true
	return true
}

// unMarkPolling unmarks the NEG
func (p *poller) unMarkPolling(key tgMeta) {
	p.lock.Lock()
	defer p.lock.Unlock()
	if t, ok := p.pollMap[key]; ok {
		t.polling = false
	}
}
