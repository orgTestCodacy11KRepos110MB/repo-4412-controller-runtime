/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cache

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"

	"sigs.k8s.io/controller-runtime/pkg/cache/internal"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	logf "sigs.k8s.io/controller-runtime/pkg/internal/log"
)

var log = logf.RuntimeLog.WithName("object-cache")

// Cache knows how to load Kubernetes objects, fetch informers to request
// to receive events for Kubernetes objects (at a low-level),
// and add indices to fields on the objects stored in the cache.
type Cache interface {
	// Cache acts as a client to objects stored in the cache.
	client.Reader

	// Cache loads informers and adds field indices.
	Informers
}

// Informers knows how to create or fetch informers for different
// group-version-kinds, and add indices to those informers.  It's safe to call
// GetInformer from multiple threads.
type Informers interface {
	// GetInformer fetches or constructs an informer for the given object that corresponds to a single
	// API kind and resource.
	GetInformer(ctx context.Context, obj client.Object) (Informer, error)

	// GetInformerForKind is similar to GetInformer, except that it takes a group-version-kind, instead
	// of the underlying object.
	GetInformerForKind(ctx context.Context, gvk schema.GroupVersionKind) (Informer, error)

	// Start runs all the informers known to this cache until the context is closed.
	// It blocks.
	Start(ctx context.Context) error

	// WaitForCacheSync waits for all the caches to sync.  Returns false if it could not sync a cache.
	WaitForCacheSync(ctx context.Context) bool

	// Informers knows how to add indices to the caches (informers) that it manages.
	client.FieldIndexer
}

// Informer - informer allows you interact with the underlying informer.
type Informer interface {
	// AddEventHandler adds an event handler to the shared informer using the shared informer's resync
	// period.  Events to a single handler are delivered sequentially, but there is no coordination
	// between different handlers.
	// It returns a registration handle for the handler that can be used to remove
	// the handler again.
	AddEventHandler(handler toolscache.ResourceEventHandler) (toolscache.ResourceEventHandlerRegistration, error)
	// AddEventHandlerWithResyncPeriod adds an event handler to the shared informer using the
	// specified resync period.  Events to a single handler are delivered sequentially, but there is
	// no coordination between different handlers.
	// It returns a registration handle for the handler that can be used to remove
	// the handler again and an error if the handler cannot be added.
	AddEventHandlerWithResyncPeriod(handler toolscache.ResourceEventHandler, resyncPeriod time.Duration) (toolscache.ResourceEventHandlerRegistration, error)
	// RemoveEventHandler removes a formerly added event handler given by
	// its registration handle.
	// This function is guaranteed to be idempotent, and thread-safe.
	RemoveEventHandler(handle toolscache.ResourceEventHandlerRegistration) error
	// AddIndexers adds more indexers to this store.  If you call this after you already have data
	// in the store, the results are undefined.
	AddIndexers(indexers toolscache.Indexers) error
	// HasSynced return true if the informers underlying store has synced.
	HasSynced() bool
}

// ObjectSelector is an alias name of internal.Selector.
type ObjectSelector internal.Selector

// SelectorsByObject associate a client.Object's GVK to a field/label selector.
// There is also `DefaultSelector` to set a global default (which will be overridden by
// a more specific setting here, if any).
type SelectorsByObject map[client.Object]ObjectSelector

// Options are the optional arguments for creating a new InformersMap object.
type Options struct {
	// HTTPClient is the http client to use for the REST client
	HTTPClient *http.Client

	// Scheme is the scheme to use for mapping objects to GroupVersionKinds
	Scheme *runtime.Scheme

	// Mapper is the RESTMapper to use for mapping GroupVersionKinds to Resources
	Mapper meta.RESTMapper

	// ResyncEvery is the base frequency the informers are resynced.
	// Defaults to defaultResyncTime.
	// A 10 percent jitter will be added to the ResyncEvery period between informers
	// So that all informers will not send list requests simultaneously.
	ResyncEvery *time.Duration

	// View restricts the cache's ListWatch to the desired fields per GVK
	// Default watches all fields and all namespaces.
	View ViewOptions
}

