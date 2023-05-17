/*
Copyright 2017 The Kubernetes Authors.

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

package main

import (
	"context"
	"flag"
	"fmt"
	"reflect"
	"thejasn/watch-based-informer-playground/monitor"
	"time"

	"k8s.io/klog/v2"

	v1 "k8s.io/api/core/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/workqueue"
)

type listObjectFunc func(string, meta_v1.ListOptions) (runtime.Object, error)
type watchObjectFunc func(string, meta_v1.ListOptions) (watch.Interface, error)

var namespace = "sandbox"

// NewController creates a new Controller.
func NewController(queue workqueue.RateLimitingInterface, indexer cache.Indexer, informer cache.Controller, clientset *kubernetes.Clientset, handlerFuncs cache.ResourceEventHandlerFuncs) *Controller {

	return &Controller{
		informer: informer,
		indexer:  indexer,
		queue:    queue,
	}
}

func (c *Controller) processNextItem() bool {
	// Wait until there is a new item in the working queue
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	// Tell the queue that we are done with processing this key. This unblocks the key for other workers
	// This allows safe parallel processing because two pods with the same key are never processed in
	// parallel.
	defer c.queue.Done(key)

	if key, ok := key.(string); ok {
		klog.Info("skipping processing pod with name ", " key ", key)
		return true
	}

	// Invoke the method containing the business logic
	err := c.syncToStdout(key.(string))
	// Handle the error if something went wrong during the execution of the business logic
	c.handleErr(err, key)
	return true
}

// syncToStdout is the business logic of the controller. In this controller it simply prints
// information about the pod to stdout. In case an error happened, it has to simply return the error.
// The retry logic should not be part of the business logic.
func (c *Controller) syncToStdout(key string) error {
	obj, exists, err := c.indexer.Get(key)
	if err != nil {
		klog.Errorf("Fetching object with key %s from store failed with %v", key, err)
		return err
	}

	if !exists {
		// Below we will warm up our cache with a Pod, so that we will see a delete for one pod
		fmt.Printf("Pod %s does not exist anymore\n", key)
	} else {
		// Note that you also have to check the uid if you have a local controlled resource, which
		// is dependent on the actual instance, to detect that a Pod was recreated with the same name
		fmt.Printf("Sync/Add/Update for Pod %s\n", obj.(*v1.Pod).GetName())
	}
	return nil
}

// handleErr checks if an error happened and makes sure we will retry later.
func (c *Controller) handleErr(err error, key interface{}) {
	if err == nil {
		// Forget about the #AddRateLimited history of the key on every successful synchronization.
		// This ensures that future processing of updates for this key is not delayed because of
		// an outdated error history.
		c.queue.Forget(key)
		return
	}

	// This controller retries 5 times if something goes wrong. After that, it stops trying.
	if c.queue.NumRequeues(key) < 5 {
		klog.Infof("Error syncing pod %v: %v", key, err)

		// Re-enqueue the key rate limited. Based on the rate limiter on the
		// queue and the re-enqueue history, the key will be processed later again.
		c.queue.AddRateLimited(key)
		return
	}

	c.queue.Forget(key)
	// Report to an external entity that, even after several retries, we could not successfully process this key
	utilruntime.HandleError(err)
	klog.Infof("Dropping pod %q out of the queue: %v", key, err)
}

// Run begins watching and syncing.
func (c *Controller) Run(workers int, stopCh chan struct{}) {
	defer utilruntime.HandleCrash()

	// Let the workers stop when we are done
	defer c.queue.ShutDown()
	klog.Info("Starting Pod controller")

	go c.informer.Run(stopCh)

	// Wait for all involved caches to be synced, before processing items from the queue is started
	if !cache.WaitForCacheSync(stopCh, c.informer.HasSynced) {
		utilruntime.HandleError(fmt.Errorf("Timed out waiting for caches to sync"))
		return
	}

	for i := 0; i < workers; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	<-stopCh
	klog.Info("Stopping Pod controller")
}

func (c *Controller) runWorker() {
	for c.processNextItem() {
	}
}

func LoadSecretManager(clientset *kubernetes.Clientset, queue workqueue.RateLimitingInterface) Manager {

	listSecret := func(namespace string, opts metav1.ListOptions) (runtime.Object, error) {
		return clientset.CoreV1().Secrets("sandbox").List(context.TODO(), opts)
	}
	watchSecret := func(namespace string, opts metav1.ListOptions) (watch.Interface, error) {
		return clientset.CoreV1().Secrets(namespace).Watch(context.TODO(), opts)
	}
	newSecret := func() runtime.Object {
		return &v1.Secret{}
	}
	isImmutable := func(object runtime.Object) bool {
		if secret, ok := object.(*v1.Secret); ok {
			return secret.Immutable != nil && *secret.Immutable
		}
		return false
	}
	gr := v1.Resource("secret")
	getSecretNames := func(route *v1.Pod) sets.String {
		result := sets.NewString()

		result.Insert(route.Spec.Containers[0].Env[0].ValueFrom.SecretKeyRef.Name)
		// Add route.spec.tls.certificateRef
		return result
	}
	secreth := cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, _ := cache.MetaNamespaceKeyFunc(obj)

			secret := obj.(*v1.Secret)
			klog.Info("Secret added ", "obj ", secret.ResourceVersion, " key ", key)

			queue.Add("sandbox/nginx")

		},
		UpdateFunc: func(old interface{}, new interface{}) {
			key, _ := cache.MetaNamespaceKeyFunc(new)

			secretOld := old.(*v1.Secret)
			secretNew := new.(*v1.Secret)
			klog.Info("Secret updated ", "old ", secretOld.ResourceVersion, " new ", secretNew.ResourceVersion, " key ", key)

			queue.Add("sandbox/nginx")
		},
		DeleteFunc: func(obj interface{}) {

			secret := obj.(*v1.Secret)
			// IndexerInformer uses a delta queue, therefore for deletes we have to use this
			// key function.
			key, _ := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)

			klog.Info("Secret deleted ", " obj ", secret.ResourceVersion, " key ", key)
			queue.Add("sandbox/nginx")
		},
	}

	return NewWatchBasedManager(listSecret, watchSecret, newSecret, isImmutable, gr, 1*time.Second, secreth, getSecretNames)
}

func main() {
	var kubeconfig string
	var master string

	flag.StringVar(&kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
	flag.StringVar(&master, "master", "", "master url")
	flag.Parse()

	// creates the connection
	config, err := clientcmd.BuildConfigFromFlags(master, kubeconfig)
	if err != nil {
		klog.Fatal(err)
	}

	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatal(err)
	}

	// create the pod watcher
	podListWatcher := cache.NewListWatchFromClient(clientset.CoreV1().RESTClient(), "pods", "sandbox", fields.Everything())

	// create the workqueue
	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

	getSecretNames := func(route *v1.Pod) sets.String {
		result := sets.NewString()

		result.Insert(route.Spec.Containers[0].Env[0].ValueFrom.SecretKeyRef.Name)
		// Add route.spec.tls.certificateRef
		return result
	}
	// secretManager := LoadSecretManager(clientset, queue)
	secretManager := monitor.NewSecretMonitor(clientset, queue)

	h := cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)

			pod := obj.(*v1.Pod)
			klog.Info("Add Event ", "pod.Name ", pod.Name, " key ", key)
			if err == nil {
				queue.Add(key)
			}

			// secretManager.RegisterRoute(pod)
			secretManager.Register(pod, getSecretNames)
		},
		UpdateFunc: func(old interface{}, new interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(new)

			oldPod := old.(*v1.Pod)
			newPod := new.(*v1.Pod)
			klog.Info("Update controller ", "old ", oldPod.ResourceVersion, " new ", newPod.ResourceVersion, " key ", key)
			if err == nil && !reflect.DeepEqual(oldPod.Spec.Containers, newPod.Spec.Containers) {
				queue.Add(key)

				// secretManager.UnregisterRoute(oldPod)
				// secretManager.RegisterRoute(newPod)

				secretManager.Unregister(oldPod, getSecretNames)
				secretManager.Register(newPod, getSecretNames)
			}
		},
		DeleteFunc: func(obj interface{}) {
			// IndexerInformer uses a delta queue, therefore for deletes we have to use this
			// key function.
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)

			pod := obj.(*v1.Pod)
			klog.Info("Delete controller ", " obj ", pod.ResourceVersion, " key ", key)
			if err == nil {
				queue.Add(key)
			}
			// secretManager.UnregisterRoute(obj.(*v1.Pod))
			secretManager.Unregister(obj.(*v1.Pod), getSecretNames)

		},
	}

	indexer, informer := cache.NewIndexerInformer(podListWatcher, &v1.Pod{}, 0, h, cache.Indexers{})

	controller := NewController(queue, indexer, informer, clientset, h)

	// Now let's start the controller
	stop := make(chan struct{})
	defer close(stop)
	go controller.Run(1, stop)

	// Wait forever
	select {}
}
