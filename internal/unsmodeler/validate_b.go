package unsmodeler

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"monstermq.io/edge/internal/oahost"
	"monstermq.io/edge/internal/winccoabrowse"
)

// Limits of a Direction B document (§6.5).
const (
	MaxTypes           = 200
	MaxTypeElements    = 500
	MaxDatapoints      = 1000
	MaxTopicBindingsB  = 5000
	metadataNamedWords = "unit description format pvRange alert range min max"
)

// typeInfo is what the checks know about one type of the document: its
// element types by path and whether it is reused.
type typeInfo struct {
	t        *TypeProposal
	ptr      string
	reused   bool              // used as is: reuse, or a new type identical to an existing one
	elements map[string]string // path -> element type (Direction B names or WinCC OA names for reused types)
	kinds    map[string]uint32 // real kinds of reused types
	oaTypes  map[string]string // real WinCC OA element types of reused types
	scalar   bool              // reused type whose root is a value element
	known    bool              // the real structure is known
}

// TopicMatch reports whether an MQTT topic filter matches a topic.
func TopicMatch(filter, topic string) bool {
	fp := strings.Split(filter, "/")
	tp := strings.Split(topic, "/")
	for i, p := range fp {
		if p == "#" {
			return true
		}
		if i >= len(tp) {
			return false
		}
		if p != "+" && p != tp[i] {
			return false
		}
	}
	return len(fp) == len(tp)
}

func inFilters(filters []string, topic string) bool {
	for _, f := range filters {
		if TopicMatch(f, topic) {
			return true
		}
	}
	return false
}

