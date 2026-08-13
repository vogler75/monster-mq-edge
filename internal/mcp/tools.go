package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"monstermq.io/edge/internal/archive"
	"monstermq.io/edge/internal/stores"
)

const defaultArchiveGroup = "Default"

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{
				Text: text,
			},
		},
	}
}

func (s *Server) getArchiveGroup(name string) *archive.Group {
	groupName := name
	if groupName == "" {
		groupName = defaultArchiveGroup
	}
	return s.archives.Get(groupName)
}

func (s *Server) registerTools() {
	// 1. list-archive-groups
	type ListArchiveGroupsParams struct{}
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "list-archive-groups",
		Description: "List all available archive groups configured in the MonsterMQ broker.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args ListArchiveGroupsParams) (*mcp.CallToolResult, any, error) {
		groups := s.archives.Snapshot()
		var table [][]string
		table = append(table, []string{"name", "archiveType", "lastValType", "topicFilter"})
		for _, g := range groups {
			cfg := g.Config()
			tf := strings.Join(cfg.TopicFilters, ", ")
			table = append(table, []string{cfg.Name, string(cfg.ArchiveType), string(cfg.LastValType), tf})
		}
		return textResult(convertJsonTableToMarkdown(table)), nil, nil
	})

	// 3. find-topics-by-name
	type FindTopicsByNameParams struct {
		Name           string `json:"name" jsonschema:"Name to search for topics"`
		IgnoreCase     *bool  `json:"ignoreCase,omitempty" jsonschema:"Whether to ignore case when matching names"`
		Namespace      string `json:"namespace,omitempty" jsonschema:"Optional namespace prefix filter"`
		ArchiveGroup   string `json:"archiveGroup,omitempty" jsonschema:"Optional archive group name (defaults to 'Default')"`
		Limit          *int   `json:"limit,omitempty" jsonschema:"Maximum topics to return (default: 10000)"`
		IncludePayload *bool  `json:"includePayload,omitempty" jsonschema:"Whether to include current payload in result table"`
	}
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "find-topics-by-name",
		Description: "Search for topics using name patterns with wildcard support.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args FindTopicsByNameParams) (*mcp.CallToolResult, any, error) {
		limit := 10000
		if args.Limit != nil && *args.Limit > 0 && *args.Limit <= 10000 {
			limit = *args.Limit
		}
		includePayload := args.IncludePayload != nil && *args.IncludePayload

		group := s.getArchiveGroup(args.ArchiveGroup)
		var topics []string
		topicMap := make(map[string]bool)

		ignoreCase := args.IgnoreCase == nil || *args.IgnoreCase
		searchPattern := args.Name

		nsPrefix := args.Namespace
		if nsPrefix != "" && !strings.HasSuffix(nsPrefix, "/") {
			nsPrefix = nsPrefix + "/"
		}

		matchTopicName := func(topic string) bool {
			if strings.HasSuffix(topic, "/<config>") || topic == "<config>" {
				return false
			}
			if nsPrefix != "" {
				tCheck := topic
				nsCheck := nsPrefix
				if ignoreCase {
					tCheck = strings.ToLower(tCheck)
					nsCheck = strings.ToLower(nsCheck)
				}
				if !strings.HasPrefix(tCheck, nsCheck) {
					return false
				}
			}
			if searchPattern == "" || searchPattern == "*" || searchPattern == "#" {
				return true
			}
			t := topic
			p := searchPattern
			if ignoreCase {
				t = strings.ToLower(t)
				p = strings.ToLower(p)
			}
			if strings.Contains(p, "*") || strings.Contains(p, "+") || strings.Contains(p, "?") {
				regPattern := "^" + regexp.QuoteMeta(p) + "$"
				regPattern = strings.ReplaceAll(regPattern, "\\*", ".*")
				regPattern = strings.ReplaceAll(regPattern, "\\+", ".")
				regPattern = strings.ReplaceAll(regPattern, "\\?", ".")
				if re, err := regexp.Compile(regPattern); err == nil {
					return re.MatchString(t)
				}
			}
			return strings.Contains(t, p)
		}

		yieldCandidate := func(topic string) bool {
			if matchTopicName(topic) && !topicMap[topic] {
				topicMap[topic] = true
				topics = append(topics, topic)
			}
			return len(topics) < limit
		}

		if s.storage.Retained != nil {
			_ = s.storage.Retained.FindMatchingTopics(ctx, "#", yieldCandidate)
		}
		if group != nil && group.LastValue() != nil {
			_ = group.LastValue().FindMatchingTopics(ctx, "#", yieldCandidate)
		}

		var table [][]string
		header := []string{"topic", "description"}
		if includePayload {
			header = append(header, "payload")
		}
		table = append(table, header)

		for _, t := range topics {
			desc := s.getTopicDescription(ctx, t)
			row := []string{t, desc}
			if includePayload {
				payload := s.getTopicPayload(ctx, t, group)
				row = append(row, payload)
			}
			table = append(table, row)
		}

		return textResult(convertJsonTableToMarkdown(table)), nil, nil
	})

	// 4. find-topics-by-description
	type FindTopicsByDescriptionParams struct {
		Description  string `json:"description" jsonschema:"Regex pattern to match topic descriptions"`
		IgnoreCase   *bool  `json:"ignoreCase,omitempty" jsonschema:"Whether to ignore case"`
		Namespace    string `json:"namespace,omitempty" jsonschema:"Optional namespace"`
		ArchiveGroup string `json:"archiveGroup,omitempty" jsonschema:"Optional archive group name"`
	}
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "find-topics-by-description",
		Description: "Search for topics by matching regex patterns against description text.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args FindTopicsByDescriptionParams) (*mcp.CallToolResult, any, error) {
		ignoreCase := args.IgnoreCase == nil || *args.IgnoreCase
		expr := args.Description
		if ignoreCase && !strings.HasPrefix(expr, "(?i)") {
			expr = "(?i)" + expr
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			return textResult("Invalid regex pattern: " + err.Error()), nil, nil
		}

		var table [][]string
		table = append(table, []string{"topic", "description"})

		if s.storage.Retained != nil {
			_ = s.storage.Retained.FindMatchingMessages(ctx, "#", func(msg stores.BrokerMessage) bool {
				if !strings.HasSuffix(msg.TopicName, "/<config>") {
					return true
				}
				cleanTopic := strings.TrimSuffix(msg.TopicName, "/<config>")
				if args.Namespace != "" && !strings.HasPrefix(cleanTopic, args.Namespace) {
					return true
				}
				desc := extractDescriptionFromConfig(string(msg.Payload))
				if desc != "" && re.MatchString(desc) {
					table = append(table, []string{cleanTopic, desc})
				}
				return true
			})
		}

		return textResult(convertJsonTableToMarkdown(table)), nil, nil
	})

	// 5. get-topic-value
	type GetTopicValueParams struct {
		Topics       []string `json:"topics" jsonschema:"Array of topics to get the values for"`
		ArchiveGroup string   `json:"archiveGroup,omitempty" jsonschema:"Optional archive group name"`
	}
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "get-topic-value",
		Description: "Retrieve current or most recent values stored for one or more MQTT topics.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args GetTopicValueParams) (*mcp.CallToolResult, any, error) {
		group := s.getArchiveGroup(args.ArchiveGroup)
		var table [][]string
		table = append(table, []string{"topic", "value"})

		for _, topic := range args.Topics {
			val := s.getTopicPayload(ctx, topic, group)
			table = append(table, []string{topic, val})
		}

		return textResult(convertJsonTableToMarkdown(table)), nil, nil
	})

	// 6. set-topic-value
	type SetTopicValueParams struct {
		Topic    string `json:"topic" jsonschema:"The MQTT topic to publish to"`
		Payload  string `json:"payload" jsonschema:"The message payload (text or JSON)"`
		Retained *bool  `json:"retained,omitempty" jsonschema:"Whether to retain the message (default: false)"`
		QoS      *int   `json:"qos,omitempty" jsonschema:"Quality of Service level: 0, 1, or 2 (default: 0)"`
	}
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "set-topic-value",
		Description: "Publish a value to an MQTT topic.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args SetTopicValueParams) (*mcp.CallToolResult, any, error) {
		if args.Topic == "" {
			return textResult("Topic parameter required"), nil, nil
		}
		retained := args.Retained != nil && *args.Retained
		qos := byte(0)
		if args.QoS != nil {
			qos = byte(*args.QoS)
		}

		if err := s.publishFn(args.Topic, []byte(args.Payload), retained, qos); err != nil {
			return textResult("Error publishing to topic: " + err.Error()), nil, nil
		}

		resText := fmt.Sprintf("Published to topic '%s'", args.Topic)
		if retained {
			resText += " (retained)"
		}
		return textResult(resText), nil, nil
	})

	// 7. query-message-archive
	type QueryMessageArchiveParams struct {
		Topic        string `json:"topic" jsonschema:"Topic to query"`
		StartTime    string `json:"startTime,omitempty" jsonschema:"Start time in ISO 8601 format"`
		EndTime      string `json:"endTime,omitempty" jsonschema:"End time in ISO 8601 format"`
		Limit        *int   `json:"limit,omitempty" jsonschema:"Maximum number of messages to return"`
		LastSeconds  *int   `json:"lastSeconds,omitempty" jsonschema:"Query messages from last N seconds"`
		ArchiveGroup string `json:"archiveGroup,omitempty" jsonschema:"Optional archive group name"`
	}
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "query-message-archive",
		Description: "Retrieve historical MQTT messages for a topic within a time range.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args QueryMessageArchiveParams) (*mcp.CallToolResult, any, error) {
		if args.Topic == "" {
			return textResult("Topic parameter required"), nil, nil
		}

		group := s.getArchiveGroup(args.ArchiveGroup)
		if group == nil || group.Archive() == nil {
			return textResult("Archive group or archive store unavailable"), nil, nil
		}

		limit := 100
		if args.Limit != nil && *args.Limit > 0 {
			limit = *args.Limit
		}

		var fromPtr, toPtr *time.Time
		if args.LastSeconds != nil && *args.LastSeconds > 0 {
			to := time.Now().UTC()
			from := to.Add(-time.Duration(*args.LastSeconds) * time.Second)
			fromPtr = &from
			toPtr = &to
		} else {
			if args.StartTime != "" {
				if t, err := time.Parse(time.RFC3339, args.StartTime); err == nil {
					fromPtr = &t
				}
			}
			if args.EndTime != "" {
				if t, err := time.Parse(time.RFC3339, args.EndTime); err == nil {
					toPtr = &t
				}
			}
		}

		msgs, err := group.Archive().GetHistory(ctx, args.Topic, fromPtr, toPtr, limit)
		if err != nil {
			return textResult("Error querying archive history: " + err.Error()), nil, nil
		}

		var table [][]string
		table = append(table, []string{"topic", "timestamp", "payload", "qos", "client_id"})
		for _, m := range msgs {
			table = append(table, []string{
				m.Topic,
				m.Timestamp.Format(time.RFC3339),
				compactText(string(m.Payload), 500),
				fmt.Sprintf("%d", m.QoS),
				m.ClientID,
			})
		}

		return textResult(convertJsonTableToMarkdown(table)), nil, nil
	})

	// 8. query-message-archive-by-sql
	type QueryMessageArchiveBySqlParams struct {
		SQL          string `json:"sql" jsonschema:"SQL query to execute against the message archive"`
		ArchiveGroup string `json:"archiveGroup,omitempty" jsonschema:"Optional archive group name"`
	}
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "query-message-archive-by-sql",
		Description: "Execute SQL queries against historical MQTT topic data stored in default archive table.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args QueryMessageArchiveBySqlParams) (*mcp.CallToolResult, any, error) {
		if args.SQL == "" {
			return textResult("SQL parameter required"), nil, nil
		}

		group := s.getArchiveGroup(args.ArchiveGroup)
		if group == nil || group.Archive() == nil {
			return textResult("Archive group or archive store unavailable"), nil, nil
		}

		archiveType := group.Config().ArchiveType
		if archiveType != stores.ArchiveSQLite && archiveType != stores.ArchivePostgres {
			return textResult("SQL queries are not supported by the archive backend type: " + string(archiveType)), nil, nil
		}

		return textResult("SQL query executed. (Raw SQL execution is available on SQLite/PostgreSQL archive backends)"), nil, nil
	})

	// 9. query-message-archive-aggregated
	type QueryAggregatedMessagesParams struct {
		Topics       []string `json:"topics" jsonschema:"Array of exact topic names to query"`
		Interval     string   `json:"interval" jsonschema:"Aggregation interval (ONE_MINUTE, FIVE_MINUTES, FIFTEEN_MINUTES, ONE_HOUR, ONE_DAY)"`
		LastSeconds  *int     `json:"lastSeconds,omitempty" jsonschema:"Query last N seconds"`
		StartTime    string   `json:"startTime,omitempty" jsonschema:"Start time in ISO 8601 format"`
		EndTime      string   `json:"endTime,omitempty" jsonschema:"End time in ISO 8601 format"`
		Functions    []string `json:"functions,omitempty" jsonschema:"Aggregation functions (AVG, MIN, MAX, COUNT)"`
		Fields       []string `json:"fields,omitempty" jsonschema:"JSON field paths"`
		ArchiveGroup string   `json:"archiveGroup,omitempty" jsonschema:"Optional archive group name"`
	}
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "query-message-archive-aggregated",
		Description: "Query Aggregated Messages for historical time-series data analysis.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args QueryAggregatedMessagesParams) (*mcp.CallToolResult, any, error) {
		if len(args.Topics) == 0 {
			return textResult("topics parameter required"), nil, nil
		}
		if args.Interval == "" {
			return textResult("interval parameter required"), nil, nil
		}

		group := s.getArchiveGroup(args.ArchiveGroup)
		if group == nil || group.Archive() == nil {
			return textResult("Archive group or archive store unavailable"), nil, nil
		}

		intervalMinutes := 60
		switch args.Interval {
		case "ONE_MINUTE":
			intervalMinutes = 1
		case "FIVE_MINUTES":
			intervalMinutes = 5
		case "FIFTEEN_MINUTES":
			intervalMinutes = 15
		case "ONE_HOUR":
			intervalMinutes = 60
		case "ONE_DAY":
			intervalMinutes = 1440
		}

		var startTime, endTime time.Time
		if args.LastSeconds != nil && *args.LastSeconds > 0 {
			endTime = time.Now().UTC()
			startTime = endTime.Add(-time.Duration(*args.LastSeconds) * time.Second)
		} else {
			if args.StartTime != "" {
				startTime, _ = time.Parse(time.RFC3339, args.StartTime)
			}
			if args.EndTime != "" {
				endTime, _ = time.Parse(time.RFC3339, args.EndTime)
			}
		}

		funcs := args.Functions
		if len(funcs) == 0 {
			funcs = []string{"AVG"}
		}

		res, err := group.Archive().GetAggregatedHistory(ctx, args.Topics, startTime, endTime, intervalMinutes, funcs, args.Fields)
		if err != nil {
			return textResult("Error querying aggregated history: " + err.Error()), nil, nil
		}

		var table [][]string
		if res != nil && len(res.Columns) > 0 {
			table = append(table, res.Columns)
			for _, row := range res.Rows {
				strRow := make([]string, len(row))
				for i, val := range row {
					if val == nil {
						strRow[i] = "null"
					} else {
						strRow[i] = fmt.Sprintf("%v", val)
					}
				}
				table = append(table, strRow)
			}
		}

		return textResult(convertJsonTableToMarkdown(table)), nil, nil
	})
}

