package redfish

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"monstermq.io/edge/internal/stores"
	"monstermq.io/edge/internal/version"
)

// Server handles DMTF Redfish HTTP REST API requests.
type Server struct {
	nodeID           string
	defaultChassisID string
	defaultSystemID  string
	defaultManagerID string
	lastVal          stores.MessageStore
	logger           *slog.Logger
	httpSrv          *http.Server
	mu               sync.RWMutex
	gateways         map[string]*GatewayConfig
	router           *chi.Mux
}

// NewServer creates a Redfish REST API handler.
func NewServer(
	nodeID string,
	defaultChassisID string,
	defaultSystemID string,
	defaultManagerID string,
	lastVal stores.MessageStore,
	logger *slog.Logger,
) *Server {
	if defaultChassisID == "" {
		defaultChassisID = "EdgeNode"
	}
	if defaultSystemID == "" {
		defaultSystemID = nodeID
		if defaultSystemID == "" {
			defaultSystemID = "edge-node"
		}
	}
	if defaultManagerID == "" {
		defaultManagerID = "monstermq-edge"
	}

	s := &Server{
		nodeID:           nodeID,
		defaultChassisID: defaultChassisID,
		defaultSystemID:  defaultSystemID,
		defaultManagerID: defaultManagerID,
		lastVal:          lastVal,
		logger:           logger,
		gateways:         make(map[string]*GatewayConfig),
	}

	s.setupRoutes()
	return s
}

// SetGateways updates the known gateways to resolve dynamic topic prefixes.
func (s *Server) SetGateways(gateways map[string]*GatewayConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gateways = make(map[string]*GatewayConfig, len(gateways))
	for k, v := range gateways {
		if v != nil {
			s.gateways[k] = v
		}
	}
}

// Handler returns the http.Handler for mounting under /redfish/v1 or standalone.
func (s *Server) Handler() http.Handler {
	return s.router
}

func (s *Server) setupRoutes() {
	r := chi.NewRouter()

	// Redfish headers middleware
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("OData-Version", "4.0")
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Auth-Token")
			w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
			if req.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, req)
		})
	})

	// Root Service
	r.Get("/", s.handleServiceRoot)
	r.Get("/odata", s.handleOData)
	r.Get("/$metadata", s.handleMetadata)

	// Chassis & Sensors
	r.Get("/Chassis", s.handleChassisCollection)
	r.Get("/Chassis/{chassisId}", s.handleChassis)
	r.Get("/Chassis/{chassisId}/Sensors", s.handleSensorCollection)
	r.Get("/Chassis/{chassisId}/Sensors/{sensorId}", s.handleSensor)
	r.Get("/Chassis/{chassisId}/Thermal", s.handleThermal)
	r.Get("/Chassis/{chassisId}/Power", s.handlePower)

	// Systems
	r.Get("/Systems", s.handleSystemsCollection)
	r.Get("/Systems/{systemId}", s.handleSystem)

	// Managers
	r.Get("/Managers", s.handleManagersCollection)
	r.Get("/Managers/{managerId}", s.handleManager)

	// Telemetry
	r.Get("/TelemetryService", s.handleTelemetryService)
	r.Get("/TelemetryService/MetricReports", s.handleMetricReportsCollection)
	r.Get("/TelemetryService/MetricReports/{reportId}", s.handleMetricReport)

	// EventService
	r.Get("/EventService", s.handleEventService)
	r.Get("/EventService/Subscriptions", s.handleEventSubscriptions)

	// JSON Schemas
	r.Get("/JsonSchemas", s.handleJsonSchemas)
	r.Get("/JsonSchemas/{schemaId}", s.handleJsonSchema)

	s.router = r
}

