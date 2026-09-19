package scripting

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
	"go.starlark.net/syntax"
	"monstermq.io/edge/internal/archive"
	"monstermq.io/edge/internal/stores"
)

// PublishedMessage records an outgoing MQTT publish from a script.
type PublishedMessage struct {
	Topic   string `json:"topic"`
	Payload string `json:"payload"`
	QoS     int    `json:"qos"`
	Retain  bool   `json:"retain"`
}

// ExecutionContext bundles host capabilities and context for a script execution.
type ExecutionContext struct {
	ScriptName string
	Trigger    string // "TOPIC", "TIMER", "CALLABLE", etc.
	Msg        *stores.BrokerMessage
	Args       map[string]any
	DryRun     bool

	State    *starlark.Dict
	Storage  *ScriptKVStore
	Global   *GlobalStore
	DB       *DatabaseManager
	Archives *archive.Manager
	Messages stores.MessageStore

	PublishFn func(topic string, payload []byte, retain bool, qos byte) error
	CallFn    func(name string, args map[string]any) (any, error)
	SubRegFn  func(filter string, callback starlark.Callable)

	Logger     *slog.Logger
	LogBuffer  *CircularLogBuffer
	Published  []PublishedMessage
	publishMu  sync.Mutex
}

// ScriptExecutionResult encapsulates output of a script execution.
type ScriptExecutionResult struct {
	Success          bool
	ReturnValue      any
	PublishedMessages []PublishedMessage
	Logs             []string
	Error            error
	ExecutionTimeMs  float64
}

// GlobalStore provides thread-safe broker-wide variable sharing across scripts.
type GlobalStore struct {
	mu   sync.RWMutex
	data map[string]any
}

func NewGlobalStore() *GlobalStore {
	return &GlobalStore{data: make(map[string]any)}
}

func (g *GlobalStore) Get(key string, defaultValue any) any {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if v, ok := g.data[key]; ok {
		return v
	}
	return defaultValue
}

func (g *GlobalStore) Set(key string, value any) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.data[key] = value
}

func (g *GlobalStore) Delete(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.data, key)
}

func (g *GlobalStore) List() map[string]any {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make(map[string]any, len(g.data))
	for k, v := range g.data {
		out[k] = v
	}
	return out
}

// Engine compiles and executes Starlark code.
type Engine struct {
	scriptName string
	code       string
	program    *starlark.Program
	maxSteps   uint64
}

// NewEngine parses and pre-compiles a Starlark script.
func NewEngine(scriptName, code string) (*Engine, error) {
	opts := &syntax.FileOptions{
		Set:             true,
		While:           false, // Disallow while loops to prevent infinite execution
		TopLevelControl: true,
	}

	_, prog, err := starlark.SourceProgramOptions(opts, scriptName+".star", code, func(name string) bool {
		return isPredeclared(name)
	})
	if err != nil {
		return nil, fmt.Errorf("compile script %s: %w", scriptName, err)
	}

	return &Engine{
		scriptName: scriptName,
		code:       code,
		program:    prog,
		maxSteps:   50000,
	}, nil
}

func (e *Engine) SetMaxExecutionSteps(steps uint64) {
	e.maxSteps = steps
}

func isPredeclared(name string) bool {
	switch name {
	case "msg", "args", "mqtt", "archive", "db", "state", "global", "globals", "shared", "storage", "scripts", "log", "console", "json":
		return true
	default:
		return false
	}
}