// ViewOptions are the optional arguments for creating a cache view.
// A cache view restricts the Get(), List(), and internal ListWatch operations
// to the desired parameter.
type ViewOptions struct {
	// Namespaces restricts the cache's ListWatch to the desired namespaces
	// Default watches all namespaces
	Namespaces []string

	// DefaultSelector will be used as selectors for all object types
	// unless they have a more specific selector set in ByObject.
	DefaultSelector ObjectSelector

	// DefaultTransform will be used as transform for all object types
	// unless they have a more specific transform set in ByObject.
	DefaultTransform toolscache.TransformFunc

	// ByObject restricts the cache's ListWatch to the desired fields per GVK at the specified object.
	ByObject ViewByObject
}

// ViewByObject offers more fine-grained control over the cache's ListWatch by object.
type ViewByObject struct {
	// Selectors restricts the cache's ListWatch to the desired
	// fields per GVK at the specified object, the map's value must implement
	// Selectors [1] using for example a Set [2]
	// [1] https://pkg.go.dev/k8s.io/apimachinery/pkg/fields#Selectors
	// [2] https://pkg.go.dev/k8s.io/apimachinery/pkg/fields#Set
	Selectors SelectorsByObject

	// Transform is a map from objects to transformer functions which
	// get applied when objects of the transformation are about to be committed
	// to cache.
	//
	// This function is called both for new objects to enter the cache,
	// and for updated objects.
	Transform TransformByObject

	// UnsafeDisableDeepCopy indicates not to deep copy objects during get or
	// list objects per GVK at the specified object.
	// Be very careful with this, when enabled you must DeepCopy any object before mutating it,
	// otherwise you will mutate the object in the cache.
	UnsafeDisableDeepCopy DisableDeepCopyByObject
}

var defaultResyncTime = 10 * time.Hour

// New initializes and returns a new Cache.
func New(config *rest.Config, opts Options) (Cache, error) {
	opts, err := defaultOpts(config, opts)
	if err != nil {
		return nil, err
	}
	selectorsByGVK, err := convertToByGVK(opts.View.ByObject.Selectors, opts.View.DefaultSelector, opts.Scheme)
	if err != nil {
		return nil, err
	}
	disableDeepCopyByGVK, err := convertToDisableDeepCopyByGVK(opts.View.ByObject.UnsafeDisableDeepCopy, opts.Scheme)
	if err != nil {
		return nil, err
	}
	transformers, err := convertToByGVK(opts.View.ByObject.Transform, opts.View.DefaultTransform, opts.Scheme)
	if err != nil {
		return nil, err
	}
	transformByGVK := internal.TransformFuncByGVKFromMap(transformers)

	internalSelectorsByGVK := internal.SelectorsByGVK{}
	for gvk, selector := range selectorsByGVK {
		internalSelectorsByGVK[gvk] = internal.Selector(selector)
	}

	if len(opts.View.Namespaces) == 0 {
		opts.View.Namespaces = []string{metav1.NamespaceAll}
	}

	if len(opts.View.Namespaces) > 1 {
		return newMultiNamespaceCache(config, opts)
	}

	return &informerCache{
		scheme: opts.Scheme,
		Informers: internal.NewInformers(config, &internal.InformersOpts{
			HTTPClient:   opts.HTTPClient,
			Scheme:       opts.Scheme,
			Mapper:       opts.Mapper,
			ResyncPeriod: *opts.ResyncEvery,
			Namespace:    opts.View.Namespaces[0],
			ByGVK: internal.InformersOptsByGVK{
				Selectors:       internalSelectorsByGVK,
				DisableDeepCopy: disableDeepCopyByGVK,
				Transformers:    transformByGVK,
			},
		}),
	}, nil
}

