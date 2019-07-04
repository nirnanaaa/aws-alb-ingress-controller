package readiness

import (
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// TGReadinessGate defines a readiness gate
const TGReadinessGate = "target-health.alb.ingress.kubernetes.io/load-balancer-tg-ready"

// TGSyncerKey includes information to uniquely identify a targetgroup syncer
type TGSyncerKey struct {
	// Namespace of service
	Namespace string
	// Name of service
	Name string
	// Service port
	Port int32
	// Service target port
	TargetPort string
}

// Target contains the essential information for each target in a target group
type Target struct {
	IP   string
	Port string
}

// TargetPodMap is a map from targets to a namespaced name of a pod
type TargetPodMap map[Target]types.NamespacedName

// Reflector defines the interaction between readiness reflector and other Target controller components
type Reflector interface {
	// Run starts the reflector.
	// Closing stopCh will signal the reflector to stop running.
	Run(stopCh <-chan struct{})
	// SyncPod signals the reflector to evaluate pod and patch pod status if needed.
	SyncPod(pod *v1.Pod)
	// CommitPods signals the reflector that pods has been added to a TG and it is time to poll the Target health status
	// syncerKey is the key to uniquely identify the Target syncer
	// tgArn is the ARN of the load target group
	// targetMap contains mapping from all network targets to pods which have been added into the NEG
	CommitPods(syncerKey TGSyncerKey, tgArn string, targetMap TargetPodMap)
}

// TGLookup defines an interface for looking up pod membership.
type TGLookup interface {
	// ReadinessGateEnabledTGs returns a list of targetgroups which has readiness gate
	// enabled for the pod's namespace and labels.
	ReadinessGateEnabledTGs(namespace string, labels map[string]string) []string

	// ReadinessGateEnabled returns true if the Pod requires readiness feedback
	ReadinessGateEnabled(syncerKey TGSyncerKey) bool
}

// NoopReflector is a mock reflector implementation
type NoopReflector struct{}

// Run is part of a mock implementation of the Reflector
func (*NoopReflector) Run(<-chan struct{}) {}

// SyncPod is part of a mock implementation of the Reflector
func (*NoopReflector) SyncPod(*v1.Pod) {}

// CommitPods is part of a mock implementation of the Reflector
func (*NoopReflector) CommitPods(TGSyncerKey, string, string, TargetPodMap) {}
