package redfish

import "time"

// ODataLink represents a hypermedia link following OData v4 conventions.
type ODataLink struct {
	ODataID string `json:"@odata.id"`
}

// Status represents the DMTF Redfish resource status.
type Status struct {
	State        string `json:"State,omitempty"`
	Health       string `json:"Health,omitempty"`
	HealthRollup string `json:"HealthRollup,omitempty"`
}

// ServiceRoot represents the root of the Redfish REST hierarchy (/redfish/v1).
type ServiceRoot struct {
	ODataContext     string    `json:"@odata.context"`
	ODataID          string    `json:"@odata.id"`
	ODataType        string    `json:"@odata.type"`
	ID               string    `json:"Id"`
	Name             string    `json:"Name"`
	RedfishVersion   string    `json:"RedfishVersion"`
	UUID             string    `json:"UUID"`
	Chassis          ODataLink `json:"Chassis"`
	Systems          ODataLink `json:"Systems"`
	Managers         ODataLink `json:"Managers"`
	TelemetryService ODataLink `json:"TelemetryService"`
	EventService     ODataLink `json:"EventService"`
	JsonSchemas      ODataLink `json:"JsonSchemas"`
}

// Collection represents a generic Redfish resource collection.
type Collection struct {
	ODataContext string      `json:"@odata.context,omitempty"`
	ODataID      string      `json:"@odata.id"`
	ODataType    string      `json:"@odata.type"`
	Name         string      `json:"Name"`
	Description  string      `json:"Description,omitempty"`
	MembersCount int         `json:"Members@odata.count"`
	Members      []ODataLink `json:"Members"`
}

// ChassisLinks represents links from a Chassis resource to other resources.
type ChassisLinks struct {
	ComputerSystems []ODataLink `json:"ComputerSystems,omitempty"`
	ManagedBy       []ODataLink `json:"ManagedBy,omitempty"`
}

// Chassis represents a physical or logical enclosure/rack/zone (/redfish/v1/Chassis/{id}).
type Chassis struct {
	ODataContext string       `json:"@odata.context"`
	ODataID      string       `json:"@odata.id"`
	ODataType    string       `json:"@odata.type"`
	ID           string       `json:"Id"`
	Name         string       `json:"Name"`
	ChassisType  string       `json:"ChassisType"`
	Manufacturer string       `json:"Manufacturer,omitempty"`
	Model        string       `json:"Model,omitempty"`
	Status       Status       `json:"Status"`
	Sensors      ODataLink    `json:"Sensors"`
	Thermal      ODataLink    `json:"Thermal"`
	Power        ODataLink    `json:"Power"`
	Links        ChassisLinks `json:"Links,omitempty"`
}

// ThresholdReading holds the numeric threshold trigger value.
type ThresholdReading struct {
	Reading float64 `json:"Reading"`
}

// SensorThresholds represents caution and critical threshold levels.
type SensorThresholds struct {
	UpperCaution  *ThresholdReading `json:"UpperCaution,omitempty"`
	UpperCritical *ThresholdReading `json:"UpperCritical,omitempty"`
	LowerCaution  *ThresholdReading `json:"LowerCaution,omitempty"`
	LowerCritical *ThresholdReading `json:"LowerCritical,omitempty"`
}

// Sensor represents a modern DMTF standalone sensor resource (/redfish/v1/Chassis/{chassisId}/Sensors/{sensorId}).
type Sensor struct {
	ODataContext    string            `json:"@odata.context"`
	ODataID         string            `json:"@odata.id"`
	ODataType       string            `json:"@odata.type"`
	ID              string            `json:"Id"`
	Name            string            `json:"Name"`
	Reading         *float64          `json:"Reading,omitempty"`
	ReadingType     string            `json:"ReadingType,omitempty"`
	ReadingUnits    string            `json:"ReadingUnits,omitempty"`
	ReadingRangeMin *float64          `json:"ReadingRangeMin,omitempty"`
	ReadingRangeMax *float64          `json:"ReadingRangeMax,omitempty"`
	Accuracy        *float64          `json:"Accuracy,omitempty"`
	Precision       *int              `json:"Precision,omitempty"`
	Status          Status            `json:"Status"`
	Thresholds      *SensorThresholds `json:"Thresholds,omitempty"`
	Oem             map[string]any    `json:"Oem,omitempty"`
}