// Execute runs the pre-compiled Starlark program with the provided execution context.
func (e *Engine) Execute(ctx context.Context, execCtx *ExecutionContext, timeoutMs int64) *ScriptExecutionResult {
	start := time.Now()
	res := &ScriptExecutionResult{
		PublishedMessages: []PublishedMessage{},
		Logs:              []string{},
	}

	if timeoutMs <= 0 {
		timeoutMs = DefaultTimeoutMs
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()

	thread := &starlark.Thread{
		Name: e.scriptName,
		Print: func(_ *starlark.Thread, msg string) {
			if execCtx.LogBuffer != nil {
				execCtx.LogBuffer.Add("[PRINT] " + msg)
			}
			res.Logs = append(res.Logs, "[PRINT] "+msg)
			if execCtx.Logger != nil {
				execCtx.Logger.Info(msg, "script", e.scriptName, "origin", "print")
			}
		},
	}
	if e.maxSteps > 0 {
		thread.SetMaxExecutionSteps(e.maxSteps)
	}

	// Watchdog for context cancellation / timeout
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-done:
		case <-runCtx.Done():
			thread.Cancel("execution timeout")
		}
	}()

	predeclared := e.buildPredeclared(runCtx, execCtx, res)
	globals, err := e.program.Init(thread, predeclared)
	res.ExecutionTimeMs = float64(time.Since(start).Microseconds()) / 1000.0

	if err != nil {
		res.Success = false
		res.Error = err
		errMsg := fmt.Sprintf("Error: %v", err)
		res.Logs = append(res.Logs, errMsg)
		if execCtx.LogBuffer != nil {
			execCtx.LogBuffer.Add(errMsg)
		}
		return res
	}

	res.Success = true
	res.PublishedMessages = execCtx.Published

	// If the script returned or assigned a "result" or "return_value" variable, capture it
	if retVal, ok := globals["return_value"]; ok {
		res.ReturnValue, _ = ToGoValue(retVal)
	} else if retVal, ok := globals["result"]; ok {
		res.ReturnValue, _ = ToGoValue(retVal)
	}

	return res
}

