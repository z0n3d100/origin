package factory

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/robfig/cron"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	operatorv1 "github.com/openshift/api/operator/v1"

	"github.com/openshift/library-go/pkg/operator/v1helpers"
	operatorv1helpers "github.com/openshift/library-go/pkg/operator/v1helpers"
)

// SyntheticRequeueError can be returned from sync() in case of forcing a sync() retry artificially.
// This can be also done by re-adding the key to queue, but this is cheaper and more convenient.
var SyntheticRequeueError = errors.New("synthetic requeue request")

// baseController represents generic Kubernetes controller boiler-plate
type baseController struct {
	name               string
	cachesToSync       []cache.InformerSynced
	sync               func(ctx context.Context, controllerContext SyncContext) error
	syncContext        SyncContext
	syncDegradedClient operatorv1helpers.OperatorClient
	resyncEvery        time.Duration
	resyncSchedules    []cron.Schedule
	postStartHooks     []PostStartHook
}

var _ Controller = &baseController{}

func (c baseController) Name() string {
	return c.name
}

type scheduledJob struct {
	queue workqueue.RateLimitingInterface
	name  string
}

func newScheduledJob(name string, queue workqueue.RateLimitingInterface) cron.Job {
	return &scheduledJob{
		queue: queue,
		name:  name,
	}
}

func (s *scheduledJob) Run() {
	klog.V(4).Infof("Triggering scheduled %q controller run", s.name)
	s.queue.Add(DefaultQueueKey)
}

func (c *baseController) Run(ctx context.Context, workers int) {
	// HandleCrash recovers panics
	defer utilruntime.HandleCrash()
	if !cache.WaitForNamedCacheSync(c.name, ctx.Done(), c.cachesToSync...) {
		panic("timeout waiting for informer cache") // this will be recovered using HandleCrash()
	}

	var workerWg sync.WaitGroup
	defer func() {
		defer klog.Infof("All %s workers have been terminated", c.name)
		workerWg.Wait()
	}()

	// queueContext is used to track and initiate queue shutdown
	queueContext, queueContextCancel := context.WithCancel(context.TODO())

	for i := 1; i <= workers; i++ {
		klog.Infof("Starting #%d worker of %s controller ...", i, c.name)
		workerWg.Add(1)
		go func() {
			defer func() {
				klog.Infof("Shutting down worker of %s controller ...", c.name)
				workerWg.Done()
			}()
			c.runWorker(queueContext)
		}()
	}

	// if scheduled run is requested, run the cron scheduler
	if c.resyncSchedules != nil {
		scheduler := cron.New()
		for _, s := range c.resyncSchedules {
			scheduler.Schedule(s, newScheduledJob(c.name, c.syncContext.Queue()))
		}
		scheduler.Start()
		defer scheduler.Stop()
	}

	// runPeriodicalResync is independent from queue
	if c.resyncEvery > 0 {
		workerWg.Add(1)
		go func() {
			defer workerWg.Done()
			c.runPeriodicalResync(ctx, c.resyncEvery)
		}()
	}

	// run post-start hooks (custom triggers, etc.)
	if len(c.postStartHooks) > 0 {
		var hookWg sync.WaitGroup
		defer func() {
			hookWg.Wait() // wait for the post-start hooks
			klog.Infof("All %s post start hooks have been terminated", c.name)
		}()
		for i := range c.postStartHooks {
			hookWg.Add(1)
			go func(index int) {
				defer hookWg.Done()
				if err := c.postStartHooks[index](ctx, c.syncContext); err != nil {
					klog.Warningf("%s controller post start hook error: %v", c.name, err)
				}
			}(i)
		}
	}

	// Handle controller shutdown

	<-ctx.Done()                     // wait for controller context to be cancelled
	c.syncContext.Queue().ShutDown() // shutdown the controller queue first
	queueContextCancel()             // cancel the queue context, which tell workers to initiate shutdown

	// Wait for all workers to finish their job.
	// at this point the Run() can hang and caller have to implement the logic that will kill
	// this controller (SIGKILL).
	klog.Infof("Shutting down %s ...", c.name)
}

func (c *baseController) Sync(ctx context.Context, syncCtx SyncContext) error {
	return c.sync(ctx, syncCtx)
}

func (c *baseController) runPeriodicalResync(ctx context.Context, interval time.Duration) {
	if interval == 0 {
		return
	}
	go wait.UntilWithContext(ctx, func(ctx context.Context) {
		c.syncContext.Queue().Add(DefaultQueueKey)
	}, interval)
}

// runWorker runs a single worker
// The worker is asked to terminate when the passed context is cancelled and is given terminationGraceDuration time
// to complete its shutdown.
func (c *baseController) runWorker(queueCtx context.Context) {
	var workerWaitGroup sync.WaitGroup
	workerWaitGroup.Add(1)
	go func() {
		defer utilruntime.HandleCrash()
		defer workerWaitGroup.Done()
		for {
			select {
			case <-queueCtx.Done():
				return
			default:
				c.processNextWorkItem(queueCtx)
			}
		}
	}()
	workerWaitGroup.Wait()
}

// reconcile wraps the sync() call and if operator client is set, it handle the degraded condition if sync() returns an error.
func (c *baseController) reconcile(ctx context.Context, syncCtx SyncContext) error {
	err := c.sync(ctx, syncCtx)
	if c.syncDegradedClient == nil {
		return err
	}
	if err != nil {
		_, _, updateErr := v1helpers.UpdateStatus(c.syncDegradedClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    c.name + "Degraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "SyncError",
			Message: err.Error(),
		}))
		if updateErr != nil {
			klog.Warningf("Updating status of %q failed: %v", c.Name(), updateErr)
		}
		return err
	}
	_, _, updateErr := v1helpers.UpdateStatus(c.syncDegradedClient,
		v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:   c.name + "Degraded",
			Status: operatorv1.ConditionFalse,
			Reason: "AsExpected",
		}))
	return updateErr
}

func (c *baseController) processNextWorkItem(queueCtx context.Context) {
	key, quit := c.syncContext.Queue().Get()
	if quit {
		return
	}
	defer c.syncContext.Queue().Done(key)

	syncCtx := c.syncContext.(syncContext)
	var ok bool
	syncCtx.queueKey, ok = key.(string)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("%q controller failed to process key %q (not a string)", c.name, key))
		return
	}

	if err := c.reconcile(queueCtx, syncCtx); err != nil {
		if err == SyntheticRequeueError {
			// logging this helps detecting wedged controllers with missing pre-requirements
			klog.V(5).Infof("%q controller requested synthetic requeue with key %q", c.name, key)
		} else {
			utilruntime.HandleError(fmt.Errorf("%q controller failed to sync %q, err: %w", c.name, key, err))
		}
		c.syncContext.Queue().AddRateLimited(key)
		return
	}

	c.syncContext.Queue().Forget(key)
}
