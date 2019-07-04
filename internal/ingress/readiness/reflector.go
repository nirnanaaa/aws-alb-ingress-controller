package readiness

import (
	"sync"
	"time"

	"fmt"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	unversionedcore "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"
	"reflect"
)

const (
	maxRetries       = 15
	tgReadyReason    = "LoadBalancerTGReady"
	tgNotReadyReason = "LoadBalancerTGNotReady"
)

// readinessReflector implements the Reflector interface
type readinessReflector struct {
	// podUpdateLock ensures that at any time there is only one
	podUpdateLock sync.Mutex
	client        kubernetes.Interface

	// pollerLock ensures there is only one poll action
	pollerLock sync.Mutex
	poller     *poller

	podLister cache.Indexer
	lookup    TGLookup

	eventBroadcaster record.EventBroadcaster
	eventRecorder    record.EventRecorder

	queue workqueue.RateLimitingInterface
}

func NewReadinessReflector(cc *context.ControllerContext, lookup TGLookup) Reflector {
	broadcaster := record.NewBroadcaster()
	broadcaster.StartLogging(klog.Infof)
	broadcaster.StartRecordingToSink(&unversionedcore.EventSinkImpl{
		Interface: cc.KubeClient.CoreV1().Events(""),
	})
	recorder := broadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "targetgroup-readiness-reflector"})
	reflector := &readinessReflector{
		client:           cc.KubeClient,
		podLister:        cc.PodInformer.GetIndexer(),
		lookup:           lookup,
		eventBroadcaster: broadcaster,
		eventRecorder:    recorder,
		queue:            workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
	}
	poller := NewPoller(cc.PodInformer.GetIndexer(), lookup, reflector, negtypes.NewAdapter(cc.Cloud))
	reflector.poller = poller
	return reflector
}

func (r *readinessReflector) Run(stopCh <-chan struct{}) {
	defer r.queue.ShutDown()
	klog.V(2).Infof("Starting TargetGroup readiness reflector")
	defer klog.V(2).Infof("Shutting down TargetGroup readiness reflector")

	go wait.Until(r.worker, time.Second, stopCh)
	<-stopCh
}

func (r *readinessReflector) worker() {
	for r.processNextWorkItem() {
	}
}

func (r *readinessReflector) processNextWorkItem() bool {
	key, quit := r.queue.Get()
	if quit {
		return false
	}
	defer r.queue.Done(key)

	err := r.syncPod(key.(string), "")
	r.handleErr(err, key)
	return true
}

// handleErr handles errors from syncPod
func (r *readinessReflector) handleErr(err error, key interface{}) {
	if err == nil {
		r.queue.Forget(key)
		return
	}

	if r.queue.NumRequeues(key) < maxRetries {
		klog.V(2).Infof("Error syncing pod %q, retrying. Error: %v", key, err)
		r.queue.AddRateLimited(key)
		return
	}

	klog.Warningf("Dropping pod %q out of the queue: %v", key, err)
	r.queue.Forget(key)
}

