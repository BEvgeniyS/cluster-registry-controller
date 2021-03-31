// Copyright (c) 2021 Banzai Cloud Zrt. All Rights Reserved.

package controllers

import (
	"context"
	"fmt"
	"strings"

	"emperror.dev/errors"
	"github.com/go-logr/logr"
	"github.com/throttled/throttled"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/banzaicloud/cluster-registry-controller/pkg/clusters"
	"github.com/banzaicloud/cluster-registry-controller/pkg/util"
	clusterregistryv1alpha1 "github.com/banzaicloud/cluster-registry/api/v1alpha1"
	"github.com/banzaicloud/k8s-objectmatcher/patch"
	"github.com/banzaicloud/operator-tools/pkg/reconciler"
	"github.com/banzaicloud/operator-tools/pkg/resources"
)

const (
	OwnershipAnnotation   = "k8s.cisco.com/resource-owner-cluster-id"
	OriginalGVKAnnotation = "k8s.cisco.com/original-group-version-kind"
)

type syncReconciler struct {
	clusters.ManagedReconciler

	localClusterID  string
	localMgr        ctrl.Manager
	localRecorder   record.EventRecorder
	clustersManager *clusters.Manager
	rateLimiter     throttled.RateLimiter

	clusterID      string
	ctrl           controller.Controller
	queue          workqueue.RateLimitingInterface
	rule           *clusterregistryv1alpha1.ResourceSyncRule
	localInformers map[string]struct{}
}

type SyncReconcilerOption func(r *syncReconciler)

func WithRateLimiter(rateLimiter throttled.RateLimiter) SyncReconcilerOption {
	return func(r *syncReconciler) {
		r.rateLimiter = rateLimiter
	}
}

func NewSyncReconciler(name string, localMgr ctrl.Manager, rule *clusterregistryv1alpha1.ResourceSyncRule, log logr.Logger, clusterID string, clustersManager *clusters.Manager, opts ...SyncReconcilerOption) (SyncReconciler, error) {
	r := &syncReconciler{
		ManagedReconciler: clusters.NewManagedReconciler(name, log),

		localMgr:        localMgr,
		localRecorder:   localMgr.GetEventRecorderFor("cluster-controller"),
		clustersManager: clustersManager,
		rule:            rule,
		clusterID:       clusterID,
		localInformers:  make(map[string]struct{}),
	}

	for _, opt := range opts {
		opt(r)
	}

	return r, nil
}

func (r *syncReconciler) GetRule() *clusterregistryv1alpha1.ResourceSyncRule {
	return r.rule
}

func (r *syncReconciler) setLocalClusterID() error {
	if r.localClusterID == "" {
		localClusterID, err := GetClusterID(r.GetContext(), r.localMgr.GetClient())
		if err != nil {
			return errors.WrapIf(err, "could not get local cluster id")
		}
		r.localClusterID = string(localClusterID)
	}

	return nil
}

func (r *syncReconciler) parseReqWithGVK(req ctrl.Request) (ctrl.Request, schema.GroupVersionKind, error) {
	objectGVK := schema.GroupVersionKind{}

	parts := strings.SplitN(req.NamespacedName.Name, "|", 2)
	if len(parts) != 2 {
		return ctrl.Request{}, objectGVK, errors.NewWithDetails("could not parse request name", "request", req)
	}
	if gvk := util.ParseGVKFromString(parts[0]); gvk != nil {
		objectGVK = *gvk
	} else {
		return ctrl.Request{}, objectGVK, errors.NewWithDetails("could not parse group version kind", "gvk", req.NamespacedName.Name)
	}

	req.NamespacedName.Name = parts[1]

	return req, objectGVK, nil
}