// validateB runs the Direction B checks of stage 2.
func validateB(ctx context.Context, doc *Document, env ValidationEnv) []Issue {
	var out []Issue
	add := func(sev, path, format string, args ...any) {
		out = append(out, Issue{Severity: sev, Path: path, Message: fmt.Sprintf(format, args...)})
	}
	s := env.Settings
	if s == nil {
		s = &Settings{Direction: DirectionMqttToWinccoa}
		s.applyDefaults()
	}
	if s.OaArchiveEnabled && s.OaArchiveGroup == "" {
		add(SeverityError, "/oaArchiveGroup", "archive group is required when device archiving is enabled")
	}
	// Document only.
	if len(doc.Types) > MaxTypes {
		add(SeverityError, "/types", "%d types, more than the limit of %d", len(doc.Types), MaxTypes)
	}
	if len(doc.Datapoints) > MaxDatapoints {
		add(SeverityError, "/datapoints", "%d datapoints, more than the limit of %d", len(doc.Datapoints), MaxDatapoints)
	}
	types := map[string]*typeInfo{}
	for i, t := range doc.Types {
		ptr := fmt.Sprintf("/types/%d", i)
		if _, dup := types[t.Name]; dup {
			add(SeverityError, ptr+"/name", "type name %s is used twice", quoteJSON(t.Name))
			continue
		}
		ti := &typeInfo{t: t, ptr: ptr, reused: t.Reuse, elements: map[string]string{}}
		types[t.Name] = ti
		if t.Excluded {
			continue
		}
		if len(t.Elements) > MaxTypeElements {
			add(SeverityError, ptr+"/elements", "%d elements, more than the limit of %d", len(t.Elements), MaxTypeElements)
		}
		for j, e := range t.Elements {
			ep := fmt.Sprintf("%s/elements/%d", ptr, j)
			if _, dup := ti.elements[e.Path]; dup {
				add(SeverityError, ep+"/path", "element %s is listed twice", quoteJSON(e.Path))
			}
			ti.elements[e.Path] = e.ElementType
			last := e.Path[strings.LastIndex(e.Path, ".")+1:]
			if strings.Contains(" "+metadataNamedWords+" ", " "+last+" ") {
				add(SeverityWarning, ep+"/path", "element %s is named like metadata; units, descriptions, formats, ranges and alerts belong into the element metadata", quoteJSON(e.Path))
			}
			checkRange(e.PvRange, ep+"/pvRange", add)
			checkAlert(e.Alert, e.ElementType, ep+"/alert", add)
			if s.OaArchiveEnabled && e.Archive != nil && e.Archive.Enabled && e.Archive.Group == "" && s.OaArchiveGroup == "" {
				add(SeverityError, ep+"/archive", "archive.group is required when archiving is enabled and no default archive group is set on the device")
			}
		}
		for path := range ti.elements {
			for other := range ti.elements {
				if strings.HasPrefix(other, path+".") {
					add(SeverityError, ptr+"/elements", "element %s is a value and the parent of %s", quoteJSON(path), quoteJSON(other))
				}
			}
		}
	}
	oa := env.OA
	var local string
	if oa != nil {
		if l, err := oa.LocalSystem(ctx); err == nil {
			local = l
		} else {
			oa = nil
		}
	}
	// WinCC OA: reused types, name conflicts of new types.
	for _, ti := range types {
		t := ti.t
		if t.Excluded {
			continue
		}
		if !t.Reuse && s.OaPrefix != "" && !strings.HasPrefix(t.Name, s.OaPrefix) {
			add(SeverityError, ti.ptr+"/name", "new type %s does not start with the prefix %s", quoteJSON(t.Name), quoteJSON(s.OaPrefix))
		}
		if oa == nil {
			continue
		}
		ts, err := oa.TypeStructure(ctx, "", t.Name)
		switch {
		case errors.Is(err, winccoabrowse.ErrNotFound):
			if t.Reuse {
				add(SeverityError, ti.ptr+"/name", "type %s does not exist in WinCC OA and cannot be reused", t.Name)
			}
			continue
		case err != nil:
			add(SeverityError, ti.ptr+"/name", "type %s: %v", t.Name, err)
			continue
		}
		ti.known = true
		ti.kinds, ti.oaTypes = map[string]uint32{}, map[string]string{}
		for _, e := range ts.Elements {
			if e.ValueElement {
				ti.kinds[e.Path] = e.Kind
				ti.oaTypes[e.Path] = e.ElementType
				if e.Path == "" {
					ti.scalar = true
				}
			}
		}
		if t.Reuse {
			for j, e := range t.Elements {
				if real, ok := ti.oaTypes[e.Path]; !ok {
					add(SeverityError, fmt.Sprintf("%s/elements/%d/path", ti.ptr, j), "element %s does not exist in the reused type %s", quoteJSON(e.Path), t.Name)
				} else if real != oaTypeOfElement(e.ElementType) {
					add(SeverityError, fmt.Sprintf("%s/elements/%d/elementType", ti.ptr, j), "element %s of the reused type %s is %s, not %s", quoteJSON(e.Path), t.Name, real, e.ElementType)
				}
			}
			continue
		}
		// A new type whose name exists: identical layout is reused, a
		// different one is a conflict.
		same := len(ti.oaTypes) == len(t.Elements)
		for _, e := range t.Elements {
			if ti.oaTypes[e.Path] != oaTypeOfElement(e.ElementType) {
				same = false
			}
		}
		if same {
			ti.reused = true
			add(SeverityWarning, ti.ptr+"/name", "type %s exists in WinCC OA with the same layout and is reused as is", t.Name)
		} else {
			add(SeverityError, ti.ptr+"/name", "type %s exists in WinCC OA with a different structure; rename it or exclude it", t.Name)
		}
	}
	// Alert classes.
	if oa != nil {
		classes := map[string][]string{}
		for _, ti := range types {
			if ti.t.Excluded {
				continue
			}
			for j, e := range ti.t.Elements {
				collectClasses(e.Alert, fmt.Sprintf("%s/elements/%d/alert", ti.ptr, j), classes)
			}
		}
		for i, d := range doc.Datapoints {
			if d.Excluded {
				continue
			}
			for j, e := range d.Elements {
				collectClasses(e.Alert, fmt.Sprintf("/datapoints/%d/elements/%d/alert", i, j), classes)
			}
		}
		names := make([]string, 0, len(classes))
		for c := range classes {
			names = append(names, c)
		}
		sort.Strings(names)
		for _, c := range names {
			found, err := oa.MatchDatapoints(ctx, "", c, "_AlertClass")
			if err == nil && !slices.Contains(found, c) {
				for _, p := range classes[c] {
					add(SeverityError, p, "alert class %s does not exist (an _AlertClass datapoint)", quoteJSON(c))
				}
			}
		}
		type groupRef struct {
			group string
			ptr   string
		}
		var refs []groupRef
		if s.OaArchiveEnabled && s.OaArchiveGroup != "" {
			refs = append(refs, groupRef{group: s.OaArchiveGroup, ptr: "/oaArchiveGroup"})
		}
		for _, ti := range types {
			if ti.t.Excluded {
				continue
			}
			for j, e := range ti.t.Elements {
				if s.OaArchiveEnabled && e.Archive != nil && e.Archive.Enabled && e.Archive.Group != "" {
					refs = append(refs, groupRef{group: e.Archive.Group, ptr: fmt.Sprintf("%s/elements/%d/archive/group", ti.ptr, j)})
				}
			}
		}
		for i, d := range doc.Datapoints {
			if d.Excluded {
				continue
			}
			for j, e := range d.Elements {
				if s.OaArchiveEnabled && e.Archive != nil && e.Archive.Enabled && e.Archive.Group != "" {
					refs = append(refs, groupRef{group: e.Archive.Group, ptr: fmt.Sprintf("/datapoints/%d/elements/%d/archive/group", i, j)})
				}
			}
		}
		if len(refs) > 0 {
			available, err := oa.ArchiveGroups(ctx, "")
			if err == nil {
				var availNames []string
				for _, g := range available {
					availNames = append(availNames, g.Name)
				}
				availStr := strings.Join(availNames, ", ")
				for _, r := range refs {
					if winccoabrowse.ResolveArchiveGroup(available, r.group) == "" {
						if availStr != "" {
							add(SeverityError, r.ptr, "archive group %s does not exist in WinCC OA (available: %s)", quoteJSON(r.group), availStr)
						} else {
							add(SeverityError, r.ptr, "archive group %s does not exist in WinCC OA (no value archive groups found)", quoteJSON(r.group))
						}
					}
				}
			}
		}
	}
	// Datapoints.
	dpNames := map[string]bool{}
	topics := 0
	var existing []string
	var existingIdx []int
	for i, d := range doc.Datapoints {
		ptr := fmt.Sprintf("/datapoints/%d", i)
		if dpNames[d.Name] {
			add(SeverityError, ptr+"/name", "datapoint name %s is used twice", quoteJSON(d.Name))
		}
		dpNames[d.Name] = true
		ti := types[d.Type]
		if ti == nil {
			add(SeverityError, ptr+"/type", "type %s is not listed in types", quoteJSON(d.Type))
			continue
		}
		if d.Excluded || ti.t.Excluded {
			continue
		}
		if s.OaPrefix != "" && !strings.HasPrefix(d.Name, s.OaPrefix) {
			add(SeverityError, ptr+"/name", "datapoint %s does not start with the prefix %s", quoteJSON(d.Name), quoteJSON(s.OaPrefix))
		}
		elemType := func(path string) (string, bool) {
			if ti.t.Reuse || (ti.reused && ti.known) {
				if !ti.known {
					return "", true // checked once the structure is known
				}
				if path == "" && !ti.scalar {
					return "", false
				}
				t, ok := ti.oaTypes[path]
				return t, ok
			}
			if path == "" {
				return "", false
			}
			t, ok := ti.elements[path]
			return t, ok
		}
		meta := map[string]bool{}
		for j, e := range d.Elements {
			ep := fmt.Sprintf("%s/elements/%d", ptr, j)
			if meta[e.Path] {
				add(SeverityError, ep+"/path", "metadata of element %s is listed twice", quoteJSON(e.Path))
			}
			meta[e.Path] = true
			et, ok := elemType(e.Path)
			if !ok {
				add(SeverityError, ep+"/path", "element %s does not exist in type %s", quoteJSON(e.Path), d.Type)
				continue
			}
			if et != "" {
				checkDatapointMeta(e, et, ep, add)
			}
			if s.OaArchiveEnabled && e.Archive != nil && e.Archive.Enabled {
				var teGroup string
				if ti.t != nil {
					for _, te := range ti.t.Elements {
						if te.Path == e.Path && te.Archive != nil {
							teGroup = te.Archive.Group
							break
						}
					}
				}
				if e.Archive.Group == "" && teGroup == "" && s.OaArchiveGroup == "" {
					add(SeverityError, ep+"/archive", "archive.group is required when archiving is enabled and no default archive group is set on the device")
				}
			}
		}
		bound := map[string]bool{}
		for j, b := range d.Bindings {
			bp := fmt.Sprintf("%s/bindings/%d", ptr, j)
			topics++
			if bound[b.Element] {
				add(SeverityError, bp+"/element", "element %s has two bindings", quoteJSON(b.Element))
			}
			bound[b.Element] = true
			et, ok := elemType(b.Element)
			if !ok {
				add(SeverityError, bp+"/element", "element %s does not exist in type %s", quoteJSON(b.Element), d.Type)
			} else if et != "" && elementKind(oaToDirectionB(et)) == oahost.KindNull {
				add(SeverityError, bp+"/element", "element %s has element type %s, which cannot be written", quoteJSON(b.Element), et)
			}
			if b.JSONPath != "" {
				if _, err := parseJSONPath(b.JSONPath); err != nil {
					add(SeverityError, bp+"/jsonPath", "%v", err)
				}
			}
			if b.TimePath != "" {
				if _, err := parseJSONPath(b.TimePath); err != nil {
					add(SeverityError, bp+"/timePath", "%v", err)
				}
				switch b.TimeFormat {
				case TimeISO8601, TimeEpochMs, TimeEpochS:
				default:
					add(SeverityError, bp+"/timeFormat", "timePath needs timeFormat iso8601, epochMs or epochS")
				}
			} else if b.TimeFormat != "" {
				add(SeverityError, bp+"/timeFormat", "timeFormat needs a timePath")
			}
			if why := reservedTopic(b.Topic, env.Roots); why != "" {
				add(SeverityError, bp+"/topic", "source topic %s %s", quoteJSON(b.Topic), why)
			}
			if doc.Source != nil && !inFilters(doc.Source.TopicFilters, b.Topic) {
				add(SeverityWarning, bp+"/topic", "source topic %s is outside the analysed topic filters", quoteJSON(b.Topic))
			}
			if env.DirectionAPublishes != nil {
				if dev := env.DirectionAPublishes(b.Topic); dev != "" {
					add(SeverityWarning, bp+"/topic", "source topic %s is published by the Direction A device %s (WinCC OA → UNS → WinCC OA loop)", quoteJSON(b.Topic), dev)
				}
			}
			if env.Sample != nil && et != "" {
				if payload, ok := env.Sample(b.Topic); ok {
					kind := elementKind(oaToDirectionB(et))
					if _, err := convertPayload(payload, b.JSONPath, kind); err != nil {
						if errors.Is(err, errNoField) {
							add(SeverityWarning, bp+"/jsonPath", "jsonPath does not resolve in the sampled payload of %s", b.Topic)
						} else {
							add(SeverityError, bp+"/element", "element type %s does not fit the sampled payload of %s: %v", et, b.Topic, err)
						}
					}
					if b.TimePath != "" {
						if t, err := sourceTime(payload, b.TimePath, b.TimeFormat); errors.Is(err, errNoField) {
							add(SeverityWarning, bp+"/timePath", "timePath does not resolve in the sampled payload of %s", b.Topic)
						} else if err != nil {
							add(SeverityError, bp+"/timeFormat", "the time at timePath in the sampled payload of %s cannot be read as %s: %v", b.Topic, b.TimeFormat, err)
						} else if err := checkSourceTime(t, time.Now()); err != nil {
							add(SeverityWarning, bp+"/timeFormat", "%v in the sampled payload of %s; check timeFormat (epochMs or epochS)", err, b.Topic)
						}
					}
				}
			}
		}
		existing = append(existing, d.Name)
		existingIdx = append(existingIdx, i)
	}
	if topics > MaxTopicBindingsB {
		add(SeverityError, "/datapoints", "%d topic bindings, more than the limit of %d", topics, MaxTopicBindingsB)
	}
	// Datapoints that exist already.
	if oa != nil && len(existing) > 0 {
		real, err := oa.DatapointTypes(ctx, "", existing)
		if err == nil {
			var addressed map[int]string
			if api, ok := oa.(interface{ API() oahost.API }); ok {
				addressed = addressedElements(ctx, api.API(), local, doc.Datapoints, existingIdx, real)
			}
			for k, rt := range real {
				i := existingIdx[k]
				d := doc.Datapoints[i]
				ptr := fmt.Sprintf("/datapoints/%d", i)
				switch {
				case rt == "":
				case rt != d.Type:
					add(SeverityError, ptr+"/name", "datapoint %s exists with type %s, not %s", d.Name, rt, d.Type)
				default:
					sev, msg := SeverityInfo, ""
					if el := addressed[i]; el != "" {
						sev, msg = SeverityWarning, fmt.Sprintf("; element %s has a driver address, the flow would compete with the driver", quoteJSON(el))
					}
					add(sev, ptr+"/name", "datapoint %s exists with type %s; it is skipped at create and fed by the flow%s", d.Name, rt, msg)
				}
			}
		}
	}
	// Device: source filters and archive group.
	if doc.Source != nil {
		for i, f := range doc.Source.TopicFilters {
			if why := reservedTopic(strings.TrimSuffix(strings.TrimSuffix(f, "#"), "/"), env.Roots); why != "" && f != "#" {
				add(SeverityError, fmt.Sprintf("/source/topicFilters/%d", i), "topic filter %s %s", quoteJSON(f), why)
			}
		}
		if doc.Source.Mode == "archive" && env.ArchiveGroupExists != nil && !env.ArchiveGroupExists(doc.Source.ArchiveGroup) {
			add(SeverityError, "/source/archiveGroup", "archive group %s does not exist", quoteJSON(doc.Source.ArchiveGroup))
		}
	}
	return out
}

