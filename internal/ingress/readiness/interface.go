package readiness

import (
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// TGReadinessGate defines a readiness gate
const TGReadinessGate = "target-health.alb.ingress.kubernetes.io/load-balancer-tg-ready"

type TGSyncKey struct {
	// Namespace of service
	Namespace string
	// Name of service
	Name string
	// Service port
	Port int32
	// Service target port
	TargetPort string
}

// NetworkEndpoint contains the essential information for each target in a target group
type NetworkEndpoint struct {
	IP   string
	Port string
}

// EndpointPodMap is a map from network endpoint to a namespaced name of a pod
type EndpointPodMap map[NetworkEndpoint]types.NamespacedName

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
	// endpointMap contains mapping from all network endpoints to pods which have been added into the NEG
	CommitPods(syncerKey TGSyncKey, tgArn string, endpointMap EndpointPodMap)
}

// TGLookup defines an interface for looking up pod membership.
type TGLookup interface {
	// ReadinessGateEnabled returns true if the Pod requires readiness feedback
	ReadinessGateEnabled(syncerKey TGSyncKey) bool
}

type NoopReflector struct{}

func (*NoopReflector) Run(<-chan struct{}) {}

func (*NoopReflector) SyncPod(*v1.Pod) {}

func (*NoopReflector) CommitPods(TGSyncKey, string, string, EndpointPodMap) {}