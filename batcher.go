package batcher

// NOTE: please review this code which organizes operations into batches based on criteria

import (
	"sync"
	"time"
)

const (
	batcherPhaseUninitialized = iota
	batcherPhaseStarted
	batcherPhasePaused
	batcherPhaseStopped
)

const (
	AuditMsgFailureOnTargetAndInflight = "an audit revealed that the target and inflight should both be zero but neither was."
	AuditMsgFailureOnTarget            = "an audit revealed that the target should be zero but was not."
	AuditMsgFailureOnInflight          = "an audit revealed that inflight should be zero but was not."
)

type IBatcher interface {
	ieventer
	WithRateLimiter(rl RateLimiter) IBatcher
	WithFlushInterval(val time.Duration) IBatcher
	WithCapacityInterval(val time.Duration) IBatcher
	WithAuditInterval(val time.Duration) IBatcher
	WithMaxOperationTime(val time.Duration) IBatcher
	WithPauseTime(val time.Duration) IBatcher
	WithErrorOnFullBuffer() IBatcher
	WithEmitBatch() IBatcher
	WithEmitFlush() IBatcher
	WithMaxConcurrentBatches(val uint32) IBatcher
	Enqueue(op IOperation) error
	Pause()
	Flush()
	Inflight() uint32
	OperationsInBuffer() uint32
	NeedsCapacity() uint32
	Start() (err error)
	Stop()
}

type Batcher struct {
	eventer

	// configuration items that should not change after Start()
	ratelimiter          RateLimiter
	flushInterval        time.Duration
	capacityInterval     time.Duration
	auditInterval        time.Duration
	maxOperationTime     time.Duration
	pauseTime            time.Duration
	errorOnFullBuffer    bool
	emitBatch            bool
	emitFlush            bool
	maxConcurrentBatches uint32

	// used for internal operations
	buffer               IBuffer       // operations that are in the queue
	pause                chan struct{} // contains a record if batcher is paused
	flush                chan struct{} // contains a record if batcher should flush
	inflight             chan struct{} // tracks the number of inflight batches
	lastFlushWithRecords time.Time     // tracks the last time records were flushed

	// manage the phase
	phaseMutex sync.Mutex
	phase      int
	shutdown   sync.WaitGroup
	stop       chan bool

	// target needs to be threadsafe and changes frequently
	targetMutex sync.RWMutex
	target      uint32
}

// This method creates a new Batcher. Generally you should have 1 Batcher per datastore. Commonly after calling NewBatcher() you will chain
// some WithXXXX methods, for instance... `NewBatcher().WithRateLimiter(limiter)`.
func NewBatcher() IBatcher {
	return NewBatcherWithBuffer(10000)
}

func NewBatcherWithBuffer(maxBufferSize uint32) IBatcher {
	r := &Batcher{}
	r.buffer = NewBuffer(maxBufferSize)
	r.pause = make(chan struct{}, 1)
	r.flush = make(chan struct{}, 1)
	return r
}

// Use AzureSharedResource or ProvisionedResource as a rate limiter with Batcher to throttle the requests made against a datastore. This is
// optional; the default behavior does not rate limit.
func (r *Batcher) WithRateLimiter(rl RateLimiter) IBatcher {
	r.ratelimiter = rl
	return r
}

// The FlushInterval determines how often the processing loop attempts to flush buffered Operations. The default is `100ms`. If a rate limiter
// is being used, the interval determines the capacity that each flush has to work with. For instance, with the default 100ms and 10,000
// available capacity, there would be 10 flushes per second, each dispatching one or more batches of Operations that aim for 1,000 total
// capacity. If no rate limiter is used, each flush will attempt to empty the buffer.
func (r *Batcher) WithFlushInterval(val time.Duration) IBatcher {
	r.flushInterval = val
	return r
}

// The CapacityInterval determines how often the processing loop asks the rate limiter for capacity by calling GiveMe(). The default is
// `100ms`. The Batcher asks for capacity equal to every Operation's cost that has not been marked done. In other words, when you Enqueue()
// an Operation it increments a target based on cost. When you call done() on a batch (or the MaxOperationTime is exceeded), the target is
// decremented by the cost of all Operations in the batch. If there is no rate limiter attached, this interval does nothing.
func (r *Batcher) WithCapacityInterval(val time.Duration) IBatcher {
	r.capacityInterval = val
	return r
}

