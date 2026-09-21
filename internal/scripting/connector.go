package scripting

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"go.starlark.net/starlark"
	"monstermq.io/edge/internal/archive"
	"monstermq.io/edge/internal/pubsub"
	"monstermq.io/edge/internal/stores"
)

// DynamicSubscription tracks dynamic in-script subscriptions created via mqtt.subscribe().
type DynamicSubscription struct {
	ID       int
	Filter   string
	Callback starlark.Callable
	StopCh   chan struct{}
}

// Connector manages the runtime lifecycle and execution of a single Script device.
type Connector struct {
	name      string
	cfg       ScriptConfig
	engine    *Engine
	logger    *slog.Logger
	nodeID    string

	// Host capabilities
	storage      *ScriptKVStore
	global       *GlobalStore
	db           *DatabaseManager
	archives     *archive.Manager
	messages     stores.MessageStore
	publishFn    func(topic string, payload []byte, retain bool, qos byte) error
	bus          *pubsub.Bus
	callScriptFn func(name string, args map[string]any) (any, error)

	// State and metrics
	state               *starlark.Dict
	logBuffer           *CircularLogBuffer
	executionCount      atomic.Int64
	errorCount          atomic.Int64
	lastExecutionTime   atomic.Pointer[string]
	lastExecutionStatus atomic.Pointer[string]

	// Concurrency & lifecycle
	execMu        sync.Mutex // enforces SINGLETON mode execution serialization
	stopCh        chan struct{}
	wg            sync.WaitGroup
	subID         int
	lastPayloads  map[string][]byte
	payloadMu     sync.RWMutex
	dynamicSubs   map[string]*DynamicSubscription
	dynamicSubMu  sync.Mutex
}

func NewConnector(
	name string,
	cfg ScriptConfig,
	engine *Engine,
	devStore stores.DeviceConfigStore,
	global *GlobalStore,
	db *DatabaseManager,
	archives *archive.Manager,
	messages stores.MessageStore,
	publishFn func(topic string, payload []byte, retain bool, qos byte) error,
	bus *pubsub.Bus,
	callScriptFn func(name string, args map[string]any) (any, error),
	nodeID string,
	logger *slog.Logger,
) *Connector {
	c := &Connector{
		name:         name,
		cfg:          cfg,
		engine:       engine,
		logger:       logger,
		nodeID:       nodeID,
		storage:      NewScriptKVStore(name, devStore, nodeID),
		global:       global,
		db:           db,
		archives:     archives,
		messages:     messages,
		publishFn:    publishFn,
		bus:          bus,
		callScriptFn: callScriptFn,
		state:        starlark.NewDict(0),
		logBuffer:    NewCircularLogBuffer(100),
		stopCh:       make(chan struct{}),
		lastPayloads: make(map[string][]byte),
		dynamicSubs:  make(map[string]*DynamicSubscription),
	}

	initStatus := "READY"
	c.lastExecutionStatus.Store(&initStatus)
	return c
}

// Start launches background subscriptions and timers for this script.
func (c *Connector) Start(ctx context.Context) error {
	// Preload persistent storage
	if err := c.storage.Load(ctx); err != nil {
		c.logger.Warn("failed to load script storage", "script", c.name, "err", err)
	}

	// 1. Topic subscriptions
	if (c.cfg.TriggerType == TriggerTypeTopic || c.cfg.TriggerType == TriggerTypeBoth) && len(c.cfg.TopicFilters) > 0 && c.bus != nil {
		subID, ch := c.bus.Subscribe(c.cfg.TopicFilters, 128)
		c.subID = subID
		c.wg.Add(1)
		go c.topicWorker(ch)
	}

	// 2. Periodic interval timer
	if (c.cfg.TriggerType == TriggerTypeTimer || c.cfg.TriggerType == TriggerTypeBoth) && c.cfg.TimerIntervalMs > 0 {
		c.wg.Add(1)
		go c.timerWorker()
	}

	c.logger.Info("script started", "name", c.name, "trigger", c.cfg.TriggerType, "filters", c.cfg.TopicFilters)
	return nil
}

