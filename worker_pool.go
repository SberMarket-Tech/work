package work

import (
	"context"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

const cronFormat = cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor

// WorkerPool represents a pool of workers. It forms the primary API of gocraft/work. WorkerPools provide the public API of gocraft/work. You can attach jobs and middlware to them. You can start and stop them. Based on their concurrency setting, they'll spin up N worker goroutines.
type WorkerPool struct {
	workerPoolID string
	concurrency  uint
	namespace    string // eg, "myapp-work"
	pool         Pool

	contextType                 reflect.Type
	jobTypes                    map[string]*jobType
	middleware                  []*middlewareHandler
	started                     bool
	periodicJobs                []*periodicJob
	watchdog                    *watchdog
	watchdogFailCheckingTimeout time.Duration

	workers          []*worker
	heartbeater      *workerPoolHeartbeater
	retrier          *requeuer
	scheduler        *requeuer
	reapPeriod       time.Duration
	deadPoolReaper   *deadPoolReaper
	periodicEnqueuer *periodicEnqueuer

	reaperHook ReaperHook
	logger     StructuredLogger
}

type jobType struct {
	Name string
	JobOptions

	isGeneric      bool
	genericHandler interface{}
	dynamicHandler reflect.Value
}

func (jt *jobType) calcBackoff(j *Job) int64 {
	if jt.Backoff == nil {
		return defaultBackoffCalculator(j)
	}
	return jt.Backoff(j)
}

// You may provide your own backoff function for retrying failed jobs or use the builtin one.
// Returns the number of seconds to wait until the next attempt.
//
// The builtin backoff calculator provides an exponentially increasing wait function.
type BackoffCalculator func(job *Job) int64

// JobOptions can be passed to JobWithOptions.
type JobOptions struct {
	Priority       uint              // Priority from 1 to 10000
	MaxFails       uint              // 1: send straight to dead (unless SkipDead)
	SkipDead       bool              // If true, don't send failed jobs to the dead queue when retries are exhausted.
	MaxConcurrency uint              // Max number of jobs to keep in flight (default is 0, meaning no max)
	Backoff        BackoffCalculator // If not set, uses the default backoff algorithm
}

// Deprecated: use JobHandler instead.
// GenericHandler is a job handler without any custom context.
type GenericHandler func(*Job) error

// Deprecated: use JobMiddleware instead.
// GenericMiddlewareHandler is a middleware without any custom context.
type GenericMiddlewareHandler func(*Job, NextMiddlewareFunc) error

// NextMiddlewareFunc is a function type (whose instances are named 'next') that you call to advance to the next middleware.
type NextMiddlewareFunc func() error

type middlewareHandler struct {
	isGeneric         bool
	genericMiddleware interface{}
	dynamicMiddleware reflect.Value
}

// Job handler types.
type (
	JobHandler        = func(*Job) error
	JobContextHandler = func(context.Context, *Job) error
)

// Job middleware types.
type (
	JobMiddleware        = func(*Job, NextMiddlewareFunc) error
	JobContextMiddleware = func(context.Context, *Job, JobContextHandler) error
)

// NewWorkerPool creates a new worker pool. ctx should be a struct literal whose type will be used for middleware and handlers.
// concurrency specifies how many workers to spin up - each worker can process jobs concurrently.
func NewWorkerPool(ctx interface{}, concurrency uint, namespace string, pool Pool, opts ...WorkerPoolOption) *WorkerPool {
	if pool == nil {
		panic("NewWorkerPool needs a non-nil Pool")
	}

	ctxType := reflect.TypeOf(ctx)
	validateContextType(ctxType)
	wp := &WorkerPool{
		workerPoolID: makeIdentifier(),
		concurrency:  concurrency,
		namespace:    namespace,
		pool:         pool,
		contextType:  ctxType,
		jobTypes:     make(map[string]*jobType),
		logger:       noopLogger,
	}

	for _, opt := range opts {
		opt(wp)
	}

	wp.watchdog = newWatchdog(
		watchdogWithLogger(wp.logger),
		watchdogWithFailCheckingTimeout(wp.watchdogFailCheckingTimeout),
	)

	for i := uint(0); i < wp.concurrency; i++ {
		w := newWorker(
			wp.namespace,
			wp.workerPoolID,
			wp.pool,
			wp.contextType,
			nil,
			wp.jobTypes,
			wp.logger,
			wp.watchdog.processedJobs,
		)
		wp.workers = append(wp.workers, w)
	}

	return wp
}

// Middleware appends the specified function to the middleware chain. The fn can
// take one of these forms:
//
//	func(context.Context, *Job, JobContextHandler) error
//	func(*Job, NextMiddlewareFunc) error
//	(*ContextType).func(context.Context, *Job, JobContextHandler) error
//	(*ContextType).func(*Job, NextMiddlewareFunc) error
//
// ContextType matches the type of ctx specified when creating a pool.
func (wp *WorkerPool) Middleware(fn interface{}) *WorkerPool {
	vfn := reflect.ValueOf(fn)
	validateMiddlewareType(wp.contextType, vfn)

	mw := &middlewareHandler{
		genericMiddleware: fn,
		dynamicMiddleware: vfn,
	}

	switch fn.(type) {
	case JobMiddleware, JobContextMiddleware:
		mw.isGeneric = true
	}

	wp.middleware = append(wp.middleware, mw)

	for _, w := range wp.workers {
		w.updateMiddlewareAndJobTypes(wp.middleware, wp.jobTypes)
	}

	return wp
}

// Job registers the job name to the specified handler fn. For instance, when workers pull jobs from the name queue they'll be processed by the specified handler function.
// fn can take one of these forms:
//
//	func(context.Context, *Job) error
//	func(*Job) error
//	(*ContextType).func(context.Context, *Job) error
//	(*ContextType).func(*Job) error
//
// ContextType matches the type of ctx specified when creating a pool.
func (wp *WorkerPool) Job(name string, fn interface{}) *WorkerPool {
	return wp.JobWithOptions(name, JobOptions{}, fn)
}

// JobWithOptions adds a handler for 'name' jobs as per the Job function, but permits you specify additional options
// such as a job's priority, retry count, and whether to send dead jobs to the dead job queue or trash them.
func (wp *WorkerPool) JobWithOptions(name string, jobOpts JobOptions, fn interface{}) *WorkerPool {
	jobOpts = applyDefaultsAndValidate(jobOpts)

	vfn := reflect.ValueOf(fn)
	validateHandlerType(wp.contextType, vfn)

	jt := &jobType{
		Name:           name,
		JobOptions:     jobOpts,
		genericHandler: fn,
		dynamicHandler: vfn,
	}

	switch fn.(type) {
	case JobHandler, JobContextHandler:
		jt.isGeneric = true
	}

	wp.jobTypes[name] = jt

	for _, w := range wp.workers {
		w.updateMiddlewareAndJobTypes(wp.middleware, wp.jobTypes)
	}

	return wp
}

func newPeriodicJob(spec string, jobName string) (*periodicJob, error) {
	schedule, err := cron.NewParser(cronFormat).Parse(spec)
	if err != nil {
		return nil, err
	}

	return &periodicJob{jobName: jobName, spec: spec, schedule: schedule}, nil
}

// PeriodicallyEnqueue will periodically enqueue jobName according to the cron-based spec.
// The spec format is based on github.com/robfig/cron/v3, which is a relatively standard cron format.
// Note that the first value can be seconds!
// If you have multiple worker pools on different machines, they'll all coordinate and only enqueue your job once.
func (wp *WorkerPool) PeriodicallyEnqueue(spec string, jobName string) *WorkerPool {
	j, err := newPeriodicJob(spec, jobName)
	if err != nil {
		panic(err)
	}

	wp.periodicJobs = append(wp.periodicJobs, j)

	return wp
}

// Start starts the workers and associated processes.
func (wp *WorkerPool) Start() {
	if wp.started {
		return
	}
	wp.started = true

	// TODO: we should cleanup stale keys on startup from previously registered jobs
	wp.writeConcurrencyControlsToRedis()
	go wp.writeKnownJobsToRedis()

	for _, w := range wp.workers {
		go w.start()
	}

	wp.heartbeater = newWorkerPoolHeartbeater(
		wp.namespace,
		wp.pool,
		wp.workerPoolID,
		wp.jobTypes,
		wp.concurrency,
		wp.workerIDs(),
		wp.logger,
	)
	wp.heartbeater.start()
	wp.startRequeuers()
	wp.periodicEnqueuer = newPeriodicEnqueuer(
		wp.namespace,
		wp.pool,
		wp.periodicJobs,
		wp.logger,
	)
	wp.periodicEnqueuer.start()

	wp.watchdog.addPeriodicJobs(wp.periodicJobs...)
	wp.watchdog.start()
}

func (wp *WorkerPool) WatchdogStats() []WatchdogStat {
	return wp.watchdog.stats()
}

// Stop stops the workers and associated processes.
func (wp *WorkerPool) Stop() {
	if !wp.started {
		return
	}
	wp.started = false

	wg := sync.WaitGroup{}
	for _, w := range wp.workers {
		wg.Add(1)
		go func(w *worker) {
			w.stop()
			wg.Done()
		}(w)
	}
	wg.Wait()
	wp.heartbeater.stop()
	wp.retrier.stop()
	wp.scheduler.stop()
	wp.deadPoolReaper.stop()
	wp.periodicEnqueuer.stop()
	wp.watchdog.stop()
}

// Drain drains all jobs in the queue before returning. Note that if jobs are added faster than we can process them, this function wouldn't return.
func (wp *WorkerPool) Drain() {
	wg := sync.WaitGroup{}
	for _, w := range wp.workers {
		wg.Add(1)
		go func(w *worker) {
			w.drain()
			wg.Done()
		}(w)
	}
	wg.Wait()
}

func (wp *WorkerPool) startRequeuers() {
	jobNames := make([]string, 0, len(wp.jobTypes))
	for name := range wp.jobTypes {
		jobNames = append(jobNames, name)
	}

	wp.retrier = newRequeuer(wp.namespace, wp.pool, redisKeyRetry(wp.namespace), jobNames, wp.logger)
	wp.scheduler = newRequeuer(wp.namespace, wp.pool, redisKeyScheduled(wp.namespace), jobNames, wp.logger)
	wp.deadPoolReaper = newDeadPoolReaper(
		wp.namespace,
		wp.pool,
		jobNames,
		wp.reapPeriod,
		wp.reaperHook,
		wp.logger,
	)
	wp.retrier.start()
	wp.scheduler.start()
	wp.deadPoolReaper.start()
}

func (wp *WorkerPool) workerIDs() []string {
	wids := make([]string, 0, len(wp.workers))
	for _, w := range wp.workers {
		wids = append(wids, w.workerID)
	}
	sort.Strings(wids)
	return wids
}

func (wp *WorkerPool) writeKnownJobsToRedis() {
	if len(wp.jobTypes) == 0 {
		return
	}

	conn := wp.pool.Get()
	defer conn.Close()
	key := redisKeyKnownJobs(wp.namespace)
	jobNames := make([]interface{}, 0, len(wp.jobTypes)+1)
	jobNames = append(jobNames, key)
	for k := range wp.jobTypes {
		jobNames = append(jobNames, k)
	}

	wp.logger.Debug("write_known_jobs", slog.Any("job_names", jobNames))
	if _, err := conn.Do("SADD", jobNames...); err != nil {
		wp.logger.Error("write_known_jobs", errAttr(err))
	}
}

func (wp *WorkerPool) writeConcurrencyControlsToRedis() {
	if len(wp.jobTypes) == 0 {
		return
	}

	conn := wp.pool.Get()
	defer conn.Close()
	for jobName, jobType := range wp.jobTypes {
		if _, err := conn.Do("SET", redisKeyJobsConcurrency(wp.namespace, jobName), jobType.MaxConcurrency); err != nil {
			wp.logger.Error("write_concurrency_controls_max_concurrency", errAttr(err))
		}
	}
}

// validateContextType will panic if context is invalid
func validateContextType(ctxType reflect.Type) {
	if ctxType.Kind() != reflect.Struct {
		panic("work: Context needs to be a struct type")
	}
}

func validateHandlerType(ctxType reflect.Type, vfn reflect.Value) {
	if !isValidHandlerType(ctxType, vfn) {
		panic(instructiveMessage(
			vfn,
			"a handler",
			"handler",
			"job *work.Job",
			"job *work.Job",
			ctxType,
		))
	}
}

func validateMiddlewareType(ctxType reflect.Type, vfn reflect.Value) {
	if !isValidMiddlewareType(ctxType, vfn) {
		panic(instructiveMessage(
			vfn,
			"middleware",
			"middleware",
			"job *work.Job, next web.JobContextHandler",
			"job *work.Job, next web.NextMiddlewareFunc",
			ctxType,
		))
	}
}

// Since it's easy to pass the wrong method as a middleware/handler, and since the user can't rely on static type checking since we use reflection,
// lets be super helpful about what they did and what they need to do.
// Arguments:
//   - vfn is the failed method
//   - addingType is for "You are adding {addingType} to a worker pool...". Eg, "middleware" or "a handler"
//   - yourType is for "Your {yourType} function can have...". Eg, "middleware" or "handler" or "error handler"
//   - args is like "job *work.Job, next web.JobContextHandler"
//   - oldArgs is like "job *work.Job, next web.NextMiddlewareFunc"
//   - NOTE: args can be calculated if you pass in each type. BUT, it doesn't have example argument name, so it has less copy/paste value.
func instructiveMessage(
	vfn reflect.Value,
	addingType string,
	yourType string,
	args string,
	oldArgs string,
	ctxType reflect.Type,
) string {
	// Get context type without package.
	ctxString := ctxType.String()
	splitted := strings.Split(ctxString, ".")
	if len(splitted) <= 1 {
		ctxString = splitted[0]
	} else {
		ctxString = splitted[1]
	}

	str := "\n" + strings.Repeat("*", 120) + "\n"
	str += "* You are adding " + addingType + " to a worker pool with context type '" + ctxString + "'\n"
	str += "*\n*\n"
	str += "* Your " + yourType + " function can have one of these signatures:\n"
	str += "*\n"
	str += "* // If you don't need context:\n"
	str += "* func YourFunctionName(" + args + ") error\n"
	str += "*\n"
	str += "* // If you want your " + yourType + " to accept a context:\n"
	str += "* func YourFunctionName(ctx context.Context, " + args + ") error // or,\n"
	str += "* func (c *" + ctxString + ") YourFunctionName(ctx context.Context, " + args + ") error\n"
	str += "*\n"
	str += "* // Deprecated but supported options:\n"
	str += "* func (c *" + ctxString + ") YourFunctionName(" + oldArgs + ") error // or,\n"
	str += "* func YourFunctionName(c *" + ctxString + ", " + oldArgs + ") error\n"
	str += "*\n"
	str += "* Unfortunately, your function has this signature: " + vfn.Type().String() + "\n"
	str += "*\n"
	str += strings.Repeat("*", 120) + "\n"

	return str
}

func isValidHandlerType(ctxType reflect.Type, vfn reflect.Value) bool {
	fnType := vfn.Type()

	if fnType.Kind() != reflect.Func {
		return false
	}

	numIn := fnType.NumIn()
	numOut := fnType.NumOut()

	if numOut != 1 {
		return false
	}

	outType := fnType.Out(0)
	var e *error

	if outType != reflect.TypeOf(e).Elem() {
		return false
	}

	var ctx *context.Context
	var j *Job

	switch numIn {
	case 1:
		// func(j *Job) error
		if fnType.In(0) != reflect.TypeOf(j) {
			return false
		}

	case 2:
		// func(c *tstCtx, j *Job) error
		// func(ctx context.Context, j *Job) error
		if fnType.In(0) != reflect.PtrTo(ctxType) && fnType.In(0) != reflect.TypeOf(ctx).Elem() {
			return false
		}
		if fnType.In(1) != reflect.TypeOf(j) {
			return false
		}

	default:
		return false
	}

	return true
}

func isValidMiddlewareType(ctxType reflect.Type, vfn reflect.Value) bool {
	fnType := vfn.Type()

	if fnType.Kind() != reflect.Func {
		return false
	}

	numIn := fnType.NumIn()
	numOut := fnType.NumOut()

	if numOut != 1 {
		return false
	}

	outType := fnType.Out(0)
	var e *error

	if outType != reflect.TypeOf(e).Elem() {
		return false
	}

	var ctx *context.Context
	var j *Job
	var nfn NextMiddlewareFunc
	var jch JobContextHandler

	switch numIn {
	// func(j *Job, n NextMiddlewareFunc) error
	case 2:
		if fnType.In(0) != reflect.TypeOf(j) {
			return false
		}
		if fnType.In(1) != reflect.TypeOf(nfn) {
			return false
		}

	case 3:
		switch fnType.In(2) {
		// func(c *tstCtx, j *Job, n NextMiddlewareFunc) error
		case reflect.TypeOf(nfn):
			if fnType.In(0) != reflect.PtrTo(ctxType) {
				return false
			}
			if fnType.In(1) != reflect.TypeOf(j) {
				return false
			}

		// func(ctx context.Context, j *Job, n JobContextHandler) error
		case reflect.TypeOf(jch):
			if fnType.In(0) != reflect.TypeOf(ctx).Elem() {
				return false
			}
			if fnType.In(1) != reflect.TypeOf(j) {
				return false
			}

		default:
			return false
		}

	default:
		return false
	}

	return true
}

func applyDefaultsAndValidate(jobOpts JobOptions) JobOptions {
	if jobOpts.Priority == 0 {
		jobOpts.Priority = 1
	}

	if jobOpts.MaxFails == 0 {
		jobOpts.MaxFails = 4
	}

	if jobOpts.Priority > 100000 {
		panic("work: JobOptions.Priority must be between 1 and 100000")
	}

	return jobOpts
}

// WorkerPoolOption is an optional option for WorkerPool.
type WorkerPoolOption func(wp *WorkerPool)

// WithReapPeriod defines the reaper running cycle period.
func WithReapPeriod(p time.Duration) WorkerPoolOption {
	return func(wp *WorkerPool) {
		wp.reapPeriod = p
	}
}

// WithReaperHook registers a hook to monitor the reaper's actions.
func WithReaperHook(h ReaperHook) WorkerPoolOption {
	return func(wp *WorkerPool) {
		wp.reaperHook = h
	}
}

// WithLogger registers logger.
func WithLogger(l StructuredLogger) WorkerPoolOption {
	return func(wp *WorkerPool) {
		wp.logger = l
	}
}

// WithWatchdogFailCheckingTimeout defines the watchdog checking timeout
// that marks task as failed (default WatchdogFailCheckingTimeout).
func WithWatchdogFailCheckingTimeout(p time.Duration) WorkerPoolOption {
	return func(wp *WorkerPool) {
		wp.watchdogFailCheckingTimeout = p
	}
}