func (e *Engine) buildPredeclared(ctx context.Context, execCtx *ExecutionContext, res *ScriptExecutionResult) starlark.StringDict {
	d := make(starlark.StringDict)

	// 1. msg
	if execCtx.Msg != nil {
		var payloadVal starlark.Value = starlark.String(string(execCtx.Msg.Payload))
		var parsed any
		if err := json.Unmarshal(execCtx.Msg.Payload, &parsed); err == nil {
			if sv, err := ToStarlarkValue(parsed); err == nil {
				payloadVal = sv
			}
		}

		msgDict := starlark.NewDict(6)
		_ = msgDict.SetKey(starlark.String("topic"), starlark.String(execCtx.Msg.TopicName))
		_ = msgDict.SetKey(starlark.String("payload"), payloadVal)
		_ = msgDict.SetKey(starlark.String("raw_payload"), starlark.String(string(execCtx.Msg.Payload)))
		_ = msgDict.SetKey(starlark.String("timestamp"), starlark.MakeInt64(execCtx.Msg.Time.UnixMilli()))
		_ = msgDict.SetKey(starlark.String("qos"), starlark.MakeInt(int(execCtx.Msg.QoS)))
		_ = msgDict.SetKey(starlark.String("retain"), starlark.Bool(execCtx.Msg.IsRetain))
		d["msg"] = msgDict
	} else {
		d["msg"] = starlark.None
	}

	// 2. args
	if execCtx.Args != nil {
		if sv, err := ToStarlarkValue(execCtx.Args); err == nil {
			d["args"] = sv
		} else {
			d["args"] = starlark.NewDict(0)
		}
	} else {
		d["args"] = starlark.NewDict(0)
	}

	// 3. state
	if execCtx.State != nil {
		d["state"] = execCtx.State
	} else {
		d["state"] = starlark.NewDict(0)
	}

	// 4. json module
	jsonModule := starlarkstruct.FromStringDict(starlarkstruct.Default, starlark.StringDict{
		"encode": starlark.NewBuiltin("json.encode", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			var v starlark.Value
			if err := starlark.UnpackPositionalArgs("json.encode", args, kwargs, 1, &v); err != nil {
				return nil, err
			}
			goVal, err := ToGoValue(v)
			if err != nil {
				return nil, err
			}
			b, err := json.Marshal(goVal)
			if err != nil {
				return nil, err
			}
			return starlark.String(string(b)), nil
		}),
		"decode": starlark.NewBuiltin("json.decode", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			var s string
			if err := starlark.UnpackPositionalArgs("json.decode", args, kwargs, 1, &s); err != nil {
				return nil, err
			}
			var parsed any
			if err := json.Unmarshal([]byte(s), &parsed); err != nil {
				return nil, fmt.Errorf("json.decode: %w", err)
			}
			return ToStarlarkValue(parsed)
		}),
	})
	d["json"] = jsonModule

	// 5. mqtt module
	mqttModule := starlarkstruct.FromStringDict(starlarkstruct.Default, starlark.StringDict{
		"publish": starlark.NewBuiltin("mqtt.publish", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			var (
				topic   string
				payload starlark.Value
				qos     int
				retain  bool
			)
			if err := starlark.UnpackArgs("mqtt.publish", args, kwargs, "topic", &topic, "payload", &payload, "qos?", &qos, "retain?", &retain); err != nil {
				return nil, err
			}

			if topic == "" {
				return nil, fmt.Errorf("mqtt.publish: topic must not be blank")
			}
			if strings.ContainsAny(topic, "+#") {
				return nil, fmt.Errorf("mqtt.publish: cannot publish to wildcard topic %q", topic)
			}

			payloadBytes, err := starlarkPayloadToBytes(payload)
			if err != nil {
				return nil, fmt.Errorf("mqtt.publish: %w", err)
			}

			execCtx.publishMu.Lock()
			execCtx.Published = append(execCtx.Published, PublishedMessage{
				Topic:   topic,
				Payload: string(payloadBytes),
				QoS:     qos,
				Retain:  retain,
			})
			execCtx.publishMu.Unlock()

			if !execCtx.DryRun && execCtx.PublishFn != nil {
				if err := execCtx.PublishFn(topic, payloadBytes, retain, byte(qos)); err != nil {
					return nil, fmt.Errorf("mqtt.publish: %w", err)
				}
			}
			return starlark.True, nil
		}),
		"subscribe": starlark.NewBuiltin("mqtt.subscribe", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			var (
				filter string
				fn     starlark.Callable
			)
			if err := starlark.UnpackArgs("mqtt.subscribe", args, kwargs, "filter", &filter, "callback", &fn); err != nil {
				return nil, err
			}
			if execCtx.SubRegFn != nil {
				execCtx.SubRegFn(filter, fn)
			}
			return starlark.True, nil
		}),
	})
	d["mqtt"] = mqttModule

	// 6. archive module
	archiveModule := starlarkstruct.FromStringDict(starlarkstruct.Default, starlark.StringDict{
		"get_last_value": starlark.NewBuiltin("archive.get_last_value", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			var (
				topic string
				group = "Default"
			)
			if err := starlark.UnpackArgs("archive.get_last_value", args, kwargs, "topic", &topic, "archive_group?", &group); err != nil {
				return nil, err
			}

			var msg *stores.BrokerMessage
			var err error
			if execCtx.Archives != nil {
				if g := execCtx.Archives.Get(group); g != nil && g.LastValue() != nil {
					msg, err = g.LastValue().Get(ctx, topic)
				}
			}
			if msg == nil && execCtx.Messages != nil {
				msg, err = execCtx.Messages.Get(ctx, topic)
			}
			if err != nil || msg == nil {
				return starlark.None, nil
			}

			dict := starlark.NewDict(4)
			_ = dict.SetKey(starlark.String("topic"), starlark.String(msg.TopicName))
			var parsed any
			if err := json.Unmarshal(msg.Payload, &parsed); err == nil {
				if sv, err := ToStarlarkValue(parsed); err == nil {
					_ = dict.SetKey(starlark.String("value"), sv)
				} else {
					_ = dict.SetKey(starlark.String("value"), starlark.String(string(msg.Payload)))
				}
			} else {
				_ = dict.SetKey(starlark.String("value"), starlark.String(string(msg.Payload)))
			}
			_ = dict.SetKey(starlark.String("timestamp"), starlark.MakeInt64(msg.Time.UnixMilli()))
			_ = dict.SetKey(starlark.String("qos"), starlark.MakeInt(int(msg.QoS)))
			return dict, nil
		}),
		"get_last_values": starlark.NewBuiltin("archive.get_last_values", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			var (
				pattern string
				limit   = 100
				group   = "Default"
			)
			if err := starlark.UnpackArgs("archive.get_last_values", args, kwargs, "pattern", &pattern, "limit?", &limit, "archive_group?", &group); err != nil {
				return nil, err
			}

			var lastStore stores.MessageStore
			if execCtx.Archives != nil {
				if g := execCtx.Archives.Get(group); g != nil {
					lastStore = g.LastValue()
				}
			}
			if lastStore == nil {
				lastStore = execCtx.Messages
			}
			if lastStore == nil {
				return starlark.NewList([]starlark.Value{}), nil
			}

			items := []starlark.Value{}
			_ = lastStore.FindMatchingMessages(ctx, pattern, func(m stores.BrokerMessage) bool {
				dict := starlark.NewDict(3)
				_ = dict.SetKey(starlark.String("topic"), starlark.String(m.TopicName))
				_ = dict.SetKey(starlark.String("value"), starlark.String(string(m.Payload)))
				_ = dict.SetKey(starlark.String("timestamp"), starlark.MakeInt64(m.Time.UnixMilli()))
				items = append(items, dict)
				return len(items) < limit
			})
			return starlark.NewList(items), nil
		}),
		"get_history": starlark.NewBuiltin("archive.get_history", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			var (
				topic string
				fromStr, toStr string
				limit = 100
				group = "Default"
			)
			if err := starlark.UnpackArgs("archive.get_history", args, kwargs, "topic", &topic, "from_time?", &fromStr, "to_time?", &toStr, "limit?", &limit, "archive_group?", &group); err != nil {
				return nil, err
			}

			if execCtx.Archives == nil {
				return starlark.NewList([]starlark.Value{}), nil
			}
			g := execCtx.Archives.Get(group)
			if g == nil || g.Archive() == nil {
				return starlark.NewList([]starlark.Value{}), nil
			}

			var from, to *time.Time
			if fromStr != "" {
				if t, err := time.Parse(time.RFC3339, fromStr); err == nil {
					from = &t
				}
			}
			if toStr != "" {
				if t, err := time.Parse(time.RFC3339, toStr); err == nil {
					to = &t
				}
			}

			rows, err := g.Archive().GetHistory(ctx, topic, from, to, limit)
			if err != nil {
				return nil, err
			}

			items := make([]starlark.Value, len(rows))
			for i, r := range rows {
				dict := starlark.NewDict(4)
				_ = dict.SetKey(starlark.String("topic"), starlark.String(r.Topic))
				_ = dict.SetKey(starlark.String("payload"), starlark.String(string(r.Payload)))
				_ = dict.SetKey(starlark.String("timestamp"), starlark.MakeInt64(r.Timestamp.UnixMilli()))
				_ = dict.SetKey(starlark.String("qos"), starlark.MakeInt(int(r.QoS)))
				items[i] = dict
			}
			return starlark.NewList(items), nil
		}),
	})
	d["archive"] = archiveModule

	// 7. db module
	dbModule := starlarkstruct.FromStringDict(starlarkstruct.Default, starlark.StringDict{
		"query": starlark.NewBuiltin("db.query", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			connName, queryStr, goArgs, err := parseDbArgs("db.query", args, kwargs)
			if err != nil {
				return nil, err
			}
			if execCtx.DB == nil {
				return nil, fmt.Errorf("db.query: database manager not initialized")
			}

			rows, err := execCtx.DB.Query(ctx, connName, queryStr, goArgs)
			if err != nil {
				return nil, err
			}

			return ToStarlarkValue(rows)
		}),
		"execute": starlark.NewBuiltin("db.execute", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			connName, queryStr, goArgs, err := parseDbArgs("db.execute", args, kwargs)
			if err != nil {
				return nil, err
			}
			if execCtx.DB == nil {
				return nil, fmt.Errorf("db.execute: database manager not initialized")
			}

			resMap, err := execCtx.DB.Execute(ctx, connName, queryStr, goArgs)
			if err != nil {
				return nil, err
			}
			return ToStarlarkValue(resMap)
		}),
	})
	d["db"] = dbModule

	// 8. global module
	if execCtx.Global != nil {
		globalModule := starlarkstruct.FromStringDict(starlarkstruct.Default, starlark.StringDict{
			"get": starlark.NewBuiltin("global.get", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
				var key string
				var defVal starlark.Value = starlark.None
				if err := starlark.UnpackArgs("global.get", args, kwargs, "key", &key, "default?", &defVal); err != nil {
					return nil, err
				}
				goDef, _ := ToGoValue(defVal)
				val := execCtx.Global.Get(key, goDef)
				return ToStarlarkValue(val)
			}),
			"set": starlark.NewBuiltin("global.set", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
				var key string
				var val starlark.Value
				if err := starlark.UnpackArgs("global.set", args, kwargs, "key", &key, "value", &val); err != nil {
					return nil, err
				}
				goVal, _ := ToGoValue(val)
				execCtx.Global.Set(key, goVal)
				return starlark.None, nil
			}),
			"delete": starlark.NewBuiltin("global.delete", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
				var key string
				if err := starlark.UnpackArgs("global.delete", args, kwargs, "key", &key); err != nil {
					return nil, err
				}
				execCtx.Global.Delete(key)
				return starlark.None, nil
			}),
			"list": starlark.NewBuiltin("global.list", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
				return ToStarlarkValue(execCtx.Global.List())
			}),
		})
		d["global"] = globalModule
		d["globals"] = globalModule
		d["shared"] = globalModule
	}

	// 9. storage module
	if execCtx.Storage != nil {
		storageModule := starlarkstruct.FromStringDict(starlarkstruct.Default, starlark.StringDict{
			"get": starlark.NewBuiltin("storage.get", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
				var key string
				var defVal starlark.Value = starlark.None
				if err := starlark.UnpackArgs("storage.get", args, kwargs, "key", &key, "default?", &defVal); err != nil {
					return nil, err
				}
				goDef, _ := ToGoValue(defVal)
				val := execCtx.Storage.Get(key, goDef)
				return ToStarlarkValue(val)
			}),
			"set": starlark.NewBuiltin("storage.set", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
				var key string
				var val starlark.Value
				if err := starlark.UnpackArgs("storage.set", args, kwargs, "key", &key, "value", &val); err != nil {
					return nil, err
				}
				goVal, _ := ToGoValue(val)
				if err := execCtx.Storage.Set(key, goVal); err != nil {
					return nil, err
				}
				return starlark.None, nil
			}),
			"delete": starlark.NewBuiltin("storage.delete", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
				var key string
				if err := starlark.UnpackArgs("storage.delete", args, kwargs, "key", &key); err != nil {
					return nil, err
				}
				if err := execCtx.Storage.Delete(key); err != nil {
					return nil, err
				}
				return starlark.None, nil
			}),
			"list": starlark.NewBuiltin("storage.list", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
				return ToStarlarkValue(execCtx.Storage.List())
			}),
		})
		d["storage"] = storageModule
	}

	// 10. scripts module (inter-script calls)
	scriptsModule := starlarkstruct.FromStringDict(starlarkstruct.Default, starlark.StringDict{
		"call": starlark.NewBuiltin("scripts.call", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			var (
				name string
				callArgs *starlark.Dict
			)
			if err := starlark.UnpackArgs("scripts.call", args, kwargs, "name", &name, "args?", &callArgs); err != nil {
				return nil, err
			}
			if execCtx.CallFn == nil {
				return nil, fmt.Errorf("scripts.call: inter-script caller not configured")
			}

			argsMap := make(map[string]any)
			if callArgs != nil {
				for _, item := range callArgs.Items() {
					k := item[0].(starlark.String).GoString()
					v, _ := ToGoValue(item[1])
					argsMap[k] = v
				}
			}

			callRes, err := execCtx.CallFn(name, argsMap)
			if err != nil {
				return nil, fmt.Errorf("scripts.call %s: %w", name, err)
			}
			return ToStarlarkValue(callRes)
		}),
	})
	d["scripts"] = scriptsModule

	// 11. log & console
	logFn := func(prefix string) func(*starlark.Thread, *starlark.Builtin, starlark.Tuple, []starlark.Tuple) (starlark.Value, error) {
		return func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
			parts := make([]string, len(args))
			for i, a := range args {
				if s, ok := a.(starlark.String); ok {
					parts[i] = s.GoString()
				} else {
					parts[i] = a.String()
				}
			}
			line := fmt.Sprintf("[%s] %s", prefix, strings.Join(parts, " "))
			res.Logs = append(res.Logs, line)
			if execCtx.LogBuffer != nil {
				execCtx.LogBuffer.Add(line)
			}
			if execCtx.Logger != nil {
				switch prefix {
				case "ERROR":
					execCtx.Logger.Error(line, "script", e.scriptName)
				case "WARN":
					execCtx.Logger.Warn(line, "script", e.scriptName)
				default:
					execCtx.Logger.Info(line, "script", e.scriptName)
				}
			}
			return starlark.None, nil
		}
	}

	logModule := starlarkstruct.FromStringDict(starlarkstruct.Default, starlark.StringDict{
		"info":  starlark.NewBuiltin("log.info", logFn("INFO")),
		"warn":  starlark.NewBuiltin("log.warn", logFn("WARN")),
		"error": starlark.NewBuiltin("log.error", logFn("ERROR")),
		"debug": starlark.NewBuiltin("log.debug", logFn("DEBUG")),
	})
	d["log"] = logModule

	consoleModule := starlarkstruct.FromStringDict(starlarkstruct.Default, starlark.StringDict{
		"log":   starlark.NewBuiltin("console.log", logFn("LOG")),
		"warn":  starlark.NewBuiltin("console.warn", logFn("WARN")),
		"error": starlark.NewBuiltin("console.error", logFn("ERROR")),
	})
	d["console"] = consoleModule

	return d
}