// BuilderWithOptions returns a Cache constructor that will build a cache
// honoring the options argument, this is useful to specify options like
// SelectorsByObject
// WARNING: If SelectorsByObject is specified, filtered out resources are not
// returned.
// WARNING: If UnsafeDisableDeepCopy is enabled, you must DeepCopy any object
// returned from cache get/list before mutating it.
func BuilderWithOptions(options Options) NewCacheFunc {
	return func(config *rest.Config, inherited Options) (Cache, error) {
		var err error
		inherited, err = defaultOpts(config, inherited)
		if err != nil {
			return nil, err
		}
		options, err = defaultOpts(config, options)
		if err != nil {
			return nil, err
		}
		combined, err := options.inheritFrom(inherited)
		if err != nil {
			return nil, err
		}
		return New(config, *combined)
	}
}

func (options Options) inheritFrom(inherited Options) (*Options, error) {
	var (
		combined Options
		err      error
	)
	combined.Scheme = combineScheme(inherited.Scheme, options.Scheme)
	combined.Mapper = selectMapper(inherited.Mapper, options.Mapper)
	combined.ResyncEvery = selectResync(inherited.ResyncEvery, options.ResyncEvery)
	combined.View.Namespaces = selectNamespaces(inherited.View.Namespaces, options.View.Namespaces)
	combined.View.ByObject.Selectors, combined.View.DefaultSelector, err = combineSelectors(inherited, options, combined.Scheme)
	if err != nil {
		return nil, err
	}
	combined.View.ByObject.UnsafeDisableDeepCopy, err = combineUnsafeDeepCopy(inherited, options, combined.Scheme)
	if err != nil {
		return nil, err
	}
	combined.View.ByObject.Transform, combined.View.DefaultTransform, err = combineTransforms(inherited, options, combined.Scheme)
	if err != nil {
		return nil, err
	}
	return &combined, nil
}

func combineScheme(schemes ...*runtime.Scheme) *runtime.Scheme {
	var out *runtime.Scheme
	for _, sch := range schemes {
		if sch == nil {
			continue
		}
		for gvk, t := range sch.AllKnownTypes() {
			if out == nil {
				out = runtime.NewScheme()
			}
			out.AddKnownTypeWithName(gvk, reflect.New(t).Interface().(runtime.Object))
		}
	}
	return out
}

func selectMapper(def, override meta.RESTMapper) meta.RESTMapper {
	if override != nil {
		return override
	}
	return def
}

func selectResync(def, override *time.Duration) *time.Duration {
	if override != nil {
		return override
	}
	return def
}

func selectNamespaces(def, override []string) []string {
	if len(override) > 0 {
		return override
	}
	return def
}

func combineSelectors(inherited, options Options, scheme *runtime.Scheme) (SelectorsByObject, ObjectSelector, error) {
	// Selectors are combined via logical AND.
	//  - Combined label selector is a union of the selectors requirements from both sets of options.
	//  - Combined field selector uses fields.AndSelectors with the combined list of non-nil field selectors
	//    defined in both sets of options.
	//
	// There is a bunch of complexity here because we need to convert to SelectorsByGVK
	// to be able to match keys between options and inherited and then convert back to SelectorsByObject
	optionsSelectorsByGVK, err := convertToByGVK(options.View.ByObject.Selectors, options.View.DefaultSelector, scheme)
	if err != nil {
		return nil, ObjectSelector{}, err
	}
	inheritedSelectorsByGVK, err := convertToByGVK(inherited.View.ByObject.Selectors, inherited.View.DefaultSelector, inherited.Scheme)
	if err != nil {
		return nil, ObjectSelector{}, err
	}

	for gvk, inheritedSelector := range inheritedSelectorsByGVK {
		optionsSelectorsByGVK[gvk] = combineSelector(inheritedSelector, optionsSelectorsByGVK[gvk])
	}
	return convertToByObject(optionsSelectorsByGVK, scheme)
}

func combineSelector(selectors ...ObjectSelector) ObjectSelector {
	ls := make([]labels.Selector, 0, len(selectors))
	fs := make([]fields.Selector, 0, len(selectors))
	for _, s := range selectors {
		ls = append(ls, s.Label)
		fs = append(fs, s.Field)
	}
	return ObjectSelector{
		Label: combineLabelSelectors(ls...),
		Field: combineFieldSelectors(fs...),
	}
}