// The AuditInterval determines how often the target capacity is audited to ensure it still seems legitimate. The default is `10s`. The
// target capacity is the amount of capacity the Batcher thinks it needs to process all outstanding Operations. Only atomic operatios are
// performed on the target and there are other failsafes such as MaxOperationTime, however, since it is critical that the target capacity
// be correct, this is one final failsafe to ensure the Batcher isn't asking for the wrong capacity. Generally you should leave this set
// at the default.
func (r *Batcher) WithAuditInterval(val time.Duration) IBatcher {
	r.auditInterval = val
	return r
}

// The MaxOperationTime determines how long Batcher waits until marking a batch done after releasing it to the Watcher. The default is `1m`.
// You should always call the done() func when your batch has completed processing instead of relying on MaxOperationTime. The MaxOperationTime
// on Batcher will be superceded by MaxOperationTime on Watcher if provided.
func (r *Batcher) WithMaxOperationTime(val time.Duration) IBatcher {
	r.maxOperationTime = val
	return r
}

// The PauseTime determines how long Batcher suspends the processing loop once Pause() is called. The default is `500ms`. Typically, Pause()
// is called because errors are being received from the datastore such as TooManyRequests or Timeout. Pausing hopefully allows the datastore
// to catch up without making the problem worse.
func (r *Batcher) WithPauseTime(val time.Duration) IBatcher {
	r.pauseTime = val
	return r
}

// Setting this option changes Enqueue() such that it throws an error if the buffer is full. Normal behavior is for the Enqueue() func to
// block until it is able to add to the buffer.
func (r *Batcher) WithErrorOnFullBuffer() IBatcher {
	r.errorOnFullBuffer = true
	return r
}

// DO NOT SET THIS IN PRODUCTION. For unit tests, it may be beneficial to raise an event for each batch of operations.
func (r *Batcher) WithEmitBatch() IBatcher {
	r.emitBatch = true
	return r
}

// Generally you do not want this setting for production, but it can be helpful for unit tests to raise an event every time
// a flush is started and completed.
func (r *Batcher) WithEmitFlush() IBatcher {
	r.emitFlush = true
	return r
}

// Setting this option limits the number of batches that can be processed at a time to the provided value.
func (r *Batcher) WithMaxConcurrentBatches(val uint32) IBatcher {
	r.maxConcurrentBatches = val
	r.inflight = make(chan struct{}, val)
	return r
}

func (r *Batcher) applyDefaults() {
	if r.flushInterval <= 0 {
		r.flushInterval = 100 * time.Millisecond
	}
	if r.capacityInterval <= 0 {
		r.capacityInterval = 100 * time.Millisecond
	}
	if r.auditInterval <= 0 {
		r.auditInterval = 10 * time.Second
	}
	if r.maxOperationTime <= 0 {
		r.maxOperationTime = 1 * time.Minute
	}
	if r.pauseTime <= 0 {
		r.pauseTime = 500 * time.Millisecond
	}
}

// Call this method to add an Operation into the buffer.
func (r *Batcher) Enqueue(op IOperation) error {

	// ensure an operation was provided
	if op == nil {
		return NoOperationError{}
	}

	// ensure there is a watcher associated with the call
	watcher := op.Watcher()
	if op.Watcher() == nil {
		return NoWatcherError{}
	}

	// ensure the cost doesn't exceed max capacity
	if r.ratelimiter != nil && op.Cost() > r.ratelimiter.MaxCapacity() {
		return TooExpensiveError{}
	}

	// ensure there are not too many attempts
	maxAttempts := watcher.MaxAttempts()
	if maxAttempts > 0 && op.Attempt() >= maxAttempts {
		return TooManyAttemptsError{}
	}

	// increment the target
	r.incTarget(int(op.Cost()))

	// put into the buffer
	return r.buffer.Enqueue(op, r.errorOnFullBuffer)
}

// Call this method when your datastore is throwing transient errors. This pauses the processing loop to ensure that you are not flooding
// the datastore with additional data it cannot process making the situation worse.
func (r *Batcher) Pause() {

	// ensure pausing only happens when it is running
	r.phaseMutex.Lock()
	defer r.phaseMutex.Unlock()
	if r.phase != batcherPhaseStarted {
		// simply ignore an invalid pause
		return
	}

	// pause
	select {
	case r.pause <- struct{}{}:
		// successfully set the pause
	default:
		// pause was already set
	}

	// switch to paused phase
	r.phase = batcherPhasePaused

}

