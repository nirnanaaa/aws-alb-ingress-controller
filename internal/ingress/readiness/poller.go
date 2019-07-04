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

// tgMeta references an AWS target group resource
type tgMeta struct {
	SyncerKey TGSyncKey
	// ARN is the arn of the tg
	ARN string
}

func (n tgMeta) String() string {
	return fmt.Sprintf("%s-%s", n.SyncerKey.String(), n.ARN)
}

// podStatusPatcher interface allows patching pod status
type podStatusPatcher interface {
	// syncPod patches the target condition in the pod status to be True.
	// key is the key to the pod. It is the namespaced name in the format of "namespace/name"
	// tgARN is the name of the Target Group resource
	syncPod(key, tgARN string) error
}

// pollTarget is the target for polling
type pollTarget struct {
	// targetMap maps targets to namespaced name of pod
	targetMap TargetPodMap
	// polling indicates if the tg is being polled
	polling bool
}

// poller tracks the tgs and corresponding targets needed to be polled.
type poller struct {
	lock sync.Mutex
	// pollMap contains targetgroups and corresponding targets needed to be polled.
	// all operations(read, write) to the pollMap are lock protected.
	pollMap map[tgMeta]*pollTarget

	podLister cache.Indexer
	lookup    TGLookup
	patcher   podStatusPatcher
	cloud     aws.CloudAPI
}

// NewPoller creates a poller
func NewPoller(podLister cache.Indexer, lookup TGLookup, patcher podStatusPatcher, cloud aws.CloudAPI) *poller {
	return &poller{
		pollMap:   make(map[tgMeta]*pollTarget),
		podLister: podLister,
		lookup:    lookup,
		patcher:   patcher,
		cloud:     cloud,
	}
}

// RegisterTargets registers the targets that need to be polled for the TG with lock
func (p *poller) RegisterTargets(key tgMeta, targetMap TargetPodMap) {
	p.lock.Lock()
	defer p.lock.Unlock()
	p.registerTargets(key, targetMap)
}

// registerTargets registers the targets that need to be polled for the TG.
// It returns false if there are no targets in need of polling, returns true if otherwise.
// Assumes p.lock is held when calling this method.
func (p *poller) registerTargets(tg tgMeta, targetMap TargetPodMap) bool {
	targetsToPoll := needToPoll(key.SyncerKey, targetMap, p.lookup, p.podLister)
	if len(targetsToPoll) == 0 {
		delete(p.pollMap, tg)
		return false
	}

	if v, ok := p.pollMap[tg]; ok {
		v.targetMap = targetsToPoll
	} else {
		p.pollMap[tg] = &pollTarget{targetMap: targetsToPoll}
	}
	return true
}

// ScanForWork returns the list of TGs that should be polled
func (p *poller) ScanForWork() []tgMeta {
	p.lock.Lock()
	defer p.lock.Unlock()
	var ret []tgMeta
	for tg, pollTarget := range p.pollMap {
		if pollTarget.polling {
			continue
		}
		if p.registerTargets(tg, pollTarget.targetMap) {
			ret = append(ret, tg)
		}
	}
	return ret
}

// Poll polls a TG and returns an error and if a retry is needed
// This function is threadsafe.
func (p *poller) Poll(tg tgMeta) (retry bool, err error) {
	if !p.markPolling(tg) {
		klog.V(4).Infof("TG %q as is already being polled or no longer needed to be polled.", tg.ARN)
		return true, nil
	}
	defer p.unMarkPolling(tg)

	var errList []error
	klog.V(2).Infof("polling TG %q", key.ARN)

	targets, err := p.cloud.GetTargetsForTargetGroupArn(context.TODO(), key.ARN)
	if err != nil {
		return true, err
	}

	// Traverse the response and check if the targets of interest are HEALTHY
	func() {
		p.lock.Lock()
		defer p.lock.Unlock()
		var healthyCount int
		for _, r := range targets {
			healthy, err := p.processHealthStatus(tg, r)
			if healthy && err == nil {
				healthyCount++
			}
			if err != nil {
				errList = append(errList, err)
			}
		}
		if healthyCount != len(p.pollMap[tg].targetMap) {
			retry = true
		}
	}()
	return retry, utilerrors.NewAggregate(errList)
}

// processHealthStatus evaluates the health status of the targetgroup target.
// Assumes p.lock is held when calling this method.
func (p *poller) processHealthStatus(tg tgMeta, healthState *elbv2.TargetHealthDescription) (healthy bool, err error) {
	target := Target{
		IP:   aws.StringValue(healthState.Target.Id),
		Port: strconv.FormatInt(aws.Int64Value(healthState.Target.Port), 10),
	}
	podName, ok := p.getPod(tg, target)
	if !ok {
		return false, nil
	}
	if aws.StringValue(healthState.TargetHealth.State) == elbv2.TargetHealthStateEnumHealthy {
		err := p.patcher.syncPod(keyFunc(podName.Namespace, podName.Name), tg.ARN)
		return true, err
	}
	return false, nil
}

// getPod checks if the namespaced name of a pod corresponds to a target and whether the pod is registered.
// Assumes p.lock is held when calling this method.
func (p *poller) getPod(tg tgMeta, target Target) (namespacedName types.NamespacedName, exists bool) {
	t, ok := p.pollMap[tg]
	if !ok {
		return types.NamespacedName{}, false
	}
	ret, ok := t.targetMap[target]
	return ret, ok
}

// markPolling returns true if the TG is successfully marked as polling
func (p *poller) markPolling(tg tgMeta) bool {
	p.lock.Lock()
	defer p.lock.Unlock()
	t, ok := p.pollMap[tg]
	if !ok {
		return false
	}
	if t.polling {
		return false
	}
	t.polling = true
	return true
}

// unMarkPolling unmarks the targetgroup
func (p *poller) unMarkPolling(tg tgMeta) {
	p.lock.Lock()
	defer p.lock.Unlock()
	if t, ok := p.pollMap[tg]; ok {
		t.polling = false
	}
}
