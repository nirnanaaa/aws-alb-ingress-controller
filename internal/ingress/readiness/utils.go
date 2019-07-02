package readiness

import (
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/utils"
	"k8s.io/klog"
)

// ReadinessConditionStatus return (cond, true) if tg condition exists, otherwise (_, false)
func ReadinessConditionStatus(pod *v1.Pod) (condition v1.PodCondition, exists bool) {
	if pod == nil {
		return v1.PodCondition{}, false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == TGReadinessGate {
			return condition, true
		}
	}
	return v1.PodCondition{}, false
}

// evalReadinessGate returns if the pod readiness gate includes the tg readiness condition and the condition status is true
func evalReadinessGate(pod *v1.Pod) (ready bool, readinessGateExists bool) {
	if pod == nil {
		return false, false
	}
	for _, gate := range pod.Spec.ReadinessGates {
		if gate.ConditionType == TGReadinessGate {
			readinessGateExists = true
		}
	}
	if condition, ok := ReadinessConditionStatus(pod); ok {
		if condition.Status == v1.ConditionTrue {
			ready = true
		}
	}
	return ready, readinessGateExists
}

func keyFunc(namespace, name string) string {
	return fmt.Sprintf("%s/%s", namespace, name)
}

// getPodFromStore return (pod, exists, nil) if it is able to successfully retrieve it from podLister.
func getPodFromStore(podLister cache.Indexer, namespace, name string) (pod *v1.Pod, exists bool, err error) {
	if podLister == nil {
		return nil, false, fmt.Errorf("podLister is nil")
	}
	key := keyFunc(namespace, name)
	obj, exists, err := podLister.GetByKey(key)
	if err != nil {
		return nil, false, fmt.Errorf("failed to retrieve pod %q from store: %v", key, err)
	}

	if !exists {
		return nil, false, nil
	}

	pod, ok := obj.(*v1.Pod)
	if !ok {
		return nil, false, fmt.Errorf("Failed to convert obj type %T to *v1.Pod", obj)
	}
	return pod, true, nil
}

// SetReadinessConditionStatus sets the status of the TG readiness condition
func SetReadinessConditionStatus(pod *v1.Pod, condition v1.PodCondition) {
	if pod == nil {
		return
	}
	for i, cond := range pod.Status.Conditions {
		if cond.Type == TGReadinessGate {
			pod.Status.Conditions[i] = condition
			return
		}
	}
	pod.Status.Conditions = append(pod.Status.Conditions, condition)
}

// patchPodStatus patches pod status with given patchBytes
func patchPodStatus(c clientset.Interface, namespace, name string, patchBytes []byte) (*v1.Pod, []byte, error) {
	updatedPod, err := c.CoreV1().Pods(namespace).Patch(name, types.StrategicMergePatchType, patchBytes, "status")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to patch status %q for pod %q/%q: %v", patchBytes, namespace, name, err)
	}
	return updatedPod, patchBytes, nil
}

// preparePatchBytesforPodStatus generates patch bytes based on the old and new pod status
func preparePatchBytesforPodStatus(oldPodStatus, newPodStatus v1.PodStatus) ([]byte, error) {
	patchBytes, err := utils.StrategicMergePatchBytes(v1.Pod{Status: oldPodStatus}, v1.Pod{Status: newPodStatus}, v1.Pod{})
	return patchBytes, err
}

// needToPoll filter out the network endpoint that needs to be polled based on the following conditions:
// 1. tg syncer has readiness gate enabled
// 2. the pod exists
// 3. the pod has tg readiness gate
// 4. the pod's tg readiness condition is not True
func needToPoll(syncerKey TGSyncKey, endpointMap EndpointPodMap, lookup TGLookup, podLister cache.Indexer) EndpointPodMap {
	if !lookup.ReadinessGateEnabled(syncerKey) {
		return EndpointPodMap{}
	}
	removeIrrelevantEndpoints(endpointMap, podLister)
	return endpointMap
}

// removeIrrelevantEndpoints will filter out the endpoints that does not need health status polling from the input endpoint map
func removeIrrelevantEndpoints(endpointMap EndpointPodMap, podLister cache.Indexer) {
	for endpoint, namespacedName := range endpointMap {
		pod, exists, err := getPodFromStore(podLister, namespacedName.Namespace, namespacedName.Name)
		if err != nil {
			klog.Warningf("Failed to retrieve pod %q from store: %v", namespacedName.String(), err)
		}
		if err == nil && exists && needToProcess(pod) {
			continue
		}
		delete(endpointMap, endpoint)
	}
}

// needToProcess check if the pod needs to be processed by readiness reflector
// If pod has tg readiness gate and its condition is False, then return true.
func needToProcess(pod *v1.Pod) bool {
	if pod == nil {
		return false
	}
	conditionReady, readinessGateExists := evalReadinessGate(pod)
	return readinessGateExists && !conditionReady
}
