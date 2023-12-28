package monitor

import (
	"context"
	"fmt"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	routev1 "github.com/openshift/api/route/v1"
)

type listObjectFunc func(string, metav1.ListOptions) (runtime.Object, error)
type watchObjectFunc func(string, metav1.ListOptions) (watch.Interface, error)

// Monitor manages Kubernetes secrets. This includes retrieving
// secrets or registering/unregistering them via Routes.
type Monitor interface {
	// Get secret by secret namespace and name.
	GetSecret(namespace, name string) (*v1.Secret, error)

	// WARNING: Register/UnregisterRoute functions should be efficient,
	// i.e. should not block on network operations.

	// RegisterRoute registers all secrets from a given Route.
	RegisterRoute(*routev1.Route, func(*routev1.Route) sets.String)

	// UnregisterRoute unregisters secrets from a given Route that are not
	// used by any other registered Route.
	UnregisterRoute(*routev1.Route, func(*routev1.Route) sets.String)
}

var _ Monitor = (*SecretMonitor)(nil)

// SecretMonitor keeps a store with secrets necessary
// for registered routes.
type SecretMonitor struct {
	registeredObjects map[objectKey]*routev1.Route
	monitors          map[objectKey]*Object

	lock sync.RWMutex

	stopCh <-chan struct{}

	listObject  listObjectFunc
	watchObject watchObjectFunc

	// monitors are the producer of the resourceChanges queue
	resourceChanges workqueue.RateLimitingInterface

	secretHandler cache.ResourceEventHandlerFuncs
}

func NewSecretMonitor(clientset *kubernetes.Clientset, queue workqueue.RateLimitingInterface) *SecretMonitor {
	return &SecretMonitor{
		registeredObjects: map[objectKey]*routev1.Route{},
		monitors:          make(map[objectKey]*Object),
		lock:              sync.RWMutex{},
		stopCh:            make(<-chan struct{}),
		resourceChanges:   queue,

		listObject: func(namespace string, opts metav1.ListOptions) (runtime.Object, error) {
			return clientset.CoreV1().Secrets(namespace).List(context.TODO(), opts)
		},
		watchObject: func(namespace string, opts metav1.ListOptions) (watch.Interface, error) {
			return clientset.CoreV1().Secrets(namespace).Watch(context.TODO(), opts)
		},

		// default secret handler
		secretHandler: cache.ResourceEventHandlerFuncs{
			AddFunc:    func(obj interface{}) {},
			UpdateFunc: func(oldObj, newObj interface{}) {},
			DeleteFunc: func(obj interface{}) {},
		},
	}
}

func (sm *SecretMonitor) WithSecretHandler(handler cache.ResourceEventHandlerFuncs) *SecretMonitor {
	sm.secretHandler = handler
	return sm
}

func (sm *SecretMonitor) GetSecret(namespace, name string) (*v1.Secret, error) {
	key := objectKey{namespace: namespace, name: name}
	gr := appsv1.Resource("secret")

	sm.lock.RLock()
	item, exists := sm.monitors[key]
	sm.lock.RUnlock()

	if !exists {
		return nil, fmt.Errorf("object %q/%q not registered", namespace, name)
	}

	if err := wait.PollImmediate(10*time.Millisecond, time.Second, item.HasSynced); err != nil {
		return nil, fmt.Errorf("failed to sync %s cache: %v", gr.String(), err)
	}

	obj, exists, err := item.GetByKey(item.Key(namespace, name))
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, apierrors.NewNotFound(gr, name)
	}

	if object, ok := obj.(*v1.Secret); ok {
		return object, nil
	}
	return nil, fmt.Errorf("unexpected object type: %v", obj)
}

