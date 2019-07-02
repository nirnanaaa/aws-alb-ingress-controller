package tg

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"strings"

	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/albctx"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/aws"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/ingress/backend"
	api "k8s.io/api/core/v1"
	extensions "k8s.io/api/extensions/v1beta1"
	"k8s.io/client-go/kubernetes"
)

// Targets contains the targets for a target group.
type Targets struct {
	// TgArn is the ARN of the target group
	TgArn string

	// Targets are the targets for the target group
	Targets []*elbv2.TargetDescription

	// TargetType is the type of targets, either ip or instance
	TargetType string

	// Ingress is the ingress for the targets
	Ingress *extensions.Ingress

	// Backend is the ingress backend for the targets
	Backend *extensions.IngressBackend
}

// NewTargets returns a new Targets pointer
func NewTargets(targetType string, ingress *extensions.Ingress, backend *extensions.IngressBackend) *Targets {
	return &Targets{
		TargetType: targetType,
		Ingress:    ingress,
		Backend:    backend,
	}
}

// TargetsController provides functionality to manage targets
type TargetsController interface {
	// Reconcile ensures the target group targets in AWS matches the targets configured in the ingress backend.
	Reconcile(context.Context, *Targets) error
}

// NewTargetsController constructs a new target group targets controller
func NewTargetsController(cloud aws.CloudAPI, endpointResolver backend.EndpointResolver, client kubernetes.Interface) TargetsController {
	return &targetsController{
		cloud:            cloud,
		endpointResolver: endpointResolver,
		client:           client,
	}
}

type targetsController struct {
	cloud            aws.CloudAPI
	endpointResolver backend.EndpointResolver
	client           kubernetes.Interface
}

func (c *targetsController) Reconcile(ctx context.Context, t *Targets) error {
	desiredTargets, err := c.endpointResolver.Resolve(t.Ingress, t.Backend, t.TargetType)
	if err != nil {
		return err
	}
	if t.TargetType == elbv2.TargetTypeEnumIp {
		err = c.populateTargetAZ(ctx, desiredTargets)
		if err != nil {
			return err
		}
	}
	currentHealth, currentTargets, err := c.getCurrentTargets(ctx, t.TgArn)
	if err != nil {
		return err
	}
	if t.TargetType == elbv2.TargetTypeEnumIp {
		// pods conditions reconciling is only implemented for target type == IP;
		// with target type == node, a 1:1 mapping between ALB target and pod is only possible if hostPort is used, which is discouraged
		pods, err := c.endpointResolver.ReverseResolve(t.Ingress, t.Backend, currentTargets)
		if err == nil {
			err = c.reconcilePodConditions(ctx, t.Ingress.Name, currentHealth, pods)
		}
		if err != nil {
			albctx.GetLogger(ctx).Errorf("Error reconsiling pod conditions for %v: %v", t.TgArn, err.Error())
			albctx.GetEventf(ctx)(api.EventTypeWarning, "ERROR", "Error reconciling pod conditions for target group %s: %s", t.TgArn, err.Error())
		}
	}
	additions, removals := targetChangeSets(currentTargets, desiredTargets)
	if len(additions) > 0 {
		albctx.GetLogger(ctx).Infof("Adding targets to %v: %v", t.TgArn, tdsString(additions))
		in := &elbv2.RegisterTargetsInput{
			TargetGroupArn: aws.String(t.TgArn),
			Targets:        additions,
		}

		if _, err := c.cloud.RegisterTargetsWithContext(ctx, in); err != nil {
			albctx.GetLogger(ctx).Errorf("Error adding targets to %v: %v", t.TgArn, err.Error())
			albctx.GetEventf(ctx)(api.EventTypeWarning, "ERROR", "Error adding targets to target group %s: %s", t.TgArn, err.Error())
			return err
		}
		// TODO add Add events ?
	}

	if len(removals) > 0 {
		albctx.GetLogger(ctx).Infof("Removing targets from %v: %v", t.TgArn, tdsString(removals))
		in := &elbv2.DeregisterTargetsInput{
			TargetGroupArn: aws.String(t.TgArn),
			Targets:        removals,
		}

		if _, err := c.cloud.DeregisterTargetsWithContext(ctx, in); err != nil {
			albctx.GetLogger(ctx).Errorf("Error removing targets from %v: %v", t.TgArn, err.Error())
			albctx.GetEventf(ctx)(api.EventTypeWarning, "ERROR", "Error removing targets from target group %s: %s", t.TgArn, err.Error())
			return err
		}
		// TODO add Delete events ?
	}
	t.Targets = desiredTargets
	return nil
}