func combineLabelSelectors(ls ...labels.Selector) labels.Selector {
	var combined labels.Selector
	for _, l := range ls {
		if l == nil {
			continue
		}
		if combined == nil {
			combined = labels.NewSelector()
		}
		reqs, _ := l.Requirements()
		combined = combined.Add(reqs...)
	}
	return combined
}

func combineFieldSelectors(fs ...fields.Selector) fields.Selector {
	nonNil := fs[:0]
	for _, f := range fs {
		if f == nil {
			continue
		}
		nonNil = append(nonNil, f)
	}
	if len(nonNil) == 0 {
		return nil
	}
	if len(nonNil) == 1 {
		return nonNil[0]
	}
	return fields.AndSelectors(nonNil...)
}

func combineUnsafeDeepCopy(inherited, options Options, scheme *runtime.Scheme) (DisableDeepCopyByObject, error) {
	// UnsafeDisableDeepCopyByObject is combined via precedence. Only if a value for a particular GVK is unset
	// in options will a value from inherited be used.
	optionsDisableDeepCopyByGVK, err := convertToDisableDeepCopyByGVK(options.View.ByObject.UnsafeDisableDeepCopy, options.Scheme)
	if err != nil {
		return nil, err
	}
	inheritedDisableDeepCopyByGVK, err := convertToDisableDeepCopyByGVK(inherited.View.ByObject.UnsafeDisableDeepCopy, inherited.Scheme)
	if err != nil {
		return nil, err
	}

	for gvk, inheritedDeepCopy := range inheritedDisableDeepCopyByGVK {
		if _, ok := optionsDisableDeepCopyByGVK[gvk]; !ok {
			if optionsDisableDeepCopyByGVK == nil {
				optionsDisableDeepCopyByGVK = map[schema.GroupVersionKind]bool{}
			}
			optionsDisableDeepCopyByGVK[gvk] = inheritedDeepCopy
		}
	}
	return convertToDisableDeepCopyByObject(optionsDisableDeepCopyByGVK, scheme)
}

func combineTransforms(inherited, options Options, scheme *runtime.Scheme) (TransformByObject, toolscache.TransformFunc, error) {
	// Transform functions are combined via chaining. If both inherited and options define a transform
	// function, the transform function from inherited will be called first, and the transform function from
	// options will be called second.
	optionsTransformByGVK, err := convertToByGVK(options.View.ByObject.Transform, options.View.DefaultTransform, options.Scheme)
	if err != nil {
		return nil, nil, err
	}
	inheritedTransformByGVK, err := convertToByGVK(inherited.View.ByObject.Transform, inherited.View.DefaultTransform, inherited.Scheme)
	if err != nil {
		return nil, nil, err
	}

	for gvk, inheritedTransform := range inheritedTransformByGVK {
		if optionsTransformByGVK == nil {
			optionsTransformByGVK = map[schema.GroupVersionKind]toolscache.TransformFunc{}
		}
		optionsTransformByGVK[gvk] = combineTransform(inheritedTransform, optionsTransformByGVK[gvk])
	}
	return convertToByObject(optionsTransformByGVK, scheme)
}

func combineTransform(inherited, current toolscache.TransformFunc) toolscache.TransformFunc {
	if inherited == nil {
		return current
	}
	if current == nil {
		return inherited
	}
	return func(in interface{}) (interface{}, error) {
		mid, err := inherited(in)
		if err != nil {
			return nil, err
		}
		return current(mid)
	}
}