// syncPod process pod and patches the targetgroup readiness condition if needed.
// if targetgroup is specified, it means the pod is healthy in the targetgroup.
func (r *readinessReflector) syncPod(key string, targetgroup string) (err error) {
	// podUpdateLock to ensure there is no race in pod status update
	r.podUpdateLock.Lock()
	defer r.podUpdateLock.Unlock()

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	pod, exists, err := getPodFromStore(r.podLister, namespace, name)
	if err != nil {
		return err
	}
	if !exists {
		klog.V(5).Infof("Pod %q is no longer exists. Skipping", key)
		return nil
	}

	// This is to prevent if the pod got updated after being added to the queue
	if !needToProcess(pod) {
		return nil
	}

	klog.V(4).Infof("Syncing Pod %q", key)
	expectedCondition := v1.PodCondition{Type: tg.TGReadinessGate}
	var message, reason string

	if len(targetgroup) > 0 {
		expectedCondition.Status = v1.ConditionTrue
		reason = tgReadyReason
		message = fmt.Sprintf("Pod has become Healthy in TargetGroup %q. Marking condition %q to True.", targetgroup, tg.TGReadinessGate)
	} else {
		tgs := r.lookup.ReadinessGateEnabledTGs(pod.Namespace, pod.Labels)
		// mark pod as ready if it belongs to no targetgroups
		if len(tgs) == 0 {
			expectedCondition.Status = v1.ConditionTrue
			reason = tgReadyReason
			message = fmt.Sprintf("Pod does not belong to any targetgroup. Marking condition %q to True.", tg.TGReadinessGate)
		} else {
			// do not patch condition status in this case to prevent race condition:
			// 1. poller marks a pod ready
			// 2. syncPod gets call and does not retrieve the updated pod spec with true targetgroup readiness condition
			// 3. syncPod patches the targetgroup readiness condition to be false
			reason = tgNotReadyReason
			message = fmt.Sprintf("Waiting for pod to become healthy in at least one of the targetgroups: %v", tgs)
		}
	}
	expectedCondition.Reason = reason
	expectedCondition.Message = message
	return r.ensurePodTGCondition(pod, expectedCondition)
}

// SyncPod filter the pods that needed to be processed and put it into queue
func (r *readinessReflector) SyncPod(pod *v1.Pod) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(pod)
	if err != nil {
		klog.Errorf("Failed to generate pod key: %v", err)
		return
	}

	if !needToProcess(pod) {
		klog.V(6).Infof("Skip processing pod %q", key)
	}
	r.queue.Add(key)
}

// CommitPods registers the current targets in the targetgroup and starts polling them if needed
func (r *readinessReflector) CommitPods(syncerKey TGSyncerKey, tgARN string, targetMap TargetPodMap) {
	tg := tgMeta{
		SyncerKey: syncerKey,
		ARN:       tgARN,
	}
	r.poller.RegisterTargets(key, targetMap)
	r.poll()
}

// poll spins off go routines to poll targetgroups
func (r *readinessReflector) poll() {
	r.pollerLock.Lock()
	defer r.pollerLock.Unlock()
	for _, key := range r.poller.ScanForWork() {
		go r.pollTG(key)
	}
}

// pollTG polls a targetgroup
func (r *readinessReflector) pollTG(tg tgMeta) {
	klog.V(4).Infof("Polling targetgroup %q", key.String())
	retry, err := r.poller.Poll(tg)
	if err != nil {
		klog.Errorf("Failed to poll %q: %v", tg, err)
	}
	if retry {
		r.poll()
	}
}

// ensurePodTGCondition ensures the pod targetgroup condition is as expected.
// TODO: also populate lastTransitionTime in the condition
func (r *readinessReflector) ensurePodTGCondition(pod *v1.Pod, expectedCondition v1.PodCondition) error {
	// check if it is necessary to patch
	condition, ok := TGReadinessConditionStatus(pod)
	if ok && reflect.DeepEqual(expectedCondition, condition) {
		klog.V(4).Infof("Targetgroup condition for pod %s/%s is expected, skip patching", pod.Namespace, pod.Name)
		return nil
	}

	// calculate patch bytes, send patch and record event
	oldStatus := pod.Status.DeepCopy()
	SetTGReadinessConditionStatus(pod, expectedCondition)
	patchBytes, err := preparePatchBytesforPodStatus(*oldStatus, pod.Status)
	if err != nil {
		return fmt.Errorf("failed to prepare patch bytes for pod %v: %v", pod, err)
	}
	r.eventRecorder.Eventf(pod, v1.EventTypeNormal, expectedCondition.Reason, expectedCondition.Message)
	_, _, err = patchPodStatus(r.client, pod.Namespace, pod.Name, patchBytes)
	return err
}
