package monitorv3

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

type ObjectKey struct {
	Namespace string
	Name      string
}

type singleItemMonitor struct {
	key ObjectKey
	// store    cache.Store
	// informer cache.Controller
	informer cache.SharedInformer

	// waitGroup is used to ensure that there won't be two concurrent calls to reflector.Run
	// waitGroup sync.WaitGroup
	numHandlers atomic.Int32

	lock    sync.Mutex
	stopped bool
	stopCh  chan struct{}
}

func newSingleItemMonitor(key ObjectKey, informer cache.SharedInformer) *singleItemMonitor {
	return &singleItemMonitor{
		key:      key,
		informer: informer,
		stopCh:   make(chan struct{}),
	}
}

func (i *singleItemMonitor) Stop() bool {
	i.lock.Lock()
	defer i.lock.Unlock()

	if i.stopped {
		return false
	}
	i.stopped = true
	close(i.stopCh)
	return true
}

func (i *singleItemMonitor) HasSynced() bool {
	return i.informer.HasSynced()
}

// func (c *singleItemMonitor) GetByKey(name string) (interface{}, bool, error) {
// 	return c.store.GetByKey(name)
// }
//
// func (c *singleItemMonitor) GetKey() objectKey {
// 	return c.key
// }

// key returns key of an object with a given name and namespace.
// This has to be in-sync with cache.MetaNamespaceKeyFunc.
// func (c *singleItemMonitor) Key(namespace, name string) string {
// 	if len(namespace) > 0 {
// 		return namespace + "/" + name
// 	}
// 	return name
// }

func (i *singleItemMonitor) StartInformer() {
	klog.Info("starting informer")
	i.informer.Run(i.stopCh)
}

func (i *singleItemMonitor) AddEventHandler(handler cache.ResourceEventHandler) (SecretEventHandlerRegistration, error) {
	registration, err := i.informer.AddEventHandler(handler)
	if err != nil {
		return nil, err
	}
	i.numHandlers.Add(1)

	return &secretEventHandlerRegistration{
		ResourceEventHandlerRegistration: registration,
		objectKey:                        i.key,
	}, nil
}

func (i *singleItemMonitor) RemoveEventHandler(handle SecretEventHandlerRegistration) error {
	if err := i.informer.RemoveEventHandler(handle); err != nil {
		return err
	}
	i.numHandlers.Add(-1)
	return nil
}

func (i *singleItemMonitor) GetItemKey() string {
	if keys := strings.Split(i.key.Name, "_"); len(keys) == 1 {
		return keys[1]
	}

	return ""
}

func (i *singleItemMonitor) GetItem() (item interface{}, exists bool, err error) {
	itemKey := i.GetItemKey()
	return i.informer.GetStore().Get(fmt.Sprintf("%s/%s", i.key.Namespace, itemKey))
}