func (r *syncReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	err := r.setLocalClusterID()
	if err != nil {
		return ctrl.Result{}, errors.WithStackIf(err)
	}

	req, objectGVK, err := r.parseReqWithGVK(req)
	if err != nil {
		return ctrl.Result{}, errors.WithStackIf(err)
	}

	log := r.GetLogger().WithValues("resource", req.NamespacedName, "gvk", objectGVK)

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(objectGVK)
	obj.SetName(req.Name)
	obj.SetNamespace(req.Namespace)

	log.Info("reconciling")

	err = r.GetManager().GetClient().Get(r.GetContext(), req.NamespacedName, obj)
	if apierrors.IsNotFound(err) {
		log.Info("object was removed, trying to delete")
		err := r.localMgr.GetClient().Delete(r.GetContext(), obj)
		if apierrors.IsNotFound(err) {
			err = nil
		}

		return ctrl.Result{}, err
	}
	if err != nil {
		return ctrl.Result{}, errors.WrapIf(err, "could not get object")
	}

	if r.rateLimiter != nil {
		limited, _, err := r.rateLimiter.RateLimit(req.String(), 1)
		if err != nil {
			return ctrl.Result{}, errors.WrapIf(err, "could not rate limit")
		}
		if limited {
			msg := "ratelimited, too frequent reconciles were happening for this object"
			r.localRecorder.Event(r.rule, corev1.EventTypeWarning, "ObjectReconcileRateLimited", fmt.Sprintf("%s (resource: %s)", msg, req))
			log.Info(msg)

			return ctrl.Result{}, nil
		}
	}

	ok, matchedRules, err := r.rule.Match(obj)
	if !ok {
		return ctrl.Result{}, nil
	}
	if err != nil {
		return ctrl.Result{}, errors.WrapIf(err, "could not match object")
	}

	rec := reconciler.NewGenericReconciler(
		r.localMgr.GetClient(),
		log,
		reconciler.ReconcilerOpts{
			EnableRecreateWorkloadOnImmutableFieldChange: true,
			Scheme: r.localMgr.GetScheme(),
		},
	)

	metaObj, err := meta.Accessor(obj)
	if err != nil {
		return ctrl.Result{}, errors.WrapIf(err, "could not get meta for object")
	}

	objAnnotations := metaObj.GetAnnotations()
	if objAnnotations == nil {
		objAnnotations = make(map[string]string)
	}

	for k, v := range matchedRules.GetMutationAnnotations().Add {
		objAnnotations[k] = v
	}

	for _, k := range matchedRules.GetMutationAnnotations().Remove {
		delete(objAnnotations, k)
	}

	objLabels := metaObj.GetLabels()
	if objLabels == nil {
		objLabels = make(map[string]string)
	}

	for k, v := range matchedRules.GetMutationLabels().Add {
		objLabels[k] = v
	}

	for _, k := range matchedRules.GetMutationLabels().Remove {
		delete(objLabels, k)
	}

	if objAnnotations[OwnershipAnnotation] == "" {
		objAnnotations[OwnershipAnnotation] = r.clusterID
	}

	if mutated, gvk := matchedRules.GetMutatedGVK(obj.GetObjectKind().GroupVersionKind()); mutated {
		objAnnotations[OriginalGVKAnnotation] = util.GVKToString(obj.GetObjectKind().GroupVersionKind())
		obj.SetGroupVersionKind(gvk)
	}

	delete(objAnnotations, patch.LastAppliedConfig)
	delete(objAnnotations, corev1.LastAppliedConfigAnnotation)
	metaObj.SetGeneration(0)
	metaObj.SetAnnotations(objAnnotations)
	metaObj.SetResourceVersion("")
	metaObj.SetUID("")
	metaObj.SetSelfLink("")
	metaObj.SetCreationTimestamp(metav1.Time{})

	metaObj.SetFinalizers(nil)

	gvk := resources.ConvertGVK(obj.GetObjectKind().GroupVersionKind())
	patchFunc, err := resources.PatchYAMLModifier(resources.K8SResourceOverlay{
		GVK:     &gvk,
		Patches: matchedRules.GetMutationOverrides(),
	}, resources.NewObjectParser(r.GetManager().GetScheme()))
	if err != nil {
		return ctrl.Result{}, errors.WrapIf(err, "could not get patch func for object")
	}

	patchedObject, err := patchFunc(obj)
	if err != nil {
		return ctrl.Result{}, errors.WrapIf(err, "could not patch object")
	}
	if obj, ok = patchedObject.(*unstructured.Unstructured); !ok {
		return ctrl.Result{}, errors.New("invalid object")
	}

	desiredState := &util.DynamicDesiredState{
		ShouldCreateFunc: func(desired runtime.Object) (bool, error) {
			metaObj, err := meta.Accessor(desired)
			if err != nil {
				return false, err
			}

			ownerClusterID := metaObj.GetAnnotations()[OwnershipAnnotation]
			if r.localClusterID != "" && r.localClusterID == ownerClusterID {
				return false, nil
			}

			return true, nil
		},
		ShouldUpdateFunc: func(current, desired runtime.Object) (bool, error) {
			metaObj, err := meta.Accessor(current)
			if err != nil {
				return false, err
			}

			ownerClusterID := metaObj.GetAnnotations()[OwnershipAnnotation]
			if ownerClusterID == "" || (r.clustersManager.GetAliveClustersByID()[ownerClusterID] != nil && r.clusterID != ownerClusterID) {
				return false, nil
			}

			return true, nil
		},
	}

	desiredObject := obj.DeepCopy()

	_, err = rec.ReconcileResource(obj, desiredState)
	if apierrors.IsAlreadyExists(errors.Cause(err)) {
		log.Info("object already exists, requeue")

		return ctrl.Result{
			Requeue: true,
		}, nil
	}
	if err != nil {
		return ctrl.Result{}, errors.WrapIf(err, "could not reconcile object")
	}
	log.Info("object reconciled")

	if r.rule.UID != "" {
		r.localRecorder.Event(r.rule, corev1.EventTypeNormal, "ObjectReconciled", fmt.Sprintf("object reconciled (resource: %s)", req))
	}

	if matchedRules.GetMutationSyncStatus() {
		err = r.localMgr.GetClient().Get(r.GetContext(), client.ObjectKey{
			Name:      obj.GetName(),
			Namespace: obj.GetNamespace(),
		}, obj)
		if err != nil {
			return ctrl.Result{}, errors.WrapIf(err, "could not get object")
		}
		desiredObject.SetResourceVersion(obj.GetResourceVersion())
		err = r.localMgr.GetClient().Status().Update(r.GetContext(), desiredObject)
		if err != nil {
			return ctrl.Result{}, errors.WrapIf(err, "could not update object status")
		}
	}

	err = r.initLocalInformer(r.GetContext(), desiredObject)
	if err != nil {
		return ctrl.Result{}, errors.WithStackIf(err)
	}

	return ctrl.Result{}, nil
}

