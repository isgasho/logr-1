package logr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/wiggin77/cfg"
	"github.com/wiggin77/merror"
)

// Logr maintains a list of log targets and accepts incoming
// log records.
type Logr struct {
	tmux    sync.RWMutex // target mutex
	targets []Target

	mux                sync.RWMutex
	maxQueueSizeActual int
	in                 chan *LogRec
	done               chan struct{}
	once               sync.Once
	shutdown           bool
	lvlCache           levelCache

	// MaxQueueSize is the maximum number of log records that can be queued.
	// If exceeded, `OnQueueFull` is called which determines if the log
	// record will be dropped or block until add is successful.
	// If this is modified, it must be done before `Configure` or
	// `AddTarget`.  Defaults to DefaultMaxQueueSize.
	MaxQueueSize int

	// OnLoggerError, when not nil, is called any time an internal
	// logging error occurs. For example, this can happen when a
	// target cannot connect to its data sink.
	OnLoggerError func(error)

	// OnQueueFull, when not nil, is called on an attempt to add
	// a log record to a full Logr queue.
	// `MaxQueueSize` can be used to modify the maximum queue size.
	// This function should return quickly, with a bool indicating whether
	// the log record should be dropped (true) or block until the log record
	// is successfully added (false). If nil then blocking (false) is assumed.
	OnQueueFull func(rec *LogRec, maxQueueSize int) bool

	// OnTargetQueueFull, when not nil, is called on an attempt to add
	// a log record to a full target queue provided the target supports reporting
	// this condition.
	// This function should return quickly, with a bool indicating whether
	// the log record should be dropped (true) or block until the log record
	// is successfully added (false). If nil then blocking (false) is assumed.
	OnTargetQueueFull func(target Target, rec *LogRec, maxQueueSize int) bool

	// OnExit, when not nil, is called when a FatalXXX style log API is called.
	// When nil, then the default behavior is to cleanly shut down this Logr and
	// call `os.Exit(code)`.
	OnExit func(code int)

	// OnPanic, when not nil, is called when a PanicXXX style log API is called.
	// When nil, then the default behavior is to cleanly shut down this Logr and
	// call `panic(err)`.
	OnPanic func(err interface{})

	// EnqueueTimeout is the amount of time a log record can take to be queued.
	// This only applies to blocking enqueue which happen after `logr.OnQueueFull`
	// is called and returns false.
	EnqueueTimeout time.Duration

	// ShutdownTimeout is the amount of time `logr.Shutdown` can execute before
	// timing out.
	ShutdownTimeout time.Duration

	// FlushTimeout is the amount of time `logr.Flush` can execute before
	// timing out.
	FlushTimeout time.Duration

	// UseSyncMapLevelCache should be set to true before the first target is added
	// when high concurrency (e.g. >32 cores) is expected.
	UseSyncMapLevelCache bool
}

// Configure adds/removes targets via the supplied `Config`.
func (logr *Logr) Configure(config *cfg.Config) error {
	// TODO
	return fmt.Errorf("not implemented yet")
}

// AddTarget adds a target to the logger which will receive
// log records for outputting.
func (logr *Logr) AddTarget(target Target) error {
	logr.mux.Lock()
	defer logr.mux.Unlock()

	if logr.shutdown {
		return fmt.Errorf("logr shut down")
	}

	logr.tmux.Lock()
	defer logr.tmux.Unlock()
	logr.targets = append(logr.targets, target)

	logr.once.Do(func() {
		logr.maxQueueSizeActual = logr.MaxQueueSize
		if logr.maxQueueSizeActual == 0 {
			logr.maxQueueSizeActual = DefaultMaxQueueSize
		}
		if logr.maxQueueSizeActual < 0 {
			logr.maxQueueSizeActual = 0
		}
		logr.in = make(chan *LogRec, logr.maxQueueSizeActual)
		logr.done = make(chan struct{})
		if logr.UseSyncMapLevelCache {
			logr.lvlCache = &syncMapLevelCache{}
		} else {
			logr.lvlCache = &arrayLevelCache{}
		}
		logr.lvlCache.setup()
		go logr.start()
	})
	logr.resetLevelCache()
	return nil
}

