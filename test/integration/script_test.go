package integration

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"monstermq.io/edge/internal/config"
)

func TestScriptingSubsystemIntegration(t *testing.T) {
	mqttPort := 21920
	gqlPort := 21921

	srv, gqlURL := startWithGraphQL(t, mqttPort, gqlPort, func(c *config.Config) {
		c.Features.PythonScripts = true
		c.SQLite.Path = filepath.Join(t.TempDir(), "scripts_test.db")
	})
	defer srv.Close()

	// 1. Verify enabledFeatures includes PythonScripts
	featuresQuery := `query { broker { enabledFeatures } }`
	resp := gqlQuery(t, gqlURL, featuresQuery, nil)
	features := resp["broker"].(map[string]any)["enabledFeatures"].([]any)
	hasPythonScripts := false
	for _, f := range features {
		if f.(string) == "PythonScripts" {
			hasPythonScripts = true
			break
		}
	}
	if !hasPythonScripts {
		t.Fatalf("expected PythonScripts in enabledFeatures, got %v", features)
	}

	// 2. Test script.test mutation (dry-run)
	testMutation := `
mutation TestScript($input: ScriptInput!, $topic: String, $payload: String) {
    script {
        test(input: $input, testTopic: $topic, testPayload: $payload) {
            success
            returnValue
            outputMessages {
                topic
                payload
                qos
                retain
            }
            logs
            errors
            executionTimeMs
        }
    }
}
`
	testScriptCode := `
if msg != None:
    temp = msg["payload"]["temperature"]
    log.info("Processing temperature: " + str(temp))
    if temp > 50:
        mqtt.publish("alerts/high_temp", json.encode({"val": temp, "status": "ALARM"}), retain=True)
`
	testVars := map[string]any{
		"input": map[string]any{
			"name":      "DryRunTest",
			"namespace": "script",
			"nodeId":    fmt.Sprintf("g-%d", gqlPort),
			"enabled":   true,
			"config": map[string]any{
				"language":     "starlark",
				"script":       testScriptCode,
				"triggerType":  "TOPIC",
				"topicFilters": []any{"sensors/+"},
				"instanceMode": "SINGLETON",
				"timeoutMs":    500,
			},
		},
		"topic":   "sensors/chamber1",
		"payload": `{"temperature": 65.2}`,
	}

	testResp := gqlQuery(t, gqlURL, testMutation, testVars)
	testData := testResp["script"].(map[string]any)["test"].(map[string]any)
	if !testData["success"].(bool) {
		t.Fatalf("dry run test failed: %v", testData["errors"])
	}
	outputMsgs := testData["outputMessages"].([]any)
	if len(outputMsgs) != 1 {
		t.Fatalf("expected 1 output message, got %d", len(outputMsgs))
	}
	out0 := outputMsgs[0].(map[string]any)
	if out0["topic"] != "alerts/high_temp" || !out0["retain"].(bool) {
		t.Fatalf("unexpected output message: %v", out0)
	}
	if !strings.Contains(out0["payload"].(string), `"status":"ALARM"`) {
		t.Fatalf("unexpected payload in output message: %s", out0["payload"])
	}

	// 3. Create persistent Script via GraphQL
	createMutation := `
mutation CreateScript($input: ScriptInput!) {
    script {
        create(input: $input) {
            success
            errors
            script {
                name
                enabled
                isOnCurrentNode
                config {
                    language
                    triggerType
                    topicFilters
                }
            }
        }
    }
}
`
	liveScriptCode := `
if msg != None:
    val = float(msg["raw_payload"])
    globals.set("last_reading", val)
    mqtt.publish("calc/squared", str(val * val), retain=False, qos=0)
`
	createVars := map[string]any{
		"input": map[string]any{
			"name":      "SquareCalculator",
			"namespace": "script",
			"nodeId":    fmt.Sprintf("g-%d", gqlPort),
			"enabled":   true,
			"config": map[string]any{
				"language":     "starlark",
				"script":       liveScriptCode,
				"triggerType":  "TOPIC",
				"topicFilters": []any{"calc/input"},
				"instanceMode": "SINGLETON",
				"timeoutMs":    500,
			},
		},
	}

	createResp := gqlQuery(t, gqlURL, createMutation, createVars)
	createData := createResp["script"].(map[string]any)["create"].(map[string]any)
	if !createData["success"].(bool) {
		t.Fatalf("create script failed: %v", createData["errors"])
	}
	sObj := createData["script"].(map[string]any)
	if sObj["name"] != "SquareCalculator" || !sObj["enabled"].(bool) {
		t.Fatalf("unexpected created script: %v", sObj)
	}

	// 4. Query script list
	listQuery := `query { scripts { name enabled isOnCurrentNode } }`
	listResp := gqlQuery(t, gqlURL, listQuery, nil)
	scriptsList := listResp["scripts"].([]any)
	if len(scriptsList) != 1 {
		t.Fatalf("expected 1 script in list, got %d", len(scriptsList))
	}

	// 5. Connect MQTT client and subscribe to calc/squared
	opts := mqtt.NewClientOptions().
		AddBroker(fmt.Sprintf("tcp://127.0.0.1:%d", mqttPort)).
		SetClientID("test-script-runner")
	cl := mqtt.NewClient(opts)
	if tok := cl.Connect(); tok.Wait() && tok.Error() != nil {
		t.Fatalf("mqtt connect: %v", tok.Error())
	}
	defer cl.Disconnect(250)

	var receivedPayload string
	var mu sync.Mutex
	doneMsg := make(chan struct{}, 1)

	cl.Subscribe("calc/squared", 0, func(_ mqtt.Client, m mqtt.Message) {
		mu.Lock()
		receivedPayload = string(m.Payload())
		mu.Unlock()
		select {
		case doneMsg <- struct{}{}:
		default:
		}
	})

	time.Sleep(100 * time.Millisecond)

	// Publish input message "9" to "calc/input"
	tok := cl.Publish("calc/input", 0, false, []byte("9"))
	tok.Wait()

	select {
	case <-doneMsg:
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for script output message on calc/squared")
	}

	mu.Lock()
	payloadGot := receivedPayload
	mu.Unlock()
	if payloadGot != "81.0" && payloadGot != "81" {
		t.Fatalf("expected 81.0 or 81, got %q", payloadGot)
	}

	// 6. Query script to check executionCount updated
	singleQuery := `
query GetScript($name: String!) {
    script(name: $name) {
        name
        executionCount
        lastExecutionStatus
    }
}
`
	singleResp := gqlQuery(t, gqlURL, singleQuery, map[string]any{"name": "SquareCalculator"})
	singleData := singleResp["script"].(map[string]any)
	if singleData["lastExecutionStatus"] != "SUCCESS" {
		t.Fatalf("expected lastExecutionStatus SUCCESS, got %v", singleData["lastExecutionStatus"])
	}
	execCount := int(singleData["executionCount"].(float64))
	if execCount < 1 {
		t.Fatalf("expected executionCount >= 1, got %d", execCount)
	}

	// 7. Stop script via toggle mutation
	toggleMutation := `
mutation ToggleScript($name: String!, $enabled: Boolean!) {
    script {
        toggle(name: $name, enabled: $enabled) {
            success
            script {
                name
                enabled
            }
        }
    }
}
`
	toggleResp := gqlQuery(t, gqlURL, toggleMutation, map[string]any{"name": "SquareCalculator", "enabled": false})
	toggleData := toggleResp["script"].(map[string]any)["toggle"].(map[string]any)
	if !toggleData["success"].(bool) || toggleData["script"].(map[string]any)["enabled"].(bool) {
		t.Fatalf("toggle to false failed: %v", toggleResp)
	}

	// 8. Delete script
	deleteMutation := `
mutation DeleteScript($name: String!) {
    script {
        delete(name: $name)
    }
}
`
	delResp := gqlQuery(t, gqlURL, deleteMutation, map[string]any{"name": "SquareCalculator"})
	if !delResp["script"].(map[string]any)["delete"].(bool) {
		t.Fatalf("delete script failed: %v", delResp)
	}

	// Verify empty list
	listResp2 := gqlQuery(t, gqlURL, listQuery, nil)
	scriptsList2 := listResp2["scripts"].([]any)
	if len(scriptsList2) != 0 {
		t.Fatalf("expected 0 scripts after delete, got %d", len(scriptsList2))
	}
}
