package controller

import (
	"context"
	"fmt"

	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/alb/lb"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/albctx"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/ingress/controller/store"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/ingress/metric"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/utils"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/pkg/util/log"

	corev1 "k8s.io/api/core/v1"
	extensions "k8s.io/api/extensions/v1beta1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// Reconciler reconciles an single ingress object
type Reconciler struct {
	client   kubernetes.Interface
	cache    cache.Cache
	recorder record.EventRecorder

	// TODO: move things out of store, and start to rely on functionality provided by client & cache
	store store.Storer

	lbController lb.Controller

	metricCollector metric.Collector
}

// Reconcile will reconcile the aws resources with k8s state of ingress.
func (r *Reconciler) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	ctx := context.Background()
	ingress := &extensions.Ingress{}
	if err := r.cache.Get(ctx, request.NamespacedName, ingress); err != nil {
		if !errors.IsNotFound(err) {
			r.metricCollector.IncReconcileErrorCount(request.NamespacedName.String())
			return reconcile.Result{}, err
		}

		if err := r.deleteIngress(ctx, request.NamespacedName); err != nil {
			r.metricCollector.IncReconcileErrorCount(request.NamespacedName.String())
			return reconcile.Result{}, err
		}

		r.metricCollector.IncReconcileCount()
		return reconcile.Result{}, nil
	}

	if err := r.reconcileIngress(ctx, request.NamespacedName, ingress); err != nil {
		r.metricCollector.IncReconcileErrorCount(request.NamespacedName.String())
		return reconcile.Result{}, err
	}

	r.metricCollector.IncReconcileCount()
	return reconcile.Result{}, nil
}

func (r *Reconciler) reconcileIngress(ctx context.Context, ingressKey types.NamespacedName, ingress *extensions.Ingress) error {
	ctx = r.buildReconcileContext(ctx, ingressKey, ingress)
	lbInfo, err := r.lbController.Reconcile(ctx, ingress)
	if err != nil {
		return err
	}
	if err := r.updateIngressStatus(ctx, ingress, lbInfo); err != nil {
		return err
	}

	return nil
}

func (r *Reconciler) deleteIngress(ctx context.Context, ingressKey types.NamespacedName) error {
	ctx = r.buildReconcileContext(ctx, ingressKey, nil)
	if err := r.lbController.Delete(ctx, ingressKey); err != nil {
		return err
	}
	return nil
}

func (r *Reconciler) updateIngressStatus(ctx context.Context, ingress *extensions.Ingress, lbInfo *lb.LoadBalancer) error {
	if len(ingress.Status.LoadBalancer.Ingress) != 1 ||
		ingress.Status.LoadBalancer.Ingress[0].IP != "" ||
		ingress.Status.LoadBalancer.Ingress[0].Hostname != lbInfo.DNSName {
		oldStatus := ingress.Status.DeepCopy()
		ingress.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{
			{
				Hostname: lbInfo.DNSName,
			},
		}
		patchBytes, err := preparePatchBytesforIngressStatus(*oldStatus, ingress.Status)
		if err != nil {
			return fmt.Errorf("failed to prepare patch bytes for ingress %v: %v", ingress, err)
		}
		_, _, err = patchIngressStatus(r.client, ingress.Namespace, ingress.Name, patchBytes)
		return err
	}
	return nil
}

// patchIngressStatus patches pod status with given patchBytes
func patchIngressStatus(c kubernetes.Interface, namespace, name string, patchBytes []byte) (*extensions.Ingress, []byte, error) {
	updatedIngress, err := c.Extensions().Ingresses(namespace).Patch(name, types.StrategicMergePatchType, patchBytes, "status")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to patch status %q for ingress %q/%q: %v", patchBytes, namespace, name, err)
	}
	return updatedIngress, patchBytes, nil
}

// preparePatchBytesforIngressStatus generates patch bytes based on the old and new ingress status
func preparePatchBytesforIngressStatus(oldIngressStatus, newIngressStatus extensions.IngressStatus) ([]byte, error) {
	patchBytes, err := utils.StrategicMergePatchBytes(extensions.Ingress{Status: oldIngressStatus}, extensions.Ingress{Status: newIngressStatus}, extensions.Ingress{})
	return patchBytes, err
}

func (r *Reconciler) buildReconcileContext(ctx context.Context, ingressKey types.NamespacedName, ingress *extensions.Ingress) context.Context {
	ctx = albctx.SetLogger(ctx, log.New(ingressKey.String()))
	if ingress != nil {
		ctx = albctx.SetEventf(ctx, func(eventType string, reason string, messageFmt string, args ...interface{}) {
			r.recorder.Eventf(ingress, eventType, reason, messageFmt, args...)
		})
	}
	return ctx
}