// For each given pod, checks for the health status of the corresponding target in the target group and adds/updates a pod condition that can be used for pod readiness gates.
func (c *targetsController) reconcilePodConditions(ctx context.Context, ingressName string, targetsHealth []*elbv2.TargetHealthDescription, pods []*api.Pod) error {
	for i, pod := range pods {
		expectedCondition := api.PodCondition{Type: TGReadinessGate}
		if !ReadinessGateEnabled(pod) {
			continue
		}
		elbTargetHealth := targetsHealth[i].TargetHealth.State
		if elbTargetHealth == nil {
			albctx.GetLogger(ctx).Errorf("target has no health state")
			continue
		}
		healthState := *elbTargetHealth
		getPodReadyState(healthState, &expectedCondition)
		condition, ok := ReadinessConditionStatus(pod)
		if ok && reflect.DeepEqual(expectedCondition, condition) {
			continue
		}
		oldStatus := pod.Status.DeepCopy()
		SetReadinessConditionStatus(pod, expectedCondition)

		patchBytes, err := preparePatchBytesforPodStatus(*oldStatus, pod.Status)
		if err != nil {
			return fmt.Errorf("failed to prepare patch bytes for pod %v: %v", pod, err)
		}
		_, _, err = patchPodStatus(c.client, pod.Namespace, pod.Name, patchBytes)
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *targetsController) getCurrentTargets(ctx context.Context, TgArn string) ([]*elbv2.TargetHealthDescription, []*elbv2.TargetDescription, error) {
	opts := &elbv2.DescribeTargetHealthInput{TargetGroupArn: aws.String(TgArn)}
	resp, err := c.cloud.DescribeTargetHealthWithContext(ctx, opts)
	if err != nil {
		return nil, nil, err
	}

	var current []*elbv2.TargetDescription
	for _, thd := range resp.TargetHealthDescriptions {
		if aws.StringValue(thd.TargetHealth.State) == elbv2.TargetHealthStateEnumDraining {
			continue
		}
		current = append(current, thd.Target)
	}
	return resp.TargetHealthDescriptions, current, nil
}

func (c *targetsController) populateTargetAZ(ctx context.Context, a []*elbv2.TargetDescription) error {
	vpc, err := c.cloud.GetVpcWithContext(ctx)
	if err != nil {
		return err
	}
	cidrBlocks := make([]*net.IPNet, 0)
	for _, cidrBlockAssociation := range vpc.CidrBlockAssociationSet {
		_, ipv4Net, err := net.ParseCIDR(*cidrBlockAssociation.CidrBlock)
		if err != nil {
			return err
		}
		cidrBlocks = append(cidrBlocks, ipv4Net)
	}
	for i := range a {
		inVPC := false
		for _, cidrBlock := range cidrBlocks {
			if cidrBlock.Contains(net.ParseIP(*a[i].Id)) {
				inVPC = true
				break
			}
		}
		if !inVPC {
			a[i].AvailabilityZone = aws.String("all")
		}
	}
	return nil
}

// targetChangeSets compares b to a, returning a list of targets to add and remove from a to match b
func targetChangeSets(current, desired []*elbv2.TargetDescription) (add []*elbv2.TargetDescription, remove []*elbv2.TargetDescription) {
	currentMap := map[string]bool{}
	desiredMap := map[string]bool{}

	for _, i := range current {
		currentMap[tdString(i)] = true
	}
	for _, i := range desired {
		desiredMap[tdString(i)] = true
	}

	for _, i := range desired {
		if _, ok := currentMap[tdString(i)]; !ok {
			add = append(add, i)
		}
	}

	for _, i := range current {
		if _, ok := desiredMap[tdString(i)]; !ok {
			remove = append(remove, i)
		}
	}

	return add, remove
}

func tdString(td *elbv2.TargetDescription) string {
	return fmt.Sprintf("%v:%v", aws.StringValue(td.Id), aws.Int64Value(td.Port))
}

func tdsString(tds []*elbv2.TargetDescription) string {
	var s []string
	for _, td := range tds {
		s = append(s, tdString(td))
	}
	return strings.Join(s, ", ")
}