func starlarkPayloadToBytes(v starlark.Value) ([]byte, error) {
	switch val := v.(type) {
	case starlark.String:
		return []byte(val.GoString()), nil
	case starlark.Bytes:
		return []byte(string(val)), nil
	default:
		goVal, err := ToGoValue(val)
		if err != nil {
			return nil, err
		}
		return json.Marshal(goVal)
	}
}

// ToStarlarkValue converts a standard Go primitive into a Starlark Value.
func ToStarlarkValue(v any) (starlark.Value, error) {
	if v == nil {
		return starlark.None, nil
	}
	switch val := v.(type) {
	case bool:
		return starlark.Bool(val), nil
	case string:
		return starlark.String(val), nil
	case []byte:
		return starlark.String(string(val)), nil
	case int:
		return starlark.MakeInt(val), nil
	case int32:
		return starlark.MakeInt64(int64(val)), nil
	case int64:
		return starlark.MakeInt64(val), nil
	case uint:
		return starlark.MakeUint(val), nil
	case uint64:
		return starlark.MakeUint64(val), nil
	case float32:
		return starlark.Float(float64(val)), nil
	case float64:
		return starlark.Float(val), nil
	case map[string]any:
		dict := starlark.NewDict(len(val))
		for k, item := range val {
			sv, err := ToStarlarkValue(item)
			if err != nil {
				return nil, err
			}
			if err := dict.SetKey(starlark.String(k), sv); err != nil {
				return nil, err
			}
		}
		return dict, nil
	case []any:
		elems := make([]starlark.Value, len(val))
		for i, item := range val {
			sv, err := ToStarlarkValue(item)
			if err != nil {
				return nil, err
			}
			elems[i] = sv
		}
		return starlark.NewList(elems), nil
	case []map[string]any:
		elems := make([]starlark.Value, len(val))
		for i, item := range val {
			sv, err := ToStarlarkValue(item)
			if err != nil {
				return nil, err
			}
			elems[i] = sv
		}
		return starlark.NewList(elems), nil
	default:
		// Fallback via JSON roundtrip
		b, err := json.Marshal(v)
		if err != nil {
			return starlark.String(fmt.Sprintf("%v", v)), nil
		}
		var parsed any
		if err := json.Unmarshal(b, &parsed); err == nil {
			return ToStarlarkValue(parsed)
		}
		return starlark.String(string(b)), nil
	}
}