func (s *Server) handleServiceRoot(w http.ResponseWriter, _ *http.Request) {
	root := ServiceRoot{
		ODataContext:     "/redfish/v1/$metadata#ServiceRoot.ServiceRoot",
		ODataID:          "/redfish/v1",
		ODataType:        "#ServiceRoot.v1_15_0.ServiceRoot",
		ID:               "RootService",
		Name:             "MonsterMQ Edge Redfish Service",
		RedfishVersion:   "1.18.0",
		UUID:             "monstermq-edge-" + s.nodeID,
		Chassis:          ODataLink{"/redfish/v1/Chassis"},
		Systems:          ODataLink{"/redfish/v1/Systems"},
		Managers:         ODataLink{"/redfish/v1/Managers"},
		TelemetryService: ODataLink{"/redfish/v1/TelemetryService"},
		EventService:     ODataLink{"/redfish/v1/EventService"},
		JsonSchemas:      ODataLink{"/redfish/v1/JsonSchemas"},
	}
	writeJSON(w, http.StatusOK, root)
}

func (s *Server) handleOData(w http.ResponseWriter, _ *http.Request) {
	doc := map[string]any{
		"@odata.context": "/redfish/v1/$metadata",
		"value": []map[string]any{
			{"name": "ServiceRoot", "kind": "Singleton", "url": "/redfish/v1"},
			{"name": "Chassis", "kind": "EntitySet", "url": "/redfish/v1/Chassis"},
			{"name": "Systems", "kind": "EntitySet", "url": "/redfish/v1/Systems"},
			{"name": "Managers", "kind": "EntitySet", "url": "/redfish/v1/Managers"},
			{"name": "TelemetryService", "kind": "Singleton", "url": "/redfish/v1/TelemetryService"},
			{"name": "EventService", "kind": "Singleton", "url": "/redfish/v1/EventService"},
			{"name": "JsonSchemas", "kind": "EntitySet", "url": "/redfish/v1/JsonSchemas"},
		},
	}
	writeJSON(w, http.StatusOK, doc)
}