func (sm *SecretMonitor) RegisterRoute(parent *routev1.Route, getReferencedObjects func(*routev1.Route) sets.String) {
	// names := getReferencedObjects(parent)
	klog.Infof("%+v\n", parent)
	name := parent.Spec
	klog.Info(name)
	klog.Info(parent.Spec.Host)

	sm.lock.Lock()
	defer sm.lock.Unlock()

	for _, name := range []string{"dummy-secret"} {

		key := objectKey{namespace: parent.Namespace, name: name, uid: parent.UID}
		m, exists := sm.monitors[key]

		if !exists {
			fieldSelector := fields.Set{"metadata.name": name}.AsSelector().String()
			listFunc := func(options metav1.ListOptions) (runtime.Object, error) {
				options.FieldSelector = fieldSelector

				klog.Info(fieldSelector)
				return sm.listObject(parent.Namespace, options)
			}
			watchFunc := func(options metav1.ListOptions) (watch.Interface, error) {
				options.FieldSelector = fieldSelector

				klog.Info(fieldSelector)
				return sm.watchObject(parent.Namespace, options)
			}

			store, informer := cache.NewInformer(
				&cache.ListWatch{ListFunc: listFunc, WatchFunc: watchFunc},
				&v1.Secret{},
				0, sm.secretHandler)

			m = NewObject(informer, store, func() (bool, error) { return informer.HasSynced(), nil }, make(chan struct{}))

			go m.StartInformer()

			sm.monitors[key] = m
		}
		m.RefCount++
		klog.Info("secret monitor key added", " reference count ", m.RefCount, " item key ", key)
	}

	var old *routev1.Route
	key := objectKey{namespace: parent.Namespace, name: parent.Name, uid: parent.UID}
	old = sm.registeredObjects[key]
	sm.registeredObjects[key] = parent

	sm.purgeMonitors(old, getReferencedObjects)
}

func (sm *SecretMonitor) UnregisterRoute(parent *routev1.Route, getReferencedObjects func(*routev1.Route) sets.String) {
	var current *routev1.Route
	key := objectKey{namespace: parent.Namespace, name: parent.Name, uid: parent.UID}

	sm.lock.Lock()
	defer sm.lock.Unlock()

	current = sm.registeredObjects[key]
	delete(sm.registeredObjects, key)

	sm.purgeMonitors(current, getReferencedObjects)
}

func (sm *SecretMonitor) purgeMonitors(parent *routev1.Route, getReferencedObjects func(*routev1.Route) sets.String) {
	if parent != nil {
		for name := range getReferencedObjects(parent) {
			key := objectKey{namespace: parent.Namespace, name: name}

			if item, ok := sm.monitors[key]; ok {
				item.RefCount--
				klog.Info("secret monitor key deleted", " reference count ", item.RefCount, " item key ", key)
				if item.RefCount == 0 {
					// Stop the underlying reflector.
					if item.Stop() {
						klog.Info("secret monitor stopped ", " reference count ", item.RefCount, " item key ", key)
					}
					delete(sm.monitors, key)
				}
			}

		}
	}
}

