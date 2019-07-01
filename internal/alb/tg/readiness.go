package tg

import (
	"fmt"

	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/utils"
	api "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	clientset "k8s.io/client-go/kubernetes"
)

// GetReadinessConditionStatus return (cond, true) if condition exists, otherwise (_, false)
func GetReadinessConditionStatus(pod *v1.Pod, conditionName v1.PodConditionType) (condition v1.PodCondition, exists bool) {
	if pod == nil {
		return v1.PodCondition{}, false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == conditionName {
			return condition, true
		}
	}
	return v1.PodCondition{}, false
}

// SetReadinessConditionStatus sets the status of the NEG readiness condition
func SetReadinessConditionStatus(pod *v1.Pod, conditionName v1.PodConditionType, condition v1.PodCondition) {
	if pod == nil {
		return
	}
	for i, cond := range pod.Status.Conditions {
		if cond.Type == conditionName {
			pod.Status.Conditions[i] = condition
			return
		}
	}
	pod.Status.Conditions = append(pod.Status.Conditions, condition)
}

func ReadinessGateEnabled(pod *v1.Pod, conditionName v1.PodConditionType) bool {
	if pod == nil {
		return false
	}
	for _, cond := range pod.Spec.ReadinessGates {
		if cond.ConditionType == conditionName {
			return true
		}
	}
	return false
}

func getPodReadyState(pod *v1.Pod, healthState string) v1.ConditionStatus {
	if healthState == elbv2.TargetHealthStateEnumHealthy {
		return v1.ConditionTrue
	}
	if healthState == elbv2.TargetHealthStateEnumUnhealthy {
		return v1.ConditionFalse
	}
	return v1.ConditionUnknown
}

// patchPodStatus patches pod status with given patchBytes
func patchPodStatus(c clientset.Interface, namespace, name string, patchBytes []byte) (*api.Pod, []byte, error) {
	updatedPod, err := c.CoreV1().Pods(namespace).Patch(name, types.StrategicMergePatchType, patchBytes, "status")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to patch status %q for pod %q/%q: %v", patchBytes, namespace, name, err)
	}
	return updatedPod, patchBytes, nil
}

// preparePatchBytesforPodStatus generates patch bytes based on the old and new pod status
func preparePatchBytesforPodStatus(oldPodStatus, newPodStatus api.PodStatus) ([]byte, error) {
	patchBytes, err := utils.StrategicMergePatchBytes(api.Pod{Status: oldPodStatus}, api.Pod{Status: newPodStatus}, api.Pod{})
	return patchBytes, err
}