func (s *Server) handleMetadata(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<edmx:Edmx xmlns:edmx="http://docs.oasis-open.org/odata/ns/edmx" Version="4.0">
  <edmx:DataServices>
    <Schema xmlns="http://docs.oasis-open.org/odata/ns/edm" Namespace="ServiceRoot">
      <EntityType Name="ServiceRoot"/>
    </Schema>
  </edmx:DataServices>
</edmx:Edmx>`
	_, _ = w.Write([]byte(xml))
}

func (s *Server) handleChassisCollection(w http.ResponseWriter, r *http.Request) {
	chassisMap := make(map[string]bool)
	chassisMap[s.defaultChassisID] = true

	sensors := s.getAllSensorRecords(r.Context())
	for _, rec := range sensors {
		if rec.ChassisID != "" {
			chassisMap[rec.ChassisID] = true
		}
	}

	var members []ODataLink
	for id := range chassisMap {
		members = append(members, ODataLink{ODataID: fmt.Sprintf("/redfish/v1/Chassis/%s", id)})
	}
	sort.Slice(members, func(i, j int) bool { return members[i].ODataID < members[j].ODataID })

	col := Collection{
		ODataContext: "/redfish/v1/$metadata#ChassisCollection.ChassisCollection",
		ODataID:      "/redfish/v1/Chassis",
		ODataType:    "#ChassisCollection.ChassisCollection",
		Name:         "Chassis Collection",
		MembersCount: len(members),
		Members:      members,
	}
	writeJSON(w, http.StatusOK, col)
}

func (s *Server) handleChassis(w http.ResponseWriter, r *http.Request) {
	chassisID := chi.URLParam(r, "chassisId")
	if chassisID == "" {
		http.NotFound(w, r)
		return
	}

	chassis := Chassis{
		ODataContext: "/redfish/v1/$metadata#Chassis.Chassis",
		ODataID:      fmt.Sprintf("/redfish/v1/Chassis/%s", chassisID),
		ODataType:    "#Chassis.v1_22_0.Chassis",
		ID:           chassisID,
		Name:         fmt.Sprintf("Chassis %s", chassisID),
		ChassisType:  "Zone",
		Manufacturer: "MonsterMQ",
		Model:        "Edge Broker",
		Status:       Status{State: "Enabled", Health: "OK"},
		Sensors:      ODataLink{fmt.Sprintf("/redfish/v1/Chassis/%s/Sensors", chassisID)},
		Thermal:      ODataLink{fmt.Sprintf("/redfish/v1/Chassis/%s/Thermal", chassisID)},
		Power:        ODataLink{fmt.Sprintf("/redfish/v1/Chassis/%s/Power", chassisID)},
		Links: ChassisLinks{
			ComputerSystems: []ODataLink{{fmt.Sprintf("/redfish/v1/Systems/%s", s.defaultSystemID)}},
			ManagedBy:       []ODataLink{{fmt.Sprintf("/redfish/v1/Managers/%s", s.defaultManagerID)}},
		},
	}

	// Calculate rollup health from active sensors
	sensors := s.getSensorsForChassis(r.Context(), chassisID)
	for _, rec := range sensors {
		if rec.Health == "Critical" {
			chassis.Status.Health = "Critical"
			break
		}
		if rec.Health == "Warning" {
			chassis.Status.Health = "Warning"
		}
	}

	writeJSON(w, http.StatusOK, chassis)
}

func (s *Server) handleSensorCollection(w http.ResponseWriter, r *http.Request) {
	chassisID := chi.URLParam(r, "chassisId")
	sensors := s.getSensorsForChassis(r.Context(), chassisID)

	var members []ODataLink
	for _, rec := range sensors {
		members = append(members, ODataLink{
			ODataID: fmt.Sprintf("/redfish/v1/Chassis/%s/Sensors/%s", chassisID, rec.SensorID),
		})
	}
	sort.Slice(members, func(i, j int) bool { return members[i].ODataID < members[j].ODataID })

	col := Collection{
		ODataContext: "/redfish/v1/$metadata#SensorCollection.SensorCollection",
		ODataID:      fmt.Sprintf("/redfish/v1/Chassis/%s/Sensors", chassisID),
		ODataType:    "#SensorCollection.SensorCollection",
		Name:         fmt.Sprintf("Sensors for Chassis %s", chassisID),
		MembersCount: len(members),
		Members:      members,
	}
	writeJSON(w, http.StatusOK, col)
}

func (s *Server) handleSensor(w http.ResponseWriter, r *http.Request) {
	chassisID := chi.URLParam(r, "chassisId")
	sensorID := chi.URLParam(r, "sensorId")

	rec, ok := s.findSensorRecord(r.Context(), chassisID, sensorID)
	if !ok {
		writeError(w, http.StatusNotFound, "ResourceMissingAtURI", fmt.Sprintf("Sensor %s not found in Chassis %s", sensorID, chassisID))
		return
	}

	reading := rec.Reading
	sensor := Sensor{
		ODataContext:    "/redfish/v1/$metadata#Sensor.Sensor",
		ODataID:         fmt.Sprintf("/redfish/v1/Chassis/%s/Sensors/%s", chassisID, sensorID),
		ODataType:       "#Sensor.v1_7_0.Sensor",
		ID:              sensorID,
		Name:            rec.Name,
		Reading:         &reading,
		ReadingType:     rec.ReadingType,
		ReadingUnits:    rec.ReadingUnits,
		ReadingRangeMin: rec.RangeMin,
		ReadingRangeMax: rec.RangeMax,
		Status: Status{
			State:  rec.State,
			Health: rec.Health,
		},
		Oem: map[string]any{
			"MonsterMQ": map[string]any{
				"Topic":           rec.SourceTopic,
				"NormalizedTopic": fmt.Sprintf("%s/%s/sensors/%s", rec.TopicPrefix, chassisID, sensorID),
				"Timestamp":       rec.Timestamp,
				"Gateway":         rec.GatewayName,
			},
		},
	}

	if rec.Thresholds != nil {
		th := &SensorThresholds{}
		if rec.Thresholds.UpperCaution != nil {
			th.UpperCaution = &ThresholdReading{*rec.Thresholds.UpperCaution}
		}
		if rec.Thresholds.UpperCritical != nil {
			th.UpperCritical = &ThresholdReading{*rec.Thresholds.UpperCritical}
		}
		if rec.Thresholds.LowerCaution != nil {
			th.LowerCaution = &ThresholdReading{*rec.Thresholds.LowerCaution}
		}
		if rec.Thresholds.LowerCritical != nil {
			th.LowerCritical = &ThresholdReading{*rec.Thresholds.LowerCritical}
		}
		sensor.Thresholds = th
	}

	writeJSON(w, http.StatusOK, sensor)
}

func (s *Server) handleThermal(w http.ResponseWriter, r *http.Request) {
	chassisID := chi.URLParam(r, "chassisId")
	sensors := s.getSensorsForChassis(r.Context(), chassisID)

	thermal := Thermal{
		ODataContext: "/redfish/v1/$metadata#Thermal.Thermal",
		ODataID:      fmt.Sprintf("/redfish/v1/Chassis/%s/Thermal", chassisID),
		ODataType:    "#Thermal.v1_7_0.Thermal",
		ID:           "Thermal",
		Name:         fmt.Sprintf("Thermal info for %s", chassisID),
		Temperatures: make([]TemperatureMember, 0),
		Fans:         make([]FanMember, 0),
		Status:       Status{State: "Enabled", Health: "OK"},
	}

	idx := 0
	for _, rec := range sensors {
		if strings.EqualFold(rec.ReadingType, "Temperature") {
			reading := rec.Reading
			temp := TemperatureMember{
				ODataID:        fmt.Sprintf("/redfish/v1/Chassis/%s/Thermal#/Temperatures/%d", chassisID, idx),
				MemberID:       fmt.Sprintf("%d", idx),
				Name:           rec.Name,
				SensorNumber:   idx + 1,
				ReadingCelsius: &reading,
				Status: Status{
					State:  rec.State,
					Health: rec.Health,
				},
			}
			if rec.Thresholds != nil {
				temp.UpperThresholdNonCritical = rec.Thresholds.UpperCaution
				temp.UpperThresholdCritical = rec.Thresholds.UpperCritical
				temp.LowerThresholdNonCritical = rec.Thresholds.LowerCaution
				temp.LowerThresholdCritical = rec.Thresholds.LowerCritical
			}
			thermal.Temperatures = append(thermal.Temperatures, temp)
			if rec.Health == "Critical" {
				thermal.Status.Health = "Critical"
			} else if rec.Health == "Warning" && thermal.Status.Health != "Critical" {
				thermal.Status.Health = "Warning"
			}
			idx++
		}
	}

	writeJSON(w, http.StatusOK, thermal)
}

func (s *Server) handlePower(w http.ResponseWriter, r *http.Request) {
	chassisID := chi.URLParam(r, "chassisId")
	sensors := s.getSensorsForChassis(r.Context(), chassisID)

	power := Power{
		ODataContext: "/redfish/v1/$metadata#Power.Power",
		ODataID:      fmt.Sprintf("/redfish/v1/Chassis/%s/Power", chassisID),
		ODataType:    "#Power.v1_7_0.Power",
		ID:           "Power",
		Name:         fmt.Sprintf("Power info for %s", chassisID),
		PowerControl: make([]PowerControlMember, 0),
		Voltages:     make([]VoltageMember, 0),
		Status:       Status{State: "Enabled", Health: "OK"},
	}

	vIdx := 0
	pIdx := 0
	for _, rec := range sensors {
		if strings.EqualFold(rec.ReadingType, "Voltage") {
			reading := rec.Reading
			v := VoltageMember{
				ODataID:      fmt.Sprintf("/redfish/v1/Chassis/%s/Power#/Voltages/%d", chassisID, vIdx),
				MemberID:     fmt.Sprintf("%d", vIdx),
				Name:         rec.Name,
				ReadingVolts: &reading,
				Status:       Status{State: rec.State, Health: rec.Health},
			}
			power.Voltages = append(power.Voltages, v)
			vIdx++
		} else if strings.EqualFold(rec.ReadingType, "Power") {
			reading := rec.Reading
			p := PowerControlMember{
				ODataID:            fmt.Sprintf("/redfish/v1/Chassis/%s/Power#/PowerControl/%d", chassisID, pIdx),
				MemberID:           fmt.Sprintf("%d", pIdx),
				Name:               rec.Name,
				PowerConsumedWatts: &reading,
				Status:             Status{State: rec.State, Health: rec.Health},
			}
			power.PowerControl = append(power.PowerControl, p)
			pIdx++
		}
	}

	writeJSON(w, http.StatusOK, power)
}

func (s *Server) handleSystemsCollection(w http.ResponseWriter, _ *http.Request) {
	col := Collection{
		ODataContext: "/redfish/v1/$metadata#ComputerSystemCollection.ComputerSystemCollection",
		ODataID:      "/redfish/v1/Systems",
		ODataType:    "#ComputerSystemCollection.ComputerSystemCollection",
		Name:         "Computer Systems Collection",
		MembersCount: 1,
		Members: []ODataLink{
			{ODataID: fmt.Sprintf("/redfish/v1/Systems/%s", s.defaultSystemID)},
		},
	}
	writeJSON(w, http.StatusOK, col)
}

func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	systemID := chi.URLParam(r, "systemId")
	system := ComputerSystem{
		ODataContext: "/redfish/v1/$metadata#ComputerSystem.ComputerSystem",
		ODataID:      fmt.Sprintf("/redfish/v1/Systems/%s", systemID),
		ODataType:    "#ComputerSystem.v1_20_0.ComputerSystem",
		ID:           systemID,
		Name:         fmt.Sprintf("MonsterMQ Edge System (%s)", s.nodeID),
		SystemType:   "OS",
		Manufacturer: "MonsterMQ",
		Model:        "Edge Broker",
		Status:       Status{State: "Enabled", Health: "OK"},
		Links: SystemLinks{
			Chassis:   []ODataLink{{fmt.Sprintf("/redfish/v1/Chassis/%s", s.defaultChassisID)}},
			ManagedBy: []ODataLink{{fmt.Sprintf("/redfish/v1/Managers/%s", s.defaultManagerID)}},
		},
	}
	writeJSON(w, http.StatusOK, system)
}

func (s *Server) handleManagersCollection(w http.ResponseWriter, _ *http.Request) {
	col := Collection{
		ODataContext: "/redfish/v1/$metadata#ManagerCollection.ManagerCollection",
		ODataID:      "/redfish/v1/Managers",
		ODataType:    "#ManagerCollection.ManagerCollection",
		Name:         "Managers Collection",
		MembersCount: 1,
		Members: []ODataLink{
			{ODataID: fmt.Sprintf("/redfish/v1/Managers/%s", s.defaultManagerID)},
		},
	}
	writeJSON(w, http.StatusOK, col)
}

func (s *Server) handleManager(w http.ResponseWriter, r *http.Request) {
	managerID := chi.URLParam(r, "managerId")
	mgr := RedfishManager{
		ODataContext:    "/redfish/v1/$metadata#Manager.Manager",
		ODataID:         fmt.Sprintf("/redfish/v1/Managers/%s", managerID),
		ODataType:       "#Manager.v1_19_0.Manager",
		ID:              managerID,
		Name:            "MonsterMQ Edge Management Service",
		ManagerType:     "Service",
		FirmwareVersion: version.Version,
		Status:          Status{State: "Enabled", Health: "OK"},
	}
	writeJSON(w, http.StatusOK, mgr)
}

func (s *Server) handleTelemetryService(w http.ResponseWriter, _ *http.Request) {
	ts := TelemetryService{
		ODataContext:  "/redfish/v1/$metadata#TelemetryService.TelemetryService",
		ODataID:       "/redfish/v1/TelemetryService",
		ODataType:     "#TelemetryService.v1_3_0.TelemetryService",
		ID:            "TelemetryService",
		Name:          "Telemetry Service",
		Status:        Status{State: "Enabled", Health: "OK"},
		MetricReports: ODataLink{"/redfish/v1/TelemetryService/MetricReports"},
	}
	writeJSON(w, http.StatusOK, ts)
}

func (s *Server) handleMetricReportsCollection(w http.ResponseWriter, _ *http.Request) {
	col := Collection{
		ODataContext: "/redfish/v1/$metadata#MetricReportCollection.MetricReportCollection",
		ODataID:      "/redfish/v1/TelemetryService/MetricReports",
		ODataType:    "#MetricReportCollection.MetricReportCollection",
		Name:         "Metric Reports Collection",
		MembersCount: 1,
		Members: []ODataLink{
			{ODataID: "/redfish/v1/TelemetryService/MetricReports/SensorsSummary"},
		},
	}
	writeJSON(w, http.StatusOK, col)
}

func (s *Server) handleMetricReport(w http.ResponseWriter, r *http.Request) {
	reportID := chi.URLParam(r, "reportId")
	sensors := s.getAllSensorRecords(r.Context())

	var values []MetricValue
	for _, rec := range sensors {
		values = append(values, MetricValue{
			MetricID:       rec.SensorID,
			MetricValue:    fmt.Sprintf("%v", rec.Reading),
			Timestamp:      rec.Timestamp,
			MetricProperty: fmt.Sprintf("/redfish/v1/Chassis/%s/Sensors/%s", rec.ChassisID, rec.SensorID),
		})
	}

	report := MetricReport{
		ODataContext: "/redfish/v1/$metadata#MetricReport.MetricReport",
		ODataID:      fmt.Sprintf("/redfish/v1/TelemetryService/MetricReports/%s", reportID),
		ODataType:    "#MetricReport.v1_5_0.MetricReport",
		ID:           reportID,
		Name:         fmt.Sprintf("Metric Report %s", reportID),
		Timestamp:    FormatTimeRFC3339(time.Now()),
		MetricValues: values,
	}
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) handleEventService(w http.ResponseWriter, _ *http.Request) {
	es := map[string]any{
		"@odata.context": "/redfish/v1/$metadata#EventService.EventService",
		"@odata.id":      "/redfish/v1/EventService",
		"@odata.type":    "#EventService.v1_10_0.EventService",
		"Id":             "EventService",
		"Name":           "Event Service",
		"Status":         Status{State: "Enabled", Health: "OK"},
		"Subscriptions":  ODataLink{"/redfish/v1/EventService/Subscriptions"},
		"ServerSentEventUri": "/redfish/v1/EventService/SSE",
	}
	writeJSON(w, http.StatusOK, es)
}

func (s *Server) handleEventSubscriptions(w http.ResponseWriter, _ *http.Request) {
	col := Collection{
		ODataContext: "/redfish/v1/$metadata#EventDestinationCollection.EventDestinationCollection",
		ODataID:      "/redfish/v1/EventService/Subscriptions",
		ODataType:    "#EventDestinationCollection.EventDestinationCollection",
		Name:         "Event Subscriptions Collection",
		MembersCount: 0,
		Members:      []ODataLink{},
	}
	writeJSON(w, http.StatusOK, col)
}

func (s *Server) handleJsonSchemas(w http.ResponseWriter, _ *http.Request) {
	schemas := []string{"ServiceRoot", "Chassis", "Sensor", "Thermal", "Power", "ComputerSystem", "Manager", "MetricReport"}
	var members []ODataLink
	for _, sc := range schemas {
		members = append(members, ODataLink{fmt.Sprintf("/redfish/v1/JsonSchemas/%s", sc)})
	}
	col := Collection{
		ODataContext: "/redfish/v1/$metadata#JsonSchemaFileCollection.JsonSchemaFileCollection",
		ODataID:      "/redfish/v1/JsonSchemas",
		ODataType:    "#JsonSchemaFileCollection.JsonSchemaFileCollection",
		Name:         "Json Schema File Collection",
		MembersCount: len(members),
		Members:      members,
	}
	writeJSON(w, http.StatusOK, col)
}

func (s *Server) handleJsonSchema(w http.ResponseWriter, r *http.Request) {
	schemaID := chi.URLParam(r, "schemaId")
	schema := map[string]any{
		"@odata.context": "/redfish/v1/$metadata#JsonSchemaFile.JsonSchemaFile",
		"@odata.id":      fmt.Sprintf("/redfish/v1/JsonSchemas/%s", schemaID),
		"@odata.type":    "#JsonSchemaFile.v1_1_4.JsonSchemaFile",
		"Id":             schemaID,
		"Name":           fmt.Sprintf("%s Schema File", schemaID),
		"Schema":         fmt.Sprintf("#%s.%s", schemaID, schemaID),
	}
	writeJSON(w, http.StatusOK, schema)
}

// Helpers for querying LastValue MessageStore

func (s *Server) getAllSensorRecords(ctx context.Context) []NormalizedSensorRecord {
	if s.lastVal == nil {
		return nil
	}

	prefixes := s.getKnownTopicPrefixes()
	var records []NormalizedSensorRecord
	seen := make(map[string]bool)

	for _, prefix := range prefixes {
		pattern := prefix + "/+/sensors/#"
		_ = s.lastVal.FindMatchingMessages(ctx, pattern, func(bm stores.BrokerMessage) bool {
			var rec NormalizedSensorRecord
			if err := json.Unmarshal(bm.Payload, &rec); err == nil {
				key := fmt.Sprintf("%s/%s", rec.ChassisID, rec.SensorID)
				if !seen[key] {
					seen[key] = true
					records = append(records, rec)
				}
			}
			return true
		})
	}

	return records
}

func (s *Server) getSensorsForChassis(ctx context.Context, chassisID string) []NormalizedSensorRecord {
	if s.lastVal == nil {
		return nil
	}

	prefixes := s.getKnownTopicPrefixes()
	var records []NormalizedSensorRecord
	seen := make(map[string]bool)

	for _, prefix := range prefixes {
		pattern := fmt.Sprintf("%s/%s/sensors/#", prefix, chassisID)
		_ = s.lastVal.FindMatchingMessages(ctx, pattern, func(bm stores.BrokerMessage) bool {
			var rec NormalizedSensorRecord
			if err := json.Unmarshal(bm.Payload, &rec); err == nil {
				if !seen[rec.SensorID] {
					seen[rec.SensorID] = true
					records = append(records, rec)
				}
			}
			return true
		})
	}

	return records
}

func (s *Server) findSensorRecord(ctx context.Context, chassisID, sensorID string) (NormalizedSensorRecord, bool) {
	if s.lastVal == nil {
		return NormalizedSensorRecord{}, false
	}

	prefixes := s.getKnownTopicPrefixes()
	for _, prefix := range prefixes {
		topic := fmt.Sprintf("%s/%s/sensors/%s", prefix, chassisID, sensorID)
		bm, err := s.lastVal.Get(ctx, topic)
		if err == nil && bm != nil && len(bm.Payload) > 0 {
			var rec NormalizedSensorRecord
			if err := json.Unmarshal(bm.Payload, &rec); err == nil {
				return rec, true
			}
		}
	}

	// Fallback scan in case chassis was wildcarded
	pattern := fmt.Sprintf("+/%s/sensors/%s", chassisID, sensorID)
	var found NormalizedSensorRecord
	isFound := false
	_ = s.lastVal.FindMatchingMessages(ctx, pattern, func(bm stores.BrokerMessage) bool {
		var rec NormalizedSensorRecord
		if err := json.Unmarshal(bm.Payload, &rec); err == nil {
			found = rec
			isFound = true
			return false
		}
		return true
	})

	return found, isFound
}

func (s *Server) getKnownTopicPrefixes() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	prefixSet := map[string]bool{"redfish": true}
	for _, gw := range s.gateways {
		if gw != nil && gw.TopicPrefix != "" {
			prefixSet[gw.TopicPrefix] = true
		}
	}

	prefixes := make([]string, 0, len(prefixSet))
	for p := range prefixSet {
		prefixes = append(prefixes, p)
	}
	return prefixes
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	w.WriteHeader(status)
	errObj := map[string]any{
		"error": map[string]any{
			"code":    "Base.1.0." + code,
			"message": message,
		},
	}
	_ = json.NewEncoder(w).Encode(errObj)
}
