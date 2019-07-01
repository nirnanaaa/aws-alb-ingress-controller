package tg

import (
	v1 "k8s.io/api/core/v1"
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
	for _, cond := range pod.Status.Conditions {
		if cond.Type == conditionName {
			return true
		}
	}
	return false
}