// NewLogger creates a Logger using defaults. A `Logger` is light-weight
// enough to create on-demand, but typically one or more Loggers are
// created and re-used.
func (logr *Logr) NewLogger() *Logger {
	logger := &Logger{logr: logr}
	return logger
}

// IsLevelEnabled returns true if at least one target has the specified
// level enabled. The result is cached so that subsequent checks are fast.
func (logr *Logr) IsLevelEnabled(lvl Level) LevelStatus {
	// Check cache.
	status, ok := logr.lvlCache.get(lvl.ID)
	if ok {
		return status
	}

	logr.mux.RLock()
	defer logr.mux.RUnlock()

	status = LevelStatus{}

	// Don't accept new log records after shutdown.
	if logr.shutdown {
		return status
	}

	// Check each target.
	logr.tmux.RLock()
	defer logr.tmux.RUnlock()
	for _, t := range logr.targets {
		e, s := t.IsLevelEnabled(lvl)
		if e {
			status.Enabled = true
			if s {
				status.Stacktrace = true
				break // if both enabled then no sense checking more targets
			}
		}
	}

	// Cache and return the result.
	logr.lvlCache.put(lvl.ID, status)
	return status
}

// ResetLevelCache resets the cached results of `IsLevelEnabled`. This is
// called any time a Target is added or a target's level is changed.
func (logr *Logr) ResetLevelCache() {
	// Write lock so that new cache entries cannot be stored while we
	// clear the cache.
	logr.mux.Lock()
	defer logr.mux.Unlock()
	logr.resetLevelCache()
}

// resetLevelCache empties the level cache without locking.
// mux.Lock must be held before calling this function.
func (logr *Logr) resetLevelCache() {
	logr.lvlCache.clear()
}

// enqueue adds a log record to the logr queue. If the queue is full then
// this function either blocks or the log record is dropped, depending on
// the result of calling `OnQueueFull`.
func (logr *Logr) enqueue(rec *LogRec) {
	if logr.in == nil {
		logr.ReportError(fmt.Errorf("AddTarget or Configure must be called before enqueue"))
	}

	select {
	case logr.in <- rec:
	default:
		if logr.OnQueueFull != nil && logr.OnQueueFull(rec, logr.maxQueueSizeActual) {
			return // drop the record
		}
		select {
		case <-time.After(logr.enqueueTimeout()):
			logr.ReportError(fmt.Errorf("enqueue timed out for log rec [%v]", rec))
		case logr.in <- rec: // block until success or timeout
		}
	}
}

// exit is called by one of the FatalXXX style APIS. If `logr.OnExit` is not nil
// then that method is called, otherwise the default behavior is to shut down this
// Logr cleanly then call `os.Exit(code)`.
func (logr *Logr) exit(code int) {
	if logr.OnExit != nil {
		logr.OnExit(code)
		return
	}

	if err := logr.Shutdown(); err != nil {
		logr.ReportError(err)
	}
	os.Exit(code)
}

// panic is called by one of the PanicXXX style APIS. If `logr.OnPanic` is not nil
// then that method is called, otherwise the default behavior is to shut down this
// Logr cleanly then call `panic(err)`.
func (logr *Logr) panic(err interface{}) {
	if logr.OnPanic != nil {
		logr.OnPanic(err)
		return
	}

	if err := logr.Shutdown(); err != nil {
		logr.ReportError(err)
	}
	panic(err)
}

// Flush blocks while flushing the logr queue and all target queues, by
// writing existing log records to valid targets.
// Any attempts to add new log records will block until flush is complete.
// `logr.FlushTimeout` determines how long flush can execute before
// timing out. Use `IsTimeoutError` to determine if the returned error is
// due to a timeout.
func (logr *Logr) Flush() error {
	logr.mux.Lock()
	defer logr.mux.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), logr.flushTimeout())
	defer cancel()

	rec := newFlushLogRec(logr.NewLogger())
	logr.enqueue(rec)

	select {
	case <-ctx.Done():
		return newTimeoutError("logr queue shutdown timeout")
	case <-rec.flush:
	}
	return nil
}