// ToGoValue converts a Starlark Value back into a native Go data structure.
func ToGoValue(v starlark.Value) (any, error) {
	if v == nil || v == starlark.None {
		return nil, nil
	}
	switch val := v.(type) {
	case starlark.Bool:
		return bool(val), nil
	case starlark.String:
		return val.GoString(), nil
	case starlark.Bytes:
		return string(val), nil
	case starlark.Int:
		if i, ok := val.Int64(); ok {
			return i, nil
		}
		return val.BigInt(), nil
	case starlark.Float:
		return float64(val), nil
	case *starlark.List:
		out := make([]any, val.Len())
		for i := 0; i < val.Len(); i++ {
			gv, err := ToGoValue(val.Index(i))
			if err != nil {
				return nil, err
			}
			out[i] = gv
		}
		return out, nil
	case starlark.Tuple:
		out := make([]any, val.Len())
		for i := 0; i < val.Len(); i++ {
			gv, err := ToGoValue(val.Index(i))
			if err != nil {
				return nil, err
			}
			out[i] = gv
		}
		return out, nil
	case *starlark.Dict:
		out := make(map[string]any, val.Len())
		for _, item := range val.Items() {
			k := item[0].String()
			if s, ok := item[0].(starlark.String); ok {
				k = s.GoString()
			}
			gv, err := ToGoValue(item[1])
			if err != nil {
				return nil, err
			}
			out[k] = gv
		}
		return out, nil
	default:
		return val.String(), nil
	}
}

