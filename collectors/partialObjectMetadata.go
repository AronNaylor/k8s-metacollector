// Copyright 2023 The Falco Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package collectors

import (
	"context"
	"encoding/json"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8sApiErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/alacuku/k8s-metadata/broker"
	"github.com/alacuku/k8s-metadata/pkg/events"
	"github.com/alacuku/k8s-metadata/pkg/fields"
	"github.com/alacuku/k8s-metadata/pkg/resource"
)

// ObjectMetaCollector collects resources' metadata, puts them in a local cache and generates appropriate
// events when such resources change over time.
type ObjectMetaCollector struct {
	client.Client
	queue broker.Queue
	cache *events.GenericCache
	// externalSource watched for events that trigger the reconcile. In some cases changes in
	// other resources triggers the current resource. For example, when a pod is created we need to trigger the namespace
	// where the pod lives in order to send also the namespace to the node where the pod is running.
	externalSource source.Source
	// name of the collector, used in the logger.
	name string
	// subscriberChan where the collector gets notified of new subscribers and dispatches the existing events through the queue.
	subscriberChan <-chan string
	logger         logr.Logger
	// The GVK for the resource need to be set.
	resource *metav1.PartialObjectMetadata
	// podMatchingFields returns a list options used to list existing pods previously indexed on a field.
	podMatchingFields func(metadata *metav1.ObjectMeta) client.ListOption
	// generatedEventMetrics tracks the number of events generated by the collector and sent to subscribers.
	generatedEventsMetrics
}

// NewObjectMetaCollector returns a new meta collector for a given resource kind.
func NewObjectMetaCollector(cl client.Client, queue broker.Queue, cache *events.GenericCache,
	res *metav1.PartialObjectMetadata, name string, opt ...ObjectMetaOption) *ObjectMetaCollector {
	opts := objectMetaOptions{
		podMatchingFields: func(meta *metav1.ObjectMeta) client.ListOption {
			return &client.ListOptions{}
		},
	}
	for _, o := range opt {
		o(&opts)
	}

	return &ObjectMetaCollector{
		Client:                 cl,
		queue:                  queue,
		cache:                  cache,
		externalSource:         opts.externalSource,
		name:                   name,
		subscriberChan:         opts.subscriberChan,
		resource:               res,
		podMatchingFields:      opts.podMatchingFields,
		generatedEventsMetrics: newGeneratedEventsMetcrics(name),
	}
}

//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch

// Reconcile generates events to be sent to nodes when changes are detected for the watched resources.
func (r *ObjectMetaCollector) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var err error
	var res *events.Resource
	var ok, deleted bool

	logger := log.FromContext(ctx)

	err = r.Get(ctx, req.NamespacedName, r.resource)
	if err != nil && !k8sApiErrors.IsNotFound(err) {
		logger.Error(err, "unable to get resource")
		return ctrl.Result{}, err
	}

	if k8sApiErrors.IsNotFound(err) {
		// When the k8s resource get deleted we need to remove it from the local cache.
		if _, ok = r.cache.Get(req.String()); ok {
			logger.Info("marking resource for deletion")
			deleted = true
		} else {
			return ctrl.Result{}, nil
		}
	}

	logger.V(5).Info("resource found")

	// Get all the nodes to which this resource is related.
	// The currentNodes are used to compute to which nodes we need to send an event
	// and of which type, Added, Deleted or Modified.
	currentNodes, err := r.Nodes(ctx, logger, &r.resource.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Check if the resource has already been cached.
	if res, ok = r.cache.Get(req.String()); !ok {
		// If first time, then we just create a new cache entry for it.
		logger.V(3).Info("never met this resource in my life")
		res = events.NewResource(r.resource.Kind, string(r.resource.UID))
	}

	// The resource has been created, or updated. Compute if we need to propagate events.
	// The outcome is saved internally to the resource. See AddNodes method for more info.
	if !deleted {
		if err := r.ObjFieldsHandler(logger, res, r.resource); err != nil {
			return ctrl.Result{}, err
		}
		// We need to know if the mutable fields has been changed. That's why AddNodes accepts
		// a bool. Otherwise, we can not tell if nodes need an "Added" event or a "Modified" one.
		res.AddNodes(currentNodes.ToSlice())
	} else {
		// If the resource has been deleted from the api-server, then we send a "Deleted" event to all nodes
		nodes := res.GetNodes()
		res.DeleteNodes(nodes.ToSlice())
	}

	// At this point our resource has all the necessary bits to know for each node which type of events need to be sent.
	evts := res.ToEvents()

	// Enqueue events.
	for _, evt := range evts {
		if evt == nil {
			continue
		}
		switch evt.Type() {
		case events.Added:
			// Perform actions for "Added" events.
			r.createCounter.Inc()
			// For each resource that generates an "Added" event, we need to add it to the cache.
			// Please keep in mind that Cache operations resets the state of the resource, such as
			// resetting the info needed to generate the events.
			r.cache.Add(req.String(), res)
		case events.Modified:
			// Run specific code for "Modified" events.
			r.updateCounter.Inc()
			r.cache.Update(req.String(), res)
		case events.Deleted:
			// Run specific code for "Deleted" events.
			r.deleteCounter.Inc()
			r.cache.Delete(req.String())
		}
		// Add event to the queue.
		r.queue.Push(evt)
	}

	return ctrl.Result{}, nil
}