// Shutdown cleanly stops the logging engine after making best efforts
// to flush all targets. Call this function right before application
// exit - logr cannot be restarted once shut down.
// `logr.ShutdownTimeout` determines how long shutdown can execute before
// timing out. Use `IsTimeoutError` to determine if the returned error is
// due to a timeout.
func (logr *Logr) Shutdown() error {
	logr.mux.Lock()
	if logr.shutdown {
		return errors.New("Shutdown called again after shut down")
	}
	logr.shutdown = true
	logr.resetLevelCache()
	logr.mux.Unlock()

	errs := merror.New()

	ctx, cancel := context.WithTimeout(context.Background(), logr.shutdownTimeout())
	defer cancel()

	// close the incoming channel and wait for read loop to exit.
	close(logr.in)
	select {
	case <-ctx.Done():
		errs.Append(newTimeoutError("logr queue shutdown timeout"))
	case <-logr.done:
	}

	// logr.in channel should now be drained to targets and no more log records
	// can be added.
	logr.tmux.RLock()
	defer logr.tmux.RUnlock()
	for _, t := range logr.targets {
		err := t.Shutdown(ctx)
		if err != nil {
			errs.Append(err)
		}
	}
	return errs.ErrorOrNil()
}

// ReportError is used to notify the host application of any internal logging errors.
// If `OnLoggerError` is not nil, it is called with the error, otherwise the error is
// output to `os.Stderr`.
func (logr *Logr) ReportError(err interface{}) {
	if logr.OnLoggerError == nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	logr.OnLoggerError(fmt.Errorf("%v", err))
}

// enqueueTimeout returns amount of time a log record can take to be queued.
// This only applies to blocking enqueue which happen after `logr.OnQueueFull` is called
// and returns false.
func (logr *Logr) enqueueTimeout() time.Duration {
	if logr.EnqueueTimeout == 0 {
		return DefaultEnqueueTimeout
	}
	return logr.EnqueueTimeout
}

// shutdownTimeout returns the timeout duration for `logr.Shutdown`.
func (logr *Logr) shutdownTimeout() time.Duration {
	if logr.ShutdownTimeout == 0 {
		return DefaultShutdownTimeout
	}
	return logr.ShutdownTimeout
}

// flushTimeout returns the timeout duration for `logr.Flush`.
func (logr *Logr) flushTimeout() time.Duration {
	if logr.FlushTimeout == 0 {
		return DefaultFlushTimeout
	}
	return logr.FlushTimeout
}

// start selects on incoming log records until done channel signals.
// Incoming log records are fanned out to all log targets.
func (logr *Logr) start() {
	defer func() {
		if r := recover(); r != nil {
			logr.ReportError(r)
			go logr.start()
		}
	}()

	var rec *LogRec
	var more bool
	for {
		select {
		case rec, more = <-logr.in:
			if more {
				if rec.flush != nil {
					logr.flush(rec.flush)
				} else {
					rec.prep()
					logr.fanout(rec)
				}
			} else {
				close(logr.done)
				return
			}
		}
	}
}

// fanout pushes a LogRec to all targets.
func (logr *Logr) fanout(rec *LogRec) {
	var target Target
	defer func() {
		if r := recover(); r != nil {
			logr.ReportError(fmt.Errorf("fanout failed for target %s, %v", target, r))
		}
	}()

	logr.tmux.RLock()
	defer logr.tmux.RUnlock()
	for _, target = range logr.targets {
		if enabled, _ := target.IsLevelEnabled(rec.Level()); enabled {
			target.Log(rec)
		}
	}
}

// flush drains the queue and notifies when done.
func (logr *Logr) flush(done chan<- struct{}) {
	// first drain the logr queue.
loop:
	for {
		var rec *LogRec
		select {
		case rec = <-logr.in:
			if rec.flush == nil {
				rec.prep()
				logr.fanout(rec)
			}
		default:
			break loop
		}
	}

	logger := logr.NewLogger()

	// drain all the targets; block until finished.
	logr.tmux.RLock()
	defer logr.tmux.RUnlock()
	for _, target := range logr.targets {
		rec := newFlushLogRec(logger)
		target.Log(rec)
		<-rec.flush
	}
	done <- struct{}{}
}