func defaultOpts(config *rest.Config, opts Options) (Options, error) {
	logger := log.WithName("setup")

	// Use the rest HTTP client for the provided config if unset
	if opts.HTTPClient == nil {
		var err error
		opts.HTTPClient, err = rest.HTTPClientFor(config)
		if err != nil {
			logger.Error(err, "Failed to get HTTP client")
			return opts, fmt.Errorf("could not create HTTP client from config: %w", err)
		}
	}

	// Use the default Kubernetes Scheme if unset
	if opts.Scheme == nil {
		opts.Scheme = scheme.Scheme
	}

	// Construct a new Mapper if unset
	if opts.Mapper == nil {
		var err error
		opts.Mapper, err = apiutil.NewDiscoveryRESTMapper(config, opts.HTTPClient)
		if err != nil {
			logger.Error(err, "Failed to get API Group-Resources")
			return opts, fmt.Errorf("could not create RESTMapper from config: %w", err)
		}
	}

	// Default the resync period to 10 hours if unset
	if opts.ResyncEvery == nil {
		opts.ResyncEvery = &defaultResyncTime
	}
	return opts, nil
}

func convertToByGVK[T any](byObject map[client.Object]T, def T, scheme *runtime.Scheme) (map[schema.GroupVersionKind]T, error) {
	byGVK := map[schema.GroupVersionKind]T{}
	for object, value := range byObject {
		gvk, err := apiutil.GVKForObject(object, scheme)
		if err != nil {
			return nil, err
		}
		byGVK[gvk] = value
	}
	byGVK[schema.GroupVersionKind{}] = def
	return byGVK, nil
}

func convertToByObject[T any](byGVK map[schema.GroupVersionKind]T, scheme *runtime.Scheme) (map[client.Object]T, T, error) {
	var byObject map[client.Object]T
	def := byGVK[schema.GroupVersionKind{}]
	for gvk, value := range byGVK {
		if gvk == (schema.GroupVersionKind{}) {
			continue
		}
		obj, err := scheme.New(gvk)
		if err != nil {
			return nil, def, err
		}
		cObj, ok := obj.(client.Object)
		if !ok {
			return nil, def, fmt.Errorf("object %T for GVK %q does not implement client.Object", obj, gvk)
		}
		if byObject == nil {
			byObject = map[client.Object]T{}
		}
		byObject[cObj] = value
	}
	return byObject, def, nil
}

// DisableDeepCopyByObject associate a client.Object's GVK to disable DeepCopy during get or list from cache.
type DisableDeepCopyByObject map[client.Object]bool

var _ client.Object = &ObjectAll{}

// ObjectAll is the argument to represent all objects' types.
type ObjectAll struct {
	client.Object
}

func convertToDisableDeepCopyByGVK(disableDeepCopyByObject DisableDeepCopyByObject, scheme *runtime.Scheme) (internal.DisableDeepCopyByGVK, error) {
	disableDeepCopyByGVK := internal.DisableDeepCopyByGVK{}
	for obj, disable := range disableDeepCopyByObject {
		switch obj.(type) {
		case ObjectAll, *ObjectAll:
			disableDeepCopyByGVK[internal.GroupVersionKindAll] = disable
		default:
			gvk, err := apiutil.GVKForObject(obj, scheme)
			if err != nil {
				return nil, err
			}
			disableDeepCopyByGVK[gvk] = disable
		}
	}
	return disableDeepCopyByGVK, nil
}

func convertToDisableDeepCopyByObject(byGVK internal.DisableDeepCopyByGVK, scheme *runtime.Scheme) (DisableDeepCopyByObject, error) {
	var byObject DisableDeepCopyByObject
	for gvk, value := range byGVK {
		if byObject == nil {
			byObject = DisableDeepCopyByObject{}
		}
		if gvk == (schema.GroupVersionKind{}) {
			byObject[ObjectAll{}] = value
			continue
		}
		obj, err := scheme.New(gvk)
		if err != nil {
			return nil, err
		}
		cObj, ok := obj.(client.Object)
		if !ok {
			return nil, fmt.Errorf("object %T for GVK %q does not implement client.Object", obj, gvk)
		}

		byObject[cObj] = value
	}
	return byObject, nil
}

// TransformByObject associate a client.Object's GVK to a transformer function
// to be applied when storing the object into the cache.
type TransformByObject map[client.Object]toolscache.TransformFunc