// oaToDirectionB maps a WinCC OA element type name to the Direction B
// element type ("" when it cannot be written by the flow).
func oaToDirectionB(oaType string) string {
	switch oaType {
	case "bool", "int", "uint", "float", "string", "time", "bit32", "blob":
		return oaType
	}
	return ""
}

func addressedElements(ctx context.Context, api oahost.API, local string, dps []*Datapoint, existingIdx []int, real []string) map[int]string {
	var (
		addrs []string
		dpIdx []int
		elems []string
	)
	for k, rt := range real {
		i := existingIdx[k]
		d := dps[i]
		if rt == "" || rt != d.Type {
			continue
		}
		for _, b := range d.Bindings {
			dpe := local + ":" + d.Name + "." + b.Element
			addrs = append(addrs, dpe+":_address.._type")
			dpIdx = append(dpIdx, i)
			elems = append(elems, b.Element)
		}
	}
	if len(addrs) == 0 {
		return nil
	}
	vals := getEach(ctx, api, addrs, 5*time.Second)
	out := map[int]string{}
	for idx, v := range vals {
		i := dpIdx[idx]
		if _, already := out[i]; already {
			continue
		}
		if v != nil && (v.Int != 0 || v.Uint != 0) {
			out[i] = elems[idx]
		}
	}
	return out
}

