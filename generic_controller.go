package main

import (
	"thejasn/watch-based-informer-playground/monitor"

	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

// Controller demonstrates how to implement a controller with client-go.
type Controller struct {
	indexer       cache.Indexer
	store         cache.Store
	secretManager Manager
	queue         workqueue.RateLimitingInterface
	informer      cache.Controller

	referenceManager monitor.Manager
}