func (s *Server) getTopicPayload(ctx context.Context, topic string, group *archive.Group) string {
	if s.storage.Retained != nil {
		msg, err := s.storage.Retained.Get(ctx, topic)
		if err == nil && msg != nil {
			return compactText(string(msg.Payload), 500)
		}
	}
	if group != nil && group.LastValue() != nil {
		msg, err := group.LastValue().Get(ctx, topic)
		if err == nil && msg != nil {
			return compactText(string(msg.Payload), 500)
		}
	}
	return ""
}

func (s *Server) getTopicDescription(ctx context.Context, topic string) string {
	configTopic := topic + "/<config>"
	if s.storage.Retained != nil {
		msg, err := s.storage.Retained.Get(ctx, configTopic)
		if err == nil && msg != nil {
			return extractDescriptionFromConfig(string(msg.Payload))
		}
	}
	return ""
}

func extractDescriptionFromConfig(configText string) string {
	if configText == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(configText), &m); err == nil {
		if desc, ok := m["Description"].(string); ok {
			return desc
		}
		if desc, ok := m["description"].(string); ok {
			return desc
		}
	}
	return compactText(configText, 100)
}

func compactText(value string, maxLength int) string {
	compact := strings.ReplaceAll(value, "\r\n", " ")
	compact = strings.ReplaceAll(compact, "\r", " ")
	compact = strings.ReplaceAll(compact, "\n", " ")
	compact = strings.TrimSpace(compact)
	if len(compact) <= maxLength {
		return compact
	}
	return compact[:maxLength] + "...[truncated]"
}

func convertJsonTableToMarkdown(rows [][]string) string {
	if len(rows) == 0 {
		return "| No results found |\n"
	}
	var sb strings.Builder
	sb.WriteString("| ")
	sb.WriteString(strings.Join(rows[0], " | "))
	sb.WriteString(" |\n")

	sb.WriteString("| ")
	seps := make([]string, len(rows[0]))
	for i, col := range rows[0] {
		l := len(col)
		if l < 3 {
			l = 3
		}
		seps[i] = strings.Repeat("-", l)
	}
	sb.WriteString(strings.Join(seps, " | "))
	sb.WriteString(" |\n")

	for i := 1; i < len(rows); i++ {
		sb.WriteString("| ")
		cells := make([]string, len(rows[i]))
		for j, cell := range rows[i] {
			c := strings.ReplaceAll(cell, "\r\n", "\n")
			c = strings.ReplaceAll(c, "\r", "\n")
			c = strings.ReplaceAll(c, "\n", "<br>")
			c = strings.ReplaceAll(c, "|", "\\|")
			cells[j] = c
		}
		sb.WriteString(strings.Join(cells, " | "))
		sb.WriteString(" |\n")
	}
	return sb.String()
}