func checkRange(r *PvRange, ptr string, add func(sev, path, format string, args ...any)) {
	if r != nil && r.Min > r.Max {
		add(SeverityError, ptr, "min %v is above max %v", r.Min, r.Max)
	}
}

func checkAlert(a *Alert, elementType, ptr string, add func(sev, path, format string, args ...any)) {
	if a == nil || a.Kind != "analog" {
		return
	}
	var below, above []float64
	seen := map[float64]bool{}
	for i, l := range a.Limits {
		if seen[l.Limit] {
			add(SeverityError, fmt.Sprintf("%s/limits/%d/limit", ptr, i), "limit %v is used twice", l.Limit)
		}
		seen[l.Limit] = true
		if elementType == "uint" && l.Limit < 0 {
			add(SeverityError, fmt.Sprintf("%s/limits/%d/limit", ptr, i), "a uint limit cannot be negative")
		}
		if l.When == "below" {
			below = append(below, l.Limit)
		} else {
			above = append(above, l.Limit)
		}
	}
	if len(below) > 0 && len(above) > 0 && slices.Max(below) >= slices.Min(above) {
		add(SeverityError, ptr+"/limits", "every below limit must be lower than every above limit (a good range must remain)")
	}
}

func collectClasses(a *Alert, ptr string, out map[string][]string) {
	if a == nil {
		return
	}
	if a.Class != "" {
		out[a.Class] = append(out[a.Class], ptr+"/class")
	}
	for i, l := range a.Limits {
		out[l.Class] = append(out[l.Class], fmt.Sprintf("%s/limits/%d/class", ptr, i))
	}
}