// Start implements the runnable interface needed in order to handle the start/stop
// using the manager. It starts go routines needed by the collector to interact with the
// broker.
func (r *ObjectMetaCollector) Start(ctx context.Context) error {
	return dispatch(ctx, r.logger, r.subscriberChan, r.queue, r.cache)
}

// ObjFieldsHandler populates the evt from the object.
func (r *ObjectMetaCollector) ObjFieldsHandler(logger logr.Logger, evt *events.Resource, obj *metav1.PartialObjectMetadata) error {
	if obj == nil {
		return nil
	}

	objUn, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		logger.Error(err, "unable to convert to unstructured")
		return err
	}

	// Remove unused meta fields
	metaUnused := []string{"resourceVersion", "creationTimestamp", "deletionTimestamp",
		"ownerReferences", "finalizers", "generateName", "deletionGracePeriodSeconds"}
	meta := objUn["metadata"]
	metaMap := meta.(map[string]interface{})
	for _, key := range metaUnused {
		delete(metaMap, key)
	}

	metaString, err := json.Marshal(metaMap)
	if err != nil {
		return err
	}
	evt.SetMeta(string(metaString))

	return nil
}

// Nodes returns all the nodes where pods related to the current deployment are running.
func (r *ObjectMetaCollector) Nodes(ctx context.Context, logger logr.Logger, meta *metav1.ObjectMeta) (fields.Nodes, error) {
	pods := corev1.PodList{}
	err := r.List(ctx, &pods, client.InNamespace(meta.Namespace), r.podMatchingFields(meta))

	if err != nil {
		logger.Error(err, "unable to list pods related to resource", "in namespace", meta.Namespace)
		return nil, err
	}

	if len(pods.Items) == 0 {
		return nil, nil
	}

	nodes := make(map[string]struct{}, len(pods.Items))
	for i := range pods.Items {
		if pods.Items[i].Spec.NodeName != "" {
			nodes[pods.Items[i].Spec.NodeName] = struct{}{}
		}
	}

	return nodes, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ObjectMetaCollector) SetupWithManager(mgr ctrl.Manager) error {
	// Set the generic logger to be used in other function then the reconcile loop.
	r.logger = mgr.GetLogger().WithName(r.name)

	lc, err := newLogConstructor(mgr.GetLogger(), r.name, r.resource.Kind)
	if err != nil {
		return err
	}

	bld := ctrl.NewControllerManagedBy(mgr).
		For(r.resource,
			builder.OnlyMetadata,
			builder.WithPredicates(predicatesWithMetrics(r.name, apiServerSource, nil))).
		WithOptions(controller.Options{LogConstructor: lc})

	if r.externalSource != nil {
		bld.Watches(r.externalSource,
			&handler.EnqueueRequestForObject{},
			builder.WithPredicates(predicatesWithMetrics(r.name, resource.Pod, nil)))
	}

	return bld.Complete(r)
}

// GetName returns the name of the collector.
func (r *ObjectMetaCollector) GetName() string {
	return r.name
}

// NewPartialObjectMetadata returns a partial object metadata for a limited set of resources. It is used as a helper
// when triggering reconciles or instantiating a collector for a given resource.
func NewPartialObjectMetadata(kind string, name *types.NamespacedName) *metav1.PartialObjectMetadata {
	obj := &metav1.PartialObjectMetadata{}
	if kind == resource.Namespace || kind == resource.Service || kind == resource.ReplicationController {
		obj.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind(kind))
	} else {
		obj.SetGroupVersionKind(appsv1.SchemeGroupVersion.WithKind(kind))
	}

	if name != nil {
		obj.Name = name.Name
		obj.Namespace = name.Namespace
	}
	return obj
}