// TemperatureMember represents a temperature item in legacy /Thermal resource.
type TemperatureMember struct {
	ODataID                    string   `json:"@odata.id"`
	MemberID                   string   `json:"MemberId"`
	Name                       string   `json:"Name"`
	SensorNumber               int      `json:"SensorNumber"`
	ReadingCelsius             *float64 `json:"ReadingCelsius,omitempty"`
	UpperThresholdNonCritical  *float64 `json:"UpperThresholdNonCritical,omitempty"`
	UpperThresholdCritical     *float64 `json:"UpperThresholdCritical,omitempty"`
	LowerThresholdNonCritical  *float64 `json:"LowerThresholdNonCritical,omitempty"`
	LowerThresholdCritical     *float64 `json:"LowerThresholdCritical,omitempty"`
	MinReadingRangeTemp        *float64 `json:"MinReadingRangeTemp,omitempty"`
	MaxReadingRangeTemp        *float64 `json:"MaxReadingRangeTemp,omitempty"`
	Status                     Status   `json:"Status"`
}

// FanMember represents a fan item in legacy /Thermal resource.
type FanMember struct {
	ODataID                    string   `json:"@odata.id"`
	MemberID                   string   `json:"MemberId"`
	Name                       string   `json:"Name"`
	Reading                    *float64 `json:"Reading,omitempty"`
	ReadingUnits               string   `json:"ReadingUnits,omitempty"`
	Status                     Status   `json:"Status"`
}

// Thermal represents the legacy Thermal subresource (/redfish/v1/Chassis/{id}/Thermal).
type Thermal struct {
	ODataContext string              `json:"@odata.context"`
	ODataID      string              `json:"@odata.id"`
	ODataType    string              `json:"@odata.type"`
	ID           string              `json:"Id"`
	Name         string              `json:"Name"`
	Temperatures []TemperatureMember `json:"Temperatures"`
	Fans         []FanMember         `json:"Fans"`
	Status       Status              `json:"Status"`
}

// PowerControlMember represents power control info in legacy /Power resource.
type PowerControlMember struct {
	ODataID            string   `json:"@odata.id"`
	MemberID           string   `json:"MemberId"`
	Name               string   `json:"Name"`
	PowerConsumedWatts *float64 `json:"PowerConsumedWatts,omitempty"`
	Status             Status   `json:"Status"`
}

// VoltageMember represents voltage measurement info in legacy /Power resource.
type VoltageMember struct {
	ODataID      string   `json:"@odata.id"`
	MemberID     string   `json:"MemberId"`
	Name         string   `json:"Name"`
	ReadingVolts *float64 `json:"ReadingVolts,omitempty"`
	Status       Status   `json:"Status"`
}

// Power represents the legacy Power subresource (/redfish/v1/Chassis/{id}/Power).
type Power struct {
	ODataContext string               `json:"@odata.context"`
	ODataID      string               `json:"@odata.id"`
	ODataType    string               `json:"@odata.type"`
	ID           string               `json:"Id"`
	Name         string               `json:"Name"`
	PowerControl []PowerControlMember `json:"PowerControl"`
	Voltages     []VoltageMember      `json:"Voltages"`
	Status       Status               `json:"Status"`
}

// SystemLinks represents links from a ComputerSystem resource.
type SystemLinks struct {
	Chassis   []ODataLink `json:"Chassis,omitempty"`
	ManagedBy []ODataLink `json:"ManagedBy,omitempty"`
}

// ComputerSystem represents an Edge node host system (/redfish/v1/Systems/{id}).
type ComputerSystem struct {
	ODataContext string      `json:"@odata.context"`
	ODataID      string      `json:"@odata.id"`
	ODataType    string      `json:"@odata.type"`
	ID           string      `json:"Id"`
	Name         string      `json:"Name"`
	SystemType   string      `json:"SystemType"`
	Manufacturer string      `json:"Manufacturer,omitempty"`
	Model        string      `json:"Model,omitempty"`
	Status       Status      `json:"Status"`
	Links        SystemLinks `json:"Links,omitempty"`
}

// RedfishManager represents a management controller daemon (/redfish/v1/Managers/{id}).
type RedfishManager struct {
	ODataContext    string `json:"@odata.context"`
	ODataID         string `json:"@odata.id"`
	ODataType       string `json:"@odata.type"`
	ID              string `json:"Id"`
	Name            string `json:"Name"`
	ManagerType     string `json:"ManagerType"`
	FirmwareVersion string `json:"FirmwareVersion,omitempty"`
	Status          Status `json:"Status"`
}

