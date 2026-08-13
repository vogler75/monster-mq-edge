package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

func runGetValue(ctx context.Context, client *Client, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: mmqctl get-value <topic> [--archive-group Default]")
	}
	topic := args[0]
	archiveGroup := "Default"

	for i := 1; i < len(args); i++ {
		if args[i] == "--archive-group" && i+1 < len(args) {
			archiveGroup = args[i+1]
			i++
		}
	}

	query := `
		query GetValue($topic: String!, $archiveGroup: String!) {
			currentValue(topic: $topic, archiveGroup: $archiveGroup) {
				topic
				payload
				format
				timestamp
				qos
			}
			retainedMessage(topic: $topic) {
				topic
				payload
				format
				timestamp
				qos
			}
		}
	`
	var res struct {
		Data struct {
			CurrentValue *struct {
				Topic     string `json:"topic"`
				Payload   string `json:"payload"`
				Format    string `json:"format"`
				Timestamp int64  `json:"timestamp"`
				QoS       int    `json:"qos"`
			} `json:"currentValue"`
			RetainedMessage *struct {
				Topic     string `json:"topic"`
				Payload   string `json:"payload"`
				Format    string `json:"format"`
				Timestamp int64  `json:"timestamp"`
				QoS       int    `json:"qos"`
			} `json:"retainedMessage"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	if err := client.DoQuery(ctx, query, map[string]any{
		"topic":        topic,
		"archiveGroup": archiveGroup,
	}, &res); err != nil {
		return err
	}
	if len(res.Errors) > 0 {
		return fmt.Errorf("GraphQL error: %s", res.Errors[0].Message)
	}

	if client.cfg.JSONMode {
		return printJSON(res.Data)
	}

	val := res.Data.CurrentValue
	if val == nil {
		val = res.Data.RetainedMessage
	}
	if val == nil {
		fmt.Printf("No value found for topic '%s'\n", topic)
		return nil
	}

	fmt.Printf("Topic:     %s\n", val.Topic)
	fmt.Printf("Payload:   %s\n", val.Payload)
	fmt.Printf("Format:    %s\n", val.Format)
	fmt.Printf("Timestamp: %d\n", val.Timestamp)
	fmt.Printf("QoS:       %d\n", val.QoS)
	return nil
}

func runSetValue(ctx context.Context, client *Client, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: mmqctl set-value <topic> <payload> [--retain] [--qos 0|1|2]")
	}
	topic := args[0]
	payload := args[1]
	retain := false
	qos := 0

	for i := 2; i < len(args); i++ {
		switch args[i] {
		case "--retain":
			retain = true
		case "--qos":
			if i+1 < len(args) {
				qos, _ = strconv.Atoi(args[i+1])
				i++
			}
		}
	}

	query := `
		mutation Publish($topic: String!, $payload: String, $qos: Int, $retain: Boolean) {
			publish(input: { topic: $topic, payload: $payload, qos: $qos, retain: $retain }) {
				success
				message
				topic
			}
		}
	`
	var res struct {
		Data struct {
			Publish struct {
				Success bool   `json:"success"`
				Message string `json:"message"`
				Topic   string `json:"topic"`
			} `json:"publish"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	if err := client.DoQuery(ctx, query, map[string]any{
		"topic":   topic,
		"payload": payload,
		"qos":     qos,
		"retain":  retain,
	}, &res); err != nil {
		return err
	}
	if len(res.Errors) > 0 {
		return fmt.Errorf("GraphQL error: %s", res.Errors[0].Message)
	}

	if client.cfg.JSONMode {
		return printJSON(res.Data.Publish)
	}

	if res.Data.Publish.Success {
		fmt.Printf("Published to topic '%s' successfully\n", topic)
	} else {
		fmt.Printf("Failed to publish: %s\n", res.Data.Publish.Message)
	}
	return nil
}

func runListTopics(ctx context.Context, client *Client, args []string) error {
	pattern := "#"
	limit := 100
	archiveGroup := "Default"

	if len(args) > 0 && !strings.HasPrefix(args[0], "--") {
		pattern = args[0]
	}

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--limit":
			if i+1 < len(args) {
				limit, _ = strconv.Atoi(args[i+1])
				i++
			}
		case "--archive-group":
			if i+1 < len(args) {
				archiveGroup = args[i+1]
				i++
			}
		}
	}

	query := `
		query ListTopics($pattern: String!, $limit: Int, $archiveGroup: String) {
			searchTopics(pattern: $pattern, limit: $limit, archiveGroup: $archiveGroup)
		}
	`
	var res struct {
		Data struct {
			SearchTopics []string `json:"searchTopics"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	if err := client.DoQuery(ctx, query, map[string]any{
		"pattern":      pattern,
		"limit":        limit,
		"archiveGroup": archiveGroup,
	}, &res); err != nil {
		return err
	}
	if len(res.Errors) > 0 {
		return fmt.Errorf("GraphQL error: %s", res.Errors[0].Message)
	}

	if client.cfg.JSONMode {
		return printJSON(res.Data.SearchTopics)
	}

	fmt.Printf("Found %d topics matching '%s':\n", len(res.Data.SearchTopics), pattern)
	for _, t := range res.Data.SearchTopics {
		fmt.Println(" -", t)
	}
	return nil
}

func runListArchives(ctx context.Context, client *Client, args []string) error {
	query := `
		query ListArchives {
			archiveGroups {
				name
				enabled
				deployed
				topicFilter
				retainedOnly
				lastValType
				archiveType
				databaseConnectionName
			}
		}
	`
	var res struct {
		Data struct {
			ArchiveGroups []struct {
				Name                   string   `json:"name"`
				Enabled                bool     `json:"enabled"`
				Deployed               bool     `json:"deployed"`
				TopicFilter            []string `json:"topicFilter"`
				RetainedOnly           bool     `json:"retainedOnly"`
				LastValType            string   `json:"lastValType"`
				ArchiveType            string   `json:"archiveType"`
				DatabaseConnectionName string   `json:"databaseConnectionName"`
			} `json:"archiveGroups"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	if err := client.DoQuery(ctx, query, nil, &res); err != nil {
		return err
	}
	if len(res.Errors) > 0 {
		return fmt.Errorf("GraphQL error: %s", res.Errors[0].Message)
	}

	if client.cfg.JSONMode {
		return printJSON(res.Data.ArchiveGroups)
	}

	fmt.Printf("%-20s %-10s %-12s %-12s %-20s\n", "NAME", "ENABLED", "LASTVAL_TYPE", "ARCHIVE_TYPE", "FILTERS")
	fmt.Println(strings.Repeat("-", 78))
	for _, g := range res.Data.ArchiveGroups {
		fmt.Printf("%-20s %-10t %-12s %-12s %-20s\n", g.Name, g.Enabled, g.LastValType, g.ArchiveType, strings.Join(g.TopicFilter, ", "))
	}
	return nil
}

func runArchiveStats(ctx context.Context, client *Client, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: mmqctl archive-stats <group> [--start ISO_TIME] [--end ISO_TIME] [--last-seconds N]")
	}
	group := args[0]
	var startTime, endTime string

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--start":
			if i+1 < len(args) {
				startTime = args[i+1]
				i++
			}
		case "--end":
			if i+1 < len(args) {
				endTime = args[i+1]
				i++
			}
		case "--last-seconds":
			if i+1 < len(args) {
				sec, _ := strconv.Atoi(args[i+1])
				now := time.Now().UTC()
				start := now.Add(-time.Duration(sec) * time.Second)
				startTime = start.Format(time.RFC3339)
				endTime = now.Format(time.RFC3339)
				i++
			}
		}
	}

	query := `
		query ArchiveStats($group: String!, $startTime: String, $endTime: String) {
			archiveStats(archiveGroup: $group, startTime: $startTime, endTime: $endTime) {
				minTimestamp
				dailyCounts {
					date
					count
				}
			}
		}
	`
	var res struct {
		Data struct {
			ArchiveStats *struct {
				MinTimestamp string `json:"minTimestamp"`
				DailyCounts  []struct {
					Date  string `json:"date"`
					Count int64  `json:"count"`
				} `json:"dailyCounts"`
			} `json:"archiveStats"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	vars := map[string]any{
		"group": group,
	}
	if startTime != "" {
		vars["startTime"] = startTime
	}
	if endTime != "" {
		vars["endTime"] = endTime
	}

	if err := client.DoQuery(ctx, query, vars, &res); err != nil {
		return err
	}
	if len(res.Errors) > 0 {
		return fmt.Errorf("GraphQL error: %s", res.Errors[0].Message)
	}

	if client.cfg.JSONMode {
		return printJSON(res.Data.ArchiveStats)
	}

	stats := res.Data.ArchiveStats
	if stats == nil {
		fmt.Printf("No stats available for group '%s'\n", group)
		return nil
	}

	fmt.Printf("Min Timestamp: %s\n", stats.MinTimestamp)
	fmt.Println("Daily Message Counts:")
	for _, dc := range stats.DailyCounts {
		fmt.Printf("  %-12s %d\n", dc.Date, dc.Count)
	}
	return nil
}

func runQueryHistory(ctx context.Context, client *Client, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: mmqctl query-history <topic> [--start ISO_TIME] [--end ISO_TIME] [--last-seconds N] [--limit N] [--archive-group Default]")
	}
	topic := args[0]
	archiveGroup := "Default"
	limit := 100
	var startTime, endTime string

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--start":
			if i+1 < len(args) {
				startTime = args[i+1]
				i++
			}
		case "--end":
			if i+1 < len(args) {
				endTime = args[i+1]
				i++
			}
		case "--last-seconds":
			if i+1 < len(args) {
				sec, _ := strconv.Atoi(args[i+1])
				now := time.Now().UTC()
				start := now.Add(-time.Duration(sec) * time.Second)
				startTime = start.Format(time.RFC3339)
				endTime = now.Format(time.RFC3339)
				i++
			}
		case "--limit":
			if i+1 < len(args) {
				limit, _ = strconv.Atoi(args[i+1])
				i++
			}
		case "--archive-group":
			if i+1 < len(args) {
				archiveGroup = args[i+1]
				i++
			}
		}
	}

	query := `
		query QueryHistory($topic: String!, $startTime: String, $endTime: String, $limit: Int, $archiveGroup: String!) {
			archivedMessages(topicFilter: $topic, startTime: $startTime, endTime: $endTime, limit: $limit, archiveGroup: $archiveGroup) {
				topic
				payload
				format
				timestamp
				qos
				clientId
			}
		}
	`
	var res struct {
		Data struct {
			ArchivedMessages []struct {
				Topic     string `json:"topic"`
				Payload   string `json:"payload"`
				Format    string `json:"format"`
				Timestamp int64  `json:"timestamp"`
				QoS       int    `json:"qos"`
				ClientID  string `json:"clientId"`
			} `json:"archivedMessages"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	vars := map[string]any{
		"topic":        topic,
		"limit":        limit,
		"archiveGroup": archiveGroup,
	}
	if startTime != "" {
		vars["startTime"] = startTime
	}
	if endTime != "" {
		vars["endTime"] = endTime
	}

	if err := client.DoQuery(ctx, query, vars, &res); err != nil {
		return err
	}
	if len(res.Errors) > 0 {
		return fmt.Errorf("GraphQL error: %s", res.Errors[0].Message)
	}

	if client.cfg.JSONMode {
		return printJSON(res.Data.ArchivedMessages)
	}

	fmt.Printf("Retrieved %d historical messages:\n", len(res.Data.ArchivedMessages))
	for _, m := range res.Data.ArchivedMessages {
		fmt.Printf(" [%d] %s: %s\n", m.Timestamp, m.Topic, m.Payload)
	}
	return nil
}

func runQueryAggregated(ctx context.Context, client *Client, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: mmqctl query-aggregated <topics...> [--interval ONE_MINUTE|FIVE_MINUTES|FIFTEEN_MINUTES|ONE_HOUR|ONE_DAY] [--functions AVG,MIN,MAX,COUNT] [--fields name1,name2] [--last-seconds N] [--archive-group Default]")
	}

	var topics []string
	interval := "ONE_MINUTE"
	functions := []string{"AVG"}
	var fields []string
	archiveGroup := "Default"
	var startTime, endTime string

	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "--") {
			switch args[i] {
			case "--interval":
				if i+1 < len(args) {
					interval = args[i+1]
					i++
				}
			case "--functions":
				if i+1 < len(args) {
					functions = strings.Split(args[i+1], ",")
					i++
				}
			case "--fields":
				if i+1 < len(args) {
					fields = strings.Split(args[i+1], ",")
					i++
				}
			case "--last-seconds":
				if i+1 < len(args) {
					sec, _ := strconv.Atoi(args[i+1])
					now := time.Now().UTC()
					start := now.Add(-time.Duration(sec) * time.Second)
					startTime = start.Format(time.RFC3339)
					endTime = now.Format(time.RFC3339)
					i++
				}
			case "--start":
				if i+1 < len(args) {
					startTime = args[i+1]
					i++
				}
			case "--end":
				if i+1 < len(args) {
					endTime = args[i+1]
					i++
				}
			case "--archive-group":
				if i+1 < len(args) {
					archiveGroup = args[i+1]
					i++
				}
			}
		} else {
			topics = append(topics, args[i])
		}
	}

	if startTime == "" || endTime == "" {
		now := time.Now().UTC()
		start := now.Add(-1 * time.Hour)
		startTime = start.Format(time.RFC3339)
		endTime = now.Format(time.RFC3339)
	}

	query := `
		query QueryAggregated($topics: [String!]!, $interval: AggregationInterval!, $startTime: String!, $endTime: String!, $functions: [AggregationFunction!], $fields: [String!], $archiveGroup: String!) {
			aggregatedMessages(topics: $topics, interval: $interval, startTime: $startTime, endTime: $endTime, functions: $functions, fields: $fields, archiveGroup: $archiveGroup) {
				columns
				rows
				interval
				startTime
				endTime
				topicCount
				rowCount
			}
		}
	`
	var res struct {
		Data struct {
			AggregatedMessages *struct {
				Columns    []string `json:"columns"`
				Rows       [][]any  `json:"rows"`
				Interval   string   `json:"interval"`
				StartTime  string   `json:"startTime"`
				EndTime    string   `json:"endTime"`
				TopicCount int      `json:"topicCount"`
				RowCount   int      `json:"rowCount"`
			} `json:"aggregatedMessages"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	vars := map[string]any{
		"topics":       topics,
		"interval":     interval,
		"startTime":    startTime,
		"endTime":      endTime,
		"functions":    functions,
		"archiveGroup": archiveGroup,
	}
	if len(fields) > 0 {
		vars["fields"] = fields
	}

	if err := client.DoQuery(ctx, query, vars, &res); err != nil {
		return err
	}
	if len(res.Errors) > 0 {
		return fmt.Errorf("GraphQL error: %s", res.Errors[0].Message)
	}

	if client.cfg.JSONMode {
		return printJSON(res.Data.AggregatedMessages)
	}

	agg := res.Data.AggregatedMessages
	if agg == nil {
		fmt.Println("No aggregated data returned.")
		return nil
	}

	fmt.Println("Columns:", strings.Join(agg.Columns, " | "))
	fmt.Println(strings.Repeat("-", 60))
	for _, row := range agg.Rows {
		var strRow []string
		for _, val := range row {
			strRow = append(strRow, fmt.Sprintf("%v", val))
		}
		fmt.Println(strings.Join(strRow, " | "))
	}
	return nil
}

func runDeviceList(ctx context.Context, client *Client, args []string) error {
	query := `
		query ListDevices {
			getDevices {
				name
				namespace
				nodeId
				type
				enabled
				createdAt
				updatedAt
			}
		}
	`
	var res struct {
		Data struct {
			GetDevices []struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
				NodeID    string `json:"nodeId"`
				Type      string `json:"type"`
				Enabled   bool   `json:"enabled"`
				CreatedAt string `json:"createdAt"`
				UpdatedAt string `json:"updatedAt"`
			} `json:"getDevices"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	if err := client.DoQuery(ctx, query, nil, &res); err != nil {
		return err
	}
	if len(res.Errors) > 0 {
		return fmt.Errorf("GraphQL error: %s", res.Errors[0].Message)
	}

	if client.cfg.JSONMode {
		return printJSON(res.Data.GetDevices)
	}

	fmt.Printf("%-20s %-15s %-15s %-15s %-10s %-20s\n", "NAME", "NAMESPACE", "NODE_ID", "TYPE", "ENABLED", "UPDATED_AT")
	fmt.Println(strings.Repeat("-", 98))
	for _, d := range res.Data.GetDevices {
		fmt.Printf("%-20s %-15s %-15s %-15s %-10t %-20s\n", d.Name, d.Namespace, d.NodeID, d.Type, d.Enabled, d.UpdatedAt)
	}
	return nil
}

func runDeviceDownload(ctx context.Context, client *Client, args []string) error {
	name := ""
	outFile := ""
	if len(args) > 0 {
		name = args[0]
	}
	if len(args) > 1 {
		outFile = args[1]
	}

	query := `
		query GetDevice($names: [String!]) {
			getDevices(names: $names) {
				name
				namespace
				nodeId
				type
				enabled
				config
				createdAt
				updatedAt
			}
		}
	`
	var res struct {
		Data struct {
			GetDevices []struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
				NodeID    string `json:"nodeId"`
				Type      string `json:"type"`
				Enabled   bool   `json:"enabled"`
				Config    any    `json:"config"`
				CreatedAt string `json:"createdAt"`
				UpdatedAt string `json:"updatedAt"`
			} `json:"getDevices"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	vars := map[string]any{}
	if name != "" {
		vars["names"] = []string{name}
	}

	if err := client.DoQuery(ctx, query, vars, &res); err != nil {
		return err
	}
	if len(res.Errors) > 0 {
		return fmt.Errorf("GraphQL error: %s", res.Errors[0].Message)
	}

	jsonBytes, err := json.MarshalIndent(res.Data.GetDevices, "", "  ")
	if err != nil {
		return err
	}

	if outFile != "" {
		if err := os.WriteFile(outFile, jsonBytes, 0644); err != nil {
			return fmt.Errorf("error writing output file: %w", err)
		}
		fmt.Printf("Device configuration saved to '%s'\n", outFile)
		return nil
	}

	fmt.Println(string(jsonBytes))
	return nil
}

func runDeviceUpload(ctx context.Context, client *Client, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: mmqctl device upload <file.json>")
	}
	inFile := args[0]

	fileBytes, err := os.ReadFile(inFile)
	if err != nil {
		return fmt.Errorf("error reading device config file: %w", err)
	}

	var configs []any
	if err := json.Unmarshal(fileBytes, &configs); err != nil {
		var singleConfig any
		if err2 := json.Unmarshal(fileBytes, &singleConfig); err2 == nil {
			configs = []any{singleConfig}
		} else {
			return fmt.Errorf("invalid JSON in device config file: %w", err)
		}
	}

	query := `
		mutation ImportDevices($configs: [DeviceInput!]!) {
			importDevices(configs: $configs) {
				success
				imported
				failed
				total
				errors
			}
		}
	`
	var res struct {
		Data struct {
			ImportDevices struct {
				Success  bool     `json:"success"`
				Imported int      `json:"imported"`
				Failed   int      `json:"failed"`
				Total    int      `json:"total"`
				Errors   []string `json:"errors"`
			} `json:"importDevices"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	if err := client.DoQuery(ctx, query, map[string]any{
		"configs": configs,
	}, &res); err != nil {
		return err
	}
	if len(res.Errors) > 0 {
		return fmt.Errorf("GraphQL error: %s", res.Errors[0].Message)
	}

	if client.cfg.JSONMode {
		return printJSON(res.Data.ImportDevices)
	}

	imp := res.Data.ImportDevices
	if imp.Success {
		fmt.Printf("Device configurations imported successfully: %d/%d configs imported\n", imp.Imported, imp.Total)
	} else {
		fmt.Printf("Device configuration import completed with errors: %d failed\n", imp.Failed)
		for _, e := range imp.Errors {
			fmt.Println(" - Error:", e)
		}
	}
	return nil
}

func runDeviceEnable(ctx context.Context, client *Client, args []string) error {
	return setDeviceEnabled(ctx, client, args, true)
}

func runDeviceDisable(ctx context.Context, client *Client, args []string) error {
	return setDeviceEnabled(ctx, client, args, false)
}

func setDeviceEnabled(ctx context.Context, client *Client, args []string, enabled bool) error {
	actionStr := "enable"
	if !enabled {
		actionStr = "disable"
	}
	if len(args) < 1 {
		return fmt.Errorf("usage: mmqctl device %s <name>", actionStr)
	}
	name := args[0]

	getGql := `
		query GetDevice($names: [String!]) {
			getDevices(names: $names) {
				name
				namespace
				nodeId
				type
				enabled
				config
			}
		}
	`
	var getRes struct {
		Data struct {
			GetDevices []struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
				NodeID    string `json:"nodeId"`
				Type      string `json:"type"`
				Enabled   bool   `json:"enabled"`
				Config    any    `json:"config"`
			} `json:"getDevices"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	if err := client.DoQuery(ctx, getGql, map[string]any{"names": []string{name}}, &getRes); err != nil {
		return err
	}
	if len(getRes.Errors) > 0 {
		return fmt.Errorf("GraphQL error: %s", getRes.Errors[0].Message)
	}
	if len(getRes.Data.GetDevices) == 0 {
		return fmt.Errorf("device '%s' not found", name)
	}

	dev := getRes.Data.GetDevices[0]
	updatedInput := map[string]any{
		"name":      dev.Name,
		"namespace": dev.Namespace,
		"nodeId":    dev.NodeID,
		"type":      dev.Type,
		"enabled":   enabled,
		"config":    dev.Config,
	}

	importGql := `
		mutation ImportDevices($configs: [DeviceInput!]!) {
			importDevices(configs: $configs) {
				success
				imported
				failed
				total
				errors
			}
		}
	`
	var importRes struct {
		Data struct {
			ImportDevices struct {
				Success  bool     `json:"success"`
				Imported int      `json:"imported"`
				Failed   int      `json:"failed"`
				Total    int      `json:"total"`
				Errors   []string `json:"errors"`
			} `json:"importDevices"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	if err := client.DoQuery(ctx, importGql, map[string]any{"configs": []any{updatedInput}}, &importRes); err != nil {
		return err
	}

	if strings.EqualFold(dev.Type, "MQTT-Client") || strings.EqualFold(dev.Type, "MQTT_CLIENT") {
		toggleGql := `
			mutation ToggleMqttClient($name: String!, $enabled: Boolean!) {
				mqttClient {
					toggle(name: $name, enabled: $enabled) {
						success
					}
				}
			}
		`
		var toggleRes struct {
			Data struct {
				MqttClient struct {
					Toggle struct {
						Success bool `json:"success"`
					} `json:"toggle"`
				} `json:"mqttClient"`
			} `json:"data"`
		}
		_ = client.DoQuery(ctx, toggleGql, map[string]any{
			"name":    name,
			"enabled": enabled,
		}, &toggleRes)
	}

	statusStr := "enabled"
	if !enabled {
		statusStr = "disabled"
	}

	if client.cfg.JSONMode {
		return printJSON(map[string]any{
			"device":  name,
			"enabled": enabled,
			"success": true,
		})
	}

	fmt.Printf("Device '%s' %s successfully\n", name, statusStr)
	return nil
}

func runListFeatures(ctx context.Context, client *Client, args []string) error {
	query := `
		query {
			broker {
				enabledFeatures
			}
		}
	`
	var res struct {
		Data struct {
			Broker struct {
				EnabledFeatures []string `json:"enabledFeatures"`
			} `json:"broker"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	if err := client.DoQuery(ctx, query, nil, &res); err != nil {
		return err
	}
	if len(res.Errors) > 0 {
		return fmt.Errorf("GraphQL error: %s", res.Errors[0].Message)
	}

	if client.cfg.JSONMode {
		return printJSON(res.Data.Broker.EnabledFeatures)
	}

	if len(res.Data.Broker.EnabledFeatures) == 0 {
		fmt.Println("No features enabled")
		return nil
	}

	fmt.Println("ENABLED FEATURES")
	fmt.Println(strings.Repeat("-", 20))
	for _, f := range res.Data.Broker.EnabledFeatures {
		fmt.Println(f)
	}
	return nil
}

func printJSON(v any) error {
	bytes, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(bytes))
	return nil
}