func (r *Batcher) resume() {
	r.phaseMutex.Lock()
	defer r.phaseMutex.Unlock()
	if r.phase == batcherPhasePaused {
		r.phase = batcherPhaseStarted
	}
}

// Call this method to manually flush as if the flushInterval were triggered.
func (r *Batcher) Flush() {

	// flush
	select {
	case r.flush <- struct{}{}:
		// successfully set the flush
	default:
		// flush was already set
	}

}

// This tells you how many operations are still in the buffer. This does not include operations that have been sent back to the Watcher as part
// of a batch for processing.
func (r *Batcher) OperationsInBuffer() uint32 {
	return r.buffer.Size()
}

// This tells you how much capacity the Batcher believes it needs to process everything outstanding. Outstanding operations include those in
// the buffer and operations and any that have been sent as a batch but not marked done yet.
func (r *Batcher) NeedsCapacity() uint32 {
	r.targetMutex.RLock()
	defer r.targetMutex.RUnlock()
	return r.target
}

func (r *Batcher) confirmTargetIsZero() bool {
	r.targetMutex.Lock()
	defer r.targetMutex.Unlock()
	if r.target > 0 {
		r.target = 0
		return false
	} else {
		return true
	}
}

func (r *Batcher) incTarget(val int) {
	r.targetMutex.Lock()
	defer r.targetMutex.Unlock()
	if val < 0 && r.target >= uint32(-val) {
		r.target += uint32(val)
	} else if val < 0 {
		r.target = 0
	} else if val > 0 {
		r.target += uint32(val)
	} // else is val=0, do nothing
}

func (r *Batcher) tryReserveBatchSlot() bool {
	if r.maxConcurrentBatches == 0 {
		return true
	}
	select {
	case r.inflight <- struct{}{}:
		return true
	default:
		return false
	}
}

func (r *Batcher) releaseBatchSlot() {
	if r.maxConcurrentBatches > 0 {
		<-r.inflight
	}
}

func (r *Batcher) confirmInflightIsZero() bool {
	isZero := true
	for {
		select {
		case <-r.inflight:
			isZero = false
		default:
			return isZero
		}
	}
}

func (r *Batcher) Inflight() uint32 {
	return uint32(len(r.inflight))
}

func (r *Batcher) processBatch(watcher IWatcher, batch []IOperation) {
	if len(batch) == 0 {
		return
	}
	r.lastFlushWithRecords = time.Now()

	// raise event
	if r.emitBatch {
		r.emit(BatchEvent, len(batch), "", batch)
	}

	go func() {

		// increment an attempt
		for _, op := range batch {
			op.MakeAttempt()
		}

		// process the batch
		waitForDone := make(chan struct{})
		go func() {
			defer close(waitForDone)
			watcher.ProcessBatch(batch)
		}()

		// the batch is "done" when the ProcessBatch func() finishes or the maxOperationTime is exceeded
		maxOperationTime := r.maxOperationTime
		if watcher.MaxOperationTime() > 0 {
			maxOperationTime = watcher.MaxOperationTime()
		}
		select {
		case <-waitForDone:
		case <-time.After(maxOperationTime):
		}

		// decrement target
		var total int = 0
		for _, op := range batch {
			total += int(op.Cost())
		}
		r.incTarget(-total)

		// remove from inflight
		r.releaseBatchSlot()

	}()
}