// MetricValue represents a single metric entry in a MetricReport.
type MetricValue struct {
	MetricID        string `json:"MetricId"`
	MetricValue     string `json:"MetricValue"`
	Timestamp       string `json:"Timestamp"`
	MetricProperty  string `json:"MetricProperty,omitempty"`
}

// MetricReport represents an aggregated metric report (/redfish/v1/TelemetryService/MetricReports/{id}).
type MetricReport struct {
	ODataContext string        `json:"@odata.context"`
	ODataID      string        `json:"@odata.id"`
	ODataType    string        `json:"@odata.type"`
	ID           string        `json:"Id"`
	Name         string        `json:"Name"`
	Timestamp    string        `json:"Timestamp"`
	MetricValues []MetricValue `json:"MetricValues"`
}

// TelemetryService represents the top-level telemetry container (/redfish/v1/TelemetryService).
type TelemetryService struct {
	ODataContext  string    `json:"@odata.context"`
	ODataID       string    `json:"@odata.id"`
	ODataType     string    `json:"@odata.type"`
	ID            string    `json:"Id"`
	Name          string    `json:"Name"`
	Status        Status    `json:"Status"`
	MetricReports ODataLink `json:"MetricReports"`
}

// ThresholdsConfig represents configured threshold limits.
type ThresholdsConfig struct {
	UpperCaution  *float64 `json:"upperCaution,omitempty"`
	UpperCritical *float64 `json:"upperCritical,omitempty"`
	LowerCaution  *float64 `json:"lowerCaution,omitempty"`
	LowerCritical *float64 `json:"lowerCritical,omitempty"`
}

// NormalizedSensorRecord is the payload structure saved in LastValue MessageStore
// under the topic path: {topicPrefix}/{chassisId}/sensors/{sensorId}
type NormalizedSensorRecord struct {
	ChassisID    string            `json:"chassisId"`
	SensorID     string            `json:"sensorId"`
	Name         string            `json:"name"`
	Reading      float64           `json:"reading"`
	ReadingType  string            `json:"readingType"`
	ReadingUnits string            `json:"readingUnits"`
	RangeMin     *float64          `json:"rangeMin,omitempty"`
	RangeMax     *float64          `json:"rangeMax,omitempty"`
	State        string            `json:"state"`
	Health       string            `json:"health"`
	Thresholds   *ThresholdsConfig `json:"thresholds,omitempty"`
	SourceTopic  string            `json:"sourceTopic"`
	Timestamp    string            `json:"timestamp"` // ISO 8601 (RFC3339)
	GatewayName  string            `json:"gatewayName"`
	TopicPrefix  string            `json:"topicPrefix"`
}

// GatewayConfig is the configuration structure decoded from DeviceConfig.Config JSON.
type GatewayConfig struct {
	TopicPrefix        string            `json:"topicPrefix"`
	TopicFilters       []string          `json:"topicFilters"`
	ChassisID          string            `json:"chassisId"`
	DefaultReadingType string            `json:"defaultReadingType"`
	DefaultReadingUnits string           `json:"defaultReadingUnits"`
	Thresholds         *ThresholdsConfig `json:"thresholds,omitempty"`
	JSONSchema         map[string]any    `json:"jsonSchema"`
}

// CalculateHealth determines the health status string ("OK", "Warning", "Critical")
// based on reading value and configured thresholds.
func CalculateHealth(reading float64, thresholds *ThresholdsConfig) string {
	if thresholds == nil {
		return "OK"
	}
	if (thresholds.UpperCritical != nil && reading >= *thresholds.UpperCritical) ||
		(thresholds.LowerCritical != nil && reading <= *thresholds.LowerCritical) {
		return "Critical"
	}
	if (thresholds.UpperCaution != nil && reading >= *thresholds.UpperCaution) ||
		(thresholds.LowerCaution != nil && reading <= *thresholds.LowerCaution) {
		return "Warning"
	}
	return "OK"
}

// FormatTimeRFC3339 formats time into standard Redfish ISO8601 / RFC3339 string.
func FormatTimeRFC3339(t time.Time) string {
	if t.IsZero() {
		return time.Now().UTC().Format(time.RFC3339)
	}
	return t.UTC().Format(time.RFC3339)
}