// checkDatapointMeta applies the elementType rules of TypeElement to the
// metadata of one datapoint element.
func checkDatapointMeta(e *DatapointElement, et, ptr string, add func(sev, path, format string, args ...any)) {
	numeric := et == "int" || et == "uint" || et == "float"
	if e.Unit != nil && (et == "bool" || et == "bit32" || et == "time" || et == "blob") {
		add(SeverityError, ptr+"/unit", "a unit is not allowed for element type %s", et)
	}
	if e.Format != nil {
		if et == "bool" || et == "time" || et == "blob" {
			add(SeverityError, ptr+"/format", "a format is not allowed for element type %s", et)
		} else if !formatFits(*e.Format, et) {
			add(SeverityError, ptr+"/format", "format %s does not fit element type %s", quoteJSON(*e.Format), et)
		}
	}
	if e.PvRange != nil {
		if !numeric {
			add(SeverityError, ptr+"/pvRange", "a value range is only allowed for int, uint and float")
		} else if et != "float" && (e.PvRange.Min != float64(int64(e.PvRange.Min)) || e.PvRange.Max != float64(int64(e.PvRange.Max))) {
			add(SeverityError, ptr+"/pvRange", "the range of an %s element must be integral", et)
		} else if et == "uint" && e.PvRange.Min < 0 {
			add(SeverityError, ptr+"/pvRange/min", "a uint range cannot be negative")
		}
		checkRange(e.PvRange, ptr+"/pvRange", add)
	}
	if e.Alert != nil {
		switch {
		case e.Alert.Kind == "analog" && !numeric:
			add(SeverityError, ptr+"/alert", "analog alerts need an int, uint or float element")
		case e.Alert.Kind == "binary" && et != "bool":
			add(SeverityError, ptr+"/alert", "binary alerts need a bool element")
		}
		checkAlert(e.Alert, et, ptr+"/alert", add)
	}
}

func formatFits(f, et string) bool {
	if f == "" {
		return true
	}
	c := f[len(f)-1]
	switch et {
	case "int", "uint", "bit32":
		return strings.ContainsRune("diouxX", rune(c))
	case "float":
		return strings.ContainsRune("eEfFgG", rune(c))
	case "string":
		return c == 's'
	}
	return false
}