// Stop terminates all background workers and unregisters subscriptions.
func (c *Connector) Stop() {
	select {
	case <-c.stopCh:
		return
	default:
		close(c.stopCh)
	}

	if c.subID > 0 && c.bus != nil {
		c.bus.Unsubscribe(c.subID)
	}

	c.dynamicSubMu.Lock()
	for _, ds := range c.dynamicSubs {
		if c.bus != nil {
			c.bus.Unsubscribe(ds.ID)
		}
		close(ds.StopCh)
	}
	c.dynamicSubs = make(map[string]*DynamicSubscription)
	c.dynamicSubMu.Unlock()

	c.wg.Wait()
	c.logger.Info("script stopped", "name", c.name)
}

func (c *Connector) topicWorker(ch <-chan stores.BrokerMessage) {
	defer c.wg.Done()
	for {
		select {
		case <-c.stopCh:
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			if c.cfg.TriggerOnChangeOnly {
				c.payloadMu.Lock()
				last, exists := c.lastPayloads[msg.TopicName]
				if exists && bytes.Equal(last, msg.Payload) {
					c.payloadMu.Unlock()
					continue
				}
				// Copy to cache
				cached := make([]byte, len(msg.Payload))
				copy(cached, msg.Payload)
				c.lastPayloads[msg.TopicName] = cached
				c.payloadMu.Unlock()
			}

			c.runExecution(&msg, nil, false, "TOPIC", msg.Time)
		}
	}
}

func (c *Connector) timerWorker() {
	defer c.wg.Done()

	intervalMs := c.cfg.TimerIntervalMs
	if intervalMs <= 0 {
		intervalMs = 1000
	}

	for {
		now := time.Now()
		var delay time.Duration
		var scheduledTime time.Time

		if intervalMs >= 1000 {
			nowUnixMs := now.UnixMilli()
			nextBoundaryMs := ((nowUnixMs / intervalMs) + 1) * intervalMs
			delayMs := nextBoundaryMs - nowUnixMs
			if delayMs <= 0 {
				delayMs = 1
			}
			delay = time.Duration(delayMs) * time.Millisecond
			scheduledTime = time.UnixMilli(nextBoundaryMs)
		} else {
			delay = time.Duration(intervalMs) * time.Millisecond
			scheduledTime = now.Add(delay)
		}

		timer := time.NewTimer(delay)
		select {
		case <-c.stopCh:
			timer.Stop()
			return
		case <-timer.C:
			c.runExecution(nil, nil, false, "TIMER", scheduledTime)
		}
	}
}

func (c *Connector) registerDynamicSubscription(filter string, callback starlark.Callable) {
	if c.bus == nil {
		return
	}
	c.dynamicSubMu.Lock()
	defer c.dynamicSubMu.Unlock()

	// Replace existing if already subscribed to same filter
	if existing, ok := c.dynamicSubs[filter]; ok {
		c.bus.Unsubscribe(existing.ID)
		close(existing.StopCh)
		delete(c.dynamicSubs, filter)
	}

	subID, ch := c.bus.Subscribe([]string{filter}, 64)
	stopCh := make(chan struct{})
	ds := &DynamicSubscription{
		ID:       subID,
		Filter:   filter,
		Callback: callback,
		StopCh:   stopCh,
	}
	c.dynamicSubs[filter] = ds

	go func() {
		for {
			select {
			case <-c.stopCh:
				return
			case <-stopCh:
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				c.runDynamicCallback(ds.Callback, &msg)
			}
		}
	}()
}

