package generated

type AggregatedResult struct {
	Columns    []string            `json:"columns"`
	Rows       [][]any             `json:"rows"`
	Interval   AggregationInterval `json:"interval"`
	StartTime  string              `json:"startTime"`
	EndTime    string              `json:"endTime"`
	TopicCount int                 `json:"topicCount"`
	RowCount   int                 `json:"rowCount"`
}