func (r *syncReconciler) setQueue(q workqueue.RateLimitingInterface) {
	r.queue = q
}

func (r *syncReconciler) SetupWithController(ctx context.Context, ctrl controller.Controller) error {
	err := r.ManagedReconciler.SetupWithController(ctx, ctrl)
	if err != nil {
		return err
	}

	gvk := schema.GroupVersionKind(r.rule.Spec.GVK)

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)

	err = ctrl.Watch(&source.Kind{
		Type: obj,
	}, &handler.EnqueueRequestsFromMapFunc{
		ToRequests: handler.ToRequestsFunc(func(obj handler.MapObject) []reconcile.Request {
			return []reconcile.Request{
				{
					NamespacedName: types.NamespacedName{
						Name:      fmt.Sprintf("%s|%s", util.GVKToString(obj.Object.GetObjectKind().GroupVersionKind()), obj.Meta.GetName()),
						Namespace: obj.Meta.GetNamespace(),
					},
				},
			}
		}),
	}, NewPredicateFuncs(func(meta metav1.Object, object runtime.Object, t string) bool {
		object.GetObjectKind().SetGroupVersionKind(gvk)
		ok, _, err := r.rule.Match(object)
		if err != nil {
			r.GetLogger().Error(err, "could not match object")

			return false
		}

		return ok
	}))
	if err != nil {
		return err
	}

	err = ctrl.Watch(&InMemorySource{
		reconciler: r,
	}, handler.Funcs{})
	if err != nil {
		return err
	}

	r.ctrl = ctrl

	return nil
}

func (r *syncReconciler) initLocalInformer(ctx context.Context, obj *unstructured.Unstructured) error {
	key := obj.GetObjectKind().GroupVersionKind().String()

	if _, ok := r.localInformers[key]; ok {
		return nil
	}

	r.GetLogger().Info("init local informer", "gvk", key)

	localInformer, err := r.localMgr.GetCache().GetInformer(ctx, obj)
	if err != nil {
		return errors.WrapIf(err, "could not create local informer for clusters")
	}

	err = r.ctrl.Watch(&source.Informer{
		Informer: localInformer,
	}, &handler.EnqueueRequestsFromMapFunc{
		ToRequests: handler.ToRequestsFunc(func(obj handler.MapObject) []reconcile.Request {
			gvk := obj.Object.GetObjectKind().GroupVersionKind()
			if ann := obj.Meta.GetAnnotations(); len(ann) > 0 && ann[OriginalGVKAnnotation] != "" {
				if originalGVK := util.ParseGVKFromString(ann[OriginalGVKAnnotation]); originalGVK != nil {
					gvk = *originalGVK
				}
			}

			return []reconcile.Request{
				{
					NamespacedName: types.NamespacedName{
						Name:      fmt.Sprintf("%s|%s", util.GVKToString(gvk), obj.Meta.GetName()),
						Namespace: obj.Meta.GetNamespace(),
					},
				},
			}
		}),
	}, r.localPredicate())
	if err != nil {
		return errors.WrapIf(err, "could not create watch for local informer")
	}

	r.localInformers[key] = struct{}{}

	return nil
}

func (r *syncReconciler) localPredicate() predicate.Funcs {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return false
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			_, ok := e.MetaOld.GetAnnotations()[OwnershipAnnotation]

			return ok
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			_, ok := e.Meta.GetAnnotations()[OwnershipAnnotation]

			return ok
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return false
		},
	}
}

func (r *syncReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	return r.ManagedReconciler.SetupWithManager(ctx, mgr)
}