//
// import (
// 	"context"
// 	"fmt"
// 	"sync"
// 	"time"
//
// 	appsv1 "k8s.io/api/apps/v1"
// 	corev1 "k8s.io/api/core/v1"
// 	v1 "k8s.io/api/core/v1"
// 	apierrors "k8s.io/apimachinery/pkg/api/errors"
// 	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
// 	"k8s.io/apimachinery/pkg/fields"
// 	"k8s.io/apimachinery/pkg/runtime"
// 	"k8s.io/apimachinery/pkg/util/sets"
// 	"k8s.io/apimachinery/pkg/util/wait"
// 	"k8s.io/apimachinery/pkg/watch"
// 	"k8s.io/client-go/kubernetes"
// 	"k8s.io/client-go/tools/cache"
// 	"k8s.io/client-go/util/workqueue"
// 	"k8s.io/klog/v2"
// )
//
// type listObjectFunc func(string, metav1.ListOptions) (runtime.Object, error)
// type watchObjectFunc func(string, metav1.ListOptions) (watch.Interface, error)
//
// type SecretMonitor struct {
// 	registeredPods map[objectKey]*v1.Pod
// 	monitors       map[objectKey]*object
//
// 	lock sync.RWMutex
//
// 	stopCh <-chan struct{}
//
// 	listObject  listObjectFunc
// 	watchObject watchObjectFunc
//
// 	// monitors are the producer of the resourceChanges queue
// 	resourceChanges workqueue.RateLimitingInterface
// }
//
// func NewSecretMonitor(clientset *kubernetes.Clientset, queue workqueue.RateLimitingInterface) *SecretMonitor {
// 	return &SecretMonitor{
// 		registeredPods: make(map[objectKey]*v1.Pod),
// 		monitors:       make(map[objectKey]*object),
// 		stopCh:         make(<-chan struct{}),
// 		listObject: func(namespace string, opts metav1.ListOptions) (runtime.Object, error) {
// 			return clientset.CoreV1().Secrets(namespace).List(context.TODO(), opts)
// 		},
// 		watchObject: func(namespace string, opts metav1.ListOptions) (watch.Interface, error) {
// 			return clientset.CoreV1().Secrets(namespace).Watch(context.TODO(), opts)
// 		},
// 		resourceChanges: queue,
// 	}
// }
//
// var _ Manager = (*SecretMonitor)(nil)
//
// func (sm *SecretMonitor) Get(namespace, name string) (runtime.Object, error) {
// 	key := objectKey{namespace: namespace, name: name}
// 	gr := appsv1.Resource("secret")
//
// 	sm.lock.RLock()
// 	item, exists := sm.monitors[key]
// 	sm.lock.RUnlock()
//
// 	if !exists {
// 		return nil, fmt.Errorf("object %q/%q not registered", namespace, name)
// 	}
//
// 	if err := wait.PollImmediate(10*time.Millisecond, time.Second, item.hasSynced); err != nil {
// 		return nil, fmt.Errorf("failed to sync %s cache: %v", gr.String(), err)
// 	}
//
// 	obj, exists, err := item.store.GetByKey(item.key(namespace, name))
// 	if err != nil {
// 		return nil, err
// 	}
// 	if !exists {
// 		return nil, apierrors.NewNotFound(gr, name)
// 	}
//
// 	if object, ok := obj.(runtime.Object); ok {
// 		return object, nil
// 	}
// 	return nil, fmt.Errorf("unexpected object type: %v", obj)
// }
//
// type Namer interface {
// 	GetNamespace() string
// 	GetName() string
// }
//
// func (sm *SecretMonitor) Register(parent runtime.Object, getReferencedObjects func(runtime.Namer) sets.String) {
//
// 	p, _ := parent.(Namer)
// 	p.GetNamespace()
//
// 	names := getReferencedObjects(parent)
//
// 	sm.lock.Lock()
// 	defer sm.lock.Unlock()
//
// 	for name := range names {
//
// 		key := objectKey{namespace: namespace, name: name}
// 		monitor, exists := sm.monitors[key]
//
// 		if !exists {
// 			fieldSelector := fields.Set{"metadata.name": name}.AsSelector().String()
// 			listFunc := func(options metav1.ListOptions) (runtime.Object, error) {
// 				options.FieldSelector = fieldSelector
// 				return sm.listObject(parent.Namespace, options)
// 			}
// 			watchFunc := func(options metav1.ListOptions) (watch.Interface, error) {
// 				options.FieldSelector = fieldSelector
// 				return sm.watchObject(parent.Namespace, options)
// 			}
//
// 			store, informer := cache.NewInformer(
// 				&cache.ListWatch{ListFunc: listFunc, WatchFunc: watchFunc},
// 				&v1.Secret{},
// 				0, cache.ResourceEventHandlerFuncs{
// 					AddFunc: func(obj interface{}) {
// 						key, _ := cache.MetaNamespaceKeyFunc(obj)
//
// 						secret := obj.(*v1.Secret)
// 						klog.Info("Secret added ", "obj ", secret.ResourceVersion, " key ", key)
//
// 						sm.resourceChanges.Add(fmt.Sprintf("%s/%s", parent.Namespace, parent.Name))
//
// 					},
// 					UpdateFunc: func(old interface{}, new interface{}) {
// 						key, _ := cache.MetaNamespaceKeyFunc(new)
//
// 						secretOld := old.(*v1.Secret)
// 						secretNew := new.(*v1.Secret)
// 						klog.Info("Secret updated ", "old ", secretOld.ResourceVersion, " new ", secretNew.ResourceVersion, " key ", key)
//
// 						sm.resourceChanges.Add(fmt.Sprintf("%s/%s", parent.Namespace, parent.Name))
// 					},
// 					DeleteFunc: func(obj interface{}) {
// 						if deletedFinalStateUnknown, ok := obj.(cache.DeletedFinalStateUnknown); ok {
// 							obj = deletedFinalStateUnknown.Obj
// 						}
//
// 						secret := obj.(*v1.Secret)
// 						// IndexerInformer uses a delta queue, therefore for deletes we have to use this
// 						// key function.
// 						key, _ := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
//
// 						klog.Info("Secret deleted ", " obj ", secret.ResourceVersion, " key ", key)
// 						sm.resourceChanges.Add(fmt.Sprintf("%s/%s", parent.Namespace, parent.Name))
// 					},
// 				})
//
// 			monitor = &object{
// 				refCount:  0,
// 				store:     store,
// 				informer:  informer,
// 				hasSynced: func() (bool, error) { return informer.HasSynced(), nil },
// 				stopCh:    make(chan struct{}),
// 			}
//
// 			go monitor.startInformer()
//
// 			sm.monitors[key] = monitor
// 		}
// 		monitor.refCount++
// 		klog.Info("watch based manager add", " ref count ", monitor.refCount, " item key ", key)
// 	}
//
// 	var prev *v1.Pod
// 	key := objectKey{namespace: parent.Namespace, name: parent.Name, uid: parent.UID}
// 	prev = sm.registeredPods[key]
// 	sm.registeredPods[key] = parent
//
// 	if prev != nil {
// 		for name := range getReferencedObjects(prev) {
// 			key := objectKey{namespace: prev.Namespace, name: name}
//
// 			if item, ok := sm.monitors[key]; ok {
// 				item.refCount--
// 				klog.Info("watch based manager delete", " ref count ", item.refCount, " item key ", key)
// 				if item.refCount == 0 {
// 					// Stop the underlying reflector.
// 					if item.stop() {
// 						klog.Info("watch based manager delete informer stopped ", " ref count ", item.refCount, " item key ", key)
// 					}
// 					delete(sm.monitors, key)
// 				}
// 			}
// 		}
// 	}
// }
//
// func (sm *SecretMonitor) Unregister(parent *v1.Pod, getReferencedObjects func(*corev1.Pod) sets.String) {
// 	var prev *v1.Pod
// 	key := objectKey{namespace: parent.Namespace, name: parent.Name, uid: parent.UID}
//
// 	sm.lock.Lock()
// 	defer sm.lock.Unlock()
//
// 	prev = sm.registeredPods[key]
// 	delete(sm.registeredPods, key)
//
// 	if prev != nil {
// 		for name := range getReferencedObjects(prev) {
// 			key := objectKey{namespace: prev.Namespace, name: name}
//
// 			if item, ok := sm.monitors[key]; ok {
// 				item.refCount--
// 				klog.Info("watch based manager delete", " ref count ", item.refCount, " item key ", key)
// 				if item.refCount == 0 {
// 					// Stop the underlying reflector.
// 					if item.stop() {
// 						klog.Info("watch based manager delete informer stopped ", " ref count ", item.refCount, " item key ", key)
// 					}
// 					delete(sm.monitors, key)
// 				}
// 			}
//
// 		}
// 	}
// }