func parseDbArgs(fnName string, args starlark.Tuple, kwargs []starlark.Tuple) (string, string, []any, error) {
	var connName, queryStr string
	var queryArgs []any

	// Check kwargs first
	for _, kw := range kwargs {
		k := string(kw[0].(starlark.String))
		switch k {
		case "conn", "conn_name":
			if s, ok := starlark.AsString(kw[1]); ok {
				connName = s
			}
		case "sql", "query":
			if s, ok := starlark.AsString(kw[1]); ok {
				queryStr = s
			}
		case "args":
			if list, ok := kw[1].(*starlark.List); ok {
				for i := 0; i < list.Len(); i++ {
					gv, _ := ToGoValue(list.Index(i))
					queryArgs = append(queryArgs, gv)
				}
			}
		}
	}

	// Positional arguments
	if queryStr == "" {
		if len(args) == 0 {
			return "", "", nil, fmt.Errorf("%s: requires at least SQL statement argument", fnName)
		}
		if len(args) == 1 {
			s, ok := starlark.AsString(args[0])
			if !ok {
				return "", "", nil, fmt.Errorf("%s: argument 1 must be string (sql)", fnName)
			}
			queryStr = s
		} else if len(args) == 2 {
			if list, ok := args[1].(*starlark.List); ok {
				s, ok := starlark.AsString(args[0])
				if !ok {
					return "", "", nil, fmt.Errorf("%s: argument 1 must be string (sql)", fnName)
				}
				queryStr = s
				for i := 0; i < list.Len(); i++ {
					gv, _ := ToGoValue(list.Index(i))
					queryArgs = append(queryArgs, gv)
				}
			} else {
				s1, ok1 := starlark.AsString(args[0])
				s2, ok2 := starlark.AsString(args[1])
				if !ok1 || !ok2 {
					return "", "", nil, fmt.Errorf("%s: invalid string arguments", fnName)
				}
				connName = s1
				queryStr = s2
			}
		} else if len(args) >= 3 {
			s1, ok1 := starlark.AsString(args[0])
			s2, ok2 := starlark.AsString(args[1])
			if !ok1 || !ok2 {
				return "", "", nil, fmt.Errorf("%s: invalid string arguments", fnName)
			}
			connName = s1
			queryStr = s2
			if list, ok := args[2].(*starlark.List); ok {
				for i := 0; i < list.Len(); i++ {
					gv, _ := ToGoValue(list.Index(i))
					queryArgs = append(queryArgs, gv)
				}
			}
		}
	} else if len(args) > 0 && queryArgs == nil {
		if list, ok := args[0].(*starlark.List); ok {
			for i := 0; i < list.Len(); i++ {
				gv, _ := ToGoValue(list.Index(i))
				queryArgs = append(queryArgs, gv)
			}
		}
	}

	if queryStr == "" {
		return "", "", nil, fmt.Errorf("%s: missing SQL statement", fnName)
	}
	return connName, queryStr, queryArgs, nil
}