// Call this method to start the processing loop. The processing loop requests capacity at the CapacityInterval, organizes operations into
// batches at the FlushInterval, and audits the capacity target at the AuditInterval.
func (r *Batcher) Start() (err error) {

	// only allow one phase at a time
	r.phaseMutex.Lock()
	defer r.phaseMutex.Unlock()
	if r.phase != batcherPhaseUninitialized {
		err = BatcherImproperOrderError{}
		return
	}

	// ensure buffer was provisioned
	if r.buffer == nil || r.buffer.Max() == 0 {
		err = BufferNotAllocated{}
		return
	}

	// apply defaults
	r.applyDefaults()

	// start the timers
	capacityTimer := time.NewTicker(r.capacityInterval)
	flushTimer := time.NewTicker(r.flushInterval)
	auditTimer := time.NewTicker(r.auditInterval)

	// prepare for shutdown
	r.shutdown.Add(1)
	r.stop = make(chan bool)

	// process
	go func() {

		// shutdown
		defer func() {
			capacityTimer.Stop()
			flushTimer.Stop()
			auditTimer.Stop()
			r.buffer.Clear()
			r.emit(ShutdownEvent, 0, "", nil)
			r.shutdown.Done()
		}()

		// loop
		for {
			select {

			case <-r.stop:
				// no more writes; abort
				return

			case <-r.pause:
				// pause; typically this is requested because there is too much pressure on the datastore
				r.emit(PauseEvent, int(r.pauseTime.Milliseconds()), "", nil)
				time.Sleep(r.pauseTime)
				r.resume()
				r.emit(ResumeEvent, 0, "", nil)

			case <-auditTimer.C:
				// ensure that if the buffer is empty and everything should have been flushed, that target is set to 0
				if r.buffer.Size() == 0 && time.Since(r.lastFlushWithRecords) > r.maxOperationTime {
					targetIsZero := r.confirmTargetIsZero()
					inflightIsZero := r.confirmInflightIsZero()
					switch {
					case !targetIsZero && !inflightIsZero:
						r.emit(AuditFailEvent, 0, AuditMsgFailureOnTargetAndInflight, nil)
					case !targetIsZero:
						r.emit(AuditFailEvent, 0, AuditMsgFailureOnTarget, nil)
					case !inflightIsZero:
						r.emit(AuditFailEvent, 0, AuditMsgFailureOnInflight, nil)
					default:
						r.emit(AuditPassEvent, 0, "", nil)
					}
				} else {
					r.emit(AuditSkipEvent, 0, "", nil)
				}

			case <-capacityTimer.C:
				// ask for capacity
				if r.ratelimiter != nil {
					request := r.NeedsCapacity()
					r.emit(RequestEvent, int(request), "", nil)
					r.ratelimiter.GiveMe(request)
				}

			case <-flushTimer.C:
				r.Flush()

			case <-r.flush:
				// flush a percentage of the capacity (by default 10%)
				if r.emitFlush {
					r.emit(FlushStartEvent, 0, "", nil)
				}

				// determine the capacity
				enforceCapacity := r.ratelimiter != nil
				var capacity uint32
				if enforceCapacity {
					capacity += uint32(float64(r.ratelimiter.Capacity()) / 1000.0 * float64(r.flushInterval.Milliseconds()))
				}

				// if there are operations in the buffer, go up to the capacity
				batches := make(map[IWatcher][]IOperation)
				var consumed uint32 = 0

				// reset the buffer cursor to the top of the buffer
				op := r.buffer.Top()

				for {

					// NOTE: by requiring consumed to be higher than capacity we ensure the process always dispatches at least 1 operation
					if enforceCapacity && consumed > capacity {
						break
					}

					// the buffer is empty or we are at the end
					if op == nil {
						break
					}

					// batch
					switch {
					case op.IsBatchable():
						watcher := op.Watcher()
						batch, ok := batches[watcher]
						if (batch == nil || !ok) && !r.tryReserveBatchSlot() {
							op = r.buffer.Skip()
							continue // there is no batch slot available
						}
						consumed += op.Cost()
						batch = append(batch, op)
						max := watcher.MaxBatchSize()
						if max > 0 && len(batch) >= int(max) {
							r.processBatch(watcher, batch)
							batches[watcher] = nil
						} else {
							batches[watcher] = batch
						}
						op = r.buffer.Remove()
					case r.tryReserveBatchSlot():
						consumed += op.Cost()
						watcher := op.Watcher()
						r.processBatch(watcher, []IOperation{op})
						op = r.buffer.Remove()
					default:
						// there is no batch slot available
						op = r.buffer.Skip()
					}

				}

				// flush all batches that were seen
				for watcher, batch := range batches {
					r.processBatch(watcher, batch)
				}

				if r.emitFlush {
					r.emit(FlushDoneEvent, 0, "", nil)
				}
			}
		}

	}()

	// end starting
	r.phase = batcherPhaseStarted

	return
}

// Call this method to stop the processing loop. You may not restart after stopping.
func (r *Batcher) Stop() {

	// only allow one phase at a time
	r.phaseMutex.Lock()
	defer r.phaseMutex.Unlock()
	if r.phase == batcherPhaseStopped {
		// NOTE: there should be no need for callers to handle errors at Stop(), we will just ignore them
		return
	}

	// signal the stop
	if r.stop != nil {
		close(r.stop)
	}
	r.shutdown.Wait()

	// update the phase
	r.phase = batcherPhaseStopped

}