func (c *Connector) runDynamicCallback(fn starlark.Callable, msg *stores.BrokerMessage) {
	if c.cfg.InstanceMode == InstanceModeSingleton {
		c.execMu.Lock()
		defer c.execMu.Unlock()
	}

	thread := &starlark.Thread{Name: c.name + "-dyn"}
	thread.SetMaxExecutionSteps(50000)

	var payloadVal starlark.Value = starlark.String(string(msg.Payload))
	var parsed any
	if err := json.Unmarshal(msg.Payload, &parsed); err == nil {
		if sv, err := ToStarlarkValue(parsed); err == nil {
			payloadVal = sv
		}
	}

	msgDict := starlark.NewDict(5)
	_ = msgDict.SetKey(starlark.String("topic"), starlark.String(msg.TopicName))
	_ = msgDict.SetKey(starlark.String("payload"), payloadVal)
	_ = msgDict.SetKey(starlark.String("timestamp"), starlark.MakeInt64(msg.Time.UnixMilli()))
	_ = msgDict.SetKey(starlark.String("qos"), starlark.MakeInt(int(msg.QoS)))
	_ = msgDict.SetKey(starlark.String("retain"), starlark.Bool(msg.IsRetain))

	_, err := starlark.Call(thread, fn, starlark.Tuple{msgDict}, nil)
	if err != nil {
		c.logBuffer.Add("[CALLBACK_ERROR] " + err.Error())
		c.logger.Warn("dynamic subscription callback error", "script", c.name, "err", err)
	}
}

func (c *Connector) runExecution(msg *stores.BrokerMessage, customArgs map[string]any, dryRun bool, triggerType string, triggerTime time.Time) *ScriptExecutionResult {
	if c.cfg.InstanceMode == InstanceModeSingleton {
		c.execMu.Lock()
		defer c.execMu.Unlock()
	}

	if triggerType == "" {
		triggerType = string(c.cfg.TriggerType)
	}
	if triggerTime.IsZero() {
		if msg != nil && !msg.Time.IsZero() {
			triggerTime = msg.Time
		} else {
			triggerTime = time.Now()
		}
	}

	execCtx := &ExecutionContext{
		ScriptName:  c.name,
		Trigger:     triggerType,
		TriggerTime: triggerTime,
		Msg:         msg,
		Args:        customArgs,
		DryRun:      dryRun,
		State:       c.state,
		Storage:     c.storage,
		Global:      c.global,
		DB:          c.db,
		Archives:    c.archives,
		Messages:    c.messages,
		PublishFn:   c.publishFn,
		CallFn:      c.callScriptFn,
		SubRegFn:    c.registerDynamicSubscription,
		Logger:      c.logger,
		LogBuffer:   c.logBuffer,
	}

	ctx := context.Background()
	res := c.engine.Execute(ctx, execCtx, c.cfg.TimeoutMs)

	c.executionCount.Add(1)
	nowStr := time.Now().UTC().Format(time.RFC3339)
	c.lastExecutionTime.Store(&nowStr)

	if res.Success {
		status := "SUCCESS"
		c.lastExecutionStatus.Store(&status)
	} else {
		c.errorCount.Add(1)
		status := "ERROR"
		c.lastExecutionStatus.Store(&status)
	}

	return res
}

// Call executes the script synchronously with custom arguments (e.g. from scripts.call).
func (c *Connector) Call(args map[string]any) (any, error) {
	res := c.runExecution(nil, args, false, "CALLABLE", time.Now())
	if !res.Success {
		return nil, res.Error
	}
	return res.ReturnValue, nil
}

// Test executes the script in isolation without network side effects (dry-run).
func (c *Connector) Test(testTopic, testPayload string, testArgs map[string]any) *ScriptExecutionResult {
	var msg *stores.BrokerMessage
	var triggerTime time.Time
	if testTopic != "" || testPayload != "" {
		triggerTime = time.Now()
		msg = &stores.BrokerMessage{
			TopicName: testTopic,
			Payload:   []byte(testPayload),
			Time:      triggerTime,
			QoS:       0,
			IsRetain:  false,
		}
	} else {
		triggerTime = time.Now()
	}
	return c.runExecution(msg, testArgs, true, "TEST", triggerTime)
}

// Stats returns the live execution statistics and metadata.
func (c *Connector) Stats() (execCount, errCount int64, lastTime, lastStatus string, recentLogs []string) {
	execCount = c.executionCount.Load()
	errCount = c.errorCount.Load()
	if t := c.lastExecutionTime.Load(); t != nil {
		lastTime = *t
	}
	if s := c.lastExecutionStatus.Load(); s != nil {
		lastStatus = *s
	}
	recentLogs = c.logBuffer.Snapshot()
	return
}
