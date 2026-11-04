package unsmodeler

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"monstermq.io/edge/internal/oahost"
	"monstermq.io/edge/internal/winccoabrowse"
)

// WinCC OA config constants written by Create in WinCC OA.
const (
	dpConfigPvRangeMinMax = 7  // DPCONFIG_MINMAX_PVSS_RANGECHECK
	dpConfigAlertBinary   = 12 // DPCONFIG_ALERT_BINARYSIGNAL
	dpConfigAlertAnalog   = 13 // DPCONFIG_ALERT_NONBINARYSIGNAL
	dpDetailRangeMinMax   = 4  // DPDETAIL_RANGETYPE_MINMAX
)

// ObjectResult is the outcome of creating one type or datapoint.
type ObjectResult struct {
	Kind         string `json:"kind"` // type, datapoint
	Name         string `json:"name"`
	Status       string `json:"status"` // created, skipped, failed, excluded
	Message      string `json:"message,omitempty"`
	ConfigWrites int    `json:"configWrites,omitempty"`
}

// CreateResult is the outcome of Create in WinCC OA.
type CreateResult struct {
	Objects      []ObjectResult `json:"objects"`
	ConfigWrites int            `json:"configWrites"`
	Failed       int            `json:"failed"`
}

// CreatePlan summarises what Create in WinCC OA would do (B1 summary).
type CreatePlan struct {
	Types        int
	Datapoints   int
	ConfigWrites int
	Excluded     int
}

// effectiveMeta merges the type-level metadata of an element with the
// datapoint's overrides.
type effectiveMeta struct {
	unit, description, format *string
	pvRange                   *PvRange
	alert                     *Alert
	archive                   *Archive
}

func (e effectiveMeta) writes() int {
	n := 0
	for _, p := range []*string{e.unit, e.description, e.format} {
		if p != nil {
			n++
		}
	}
	if e.pvRange != nil {
		n++
	}
	if e.alert != nil {
		n++
	}
	if e.archive != nil && e.archive.Enabled && e.archive.Group != "" {
		n++
	}
	return n
}

func effectiveArchive(s *Settings, te *TypeElement, de *DatapointElement) *Archive {
	var enabled bool
	var group string
	if s != nil && s.OaArchiveEnabled {
		enabled, group = true, s.OaArchiveGroup
	}
	if te != nil && te.Archive != nil {
		enabled = te.Archive.Enabled
		if te.Archive.Group != "" {
			group = te.Archive.Group
		}
	}
	if de != nil && de.Archive != nil {
		enabled = de.Archive.Enabled
		if de.Archive.Group != "" {
			group = de.Archive.Group
		}
	}
	if !enabled && group == "" {
		return nil
	}
	return &Archive{Enabled: enabled, Group: group}
}

func datapointMeta(t *TypeProposal, d *Datapoint, s *Settings) map[string]effectiveMeta {
	out := map[string]effectiveMeta{}
	teByPath := map[string]*TypeElement{}
	if t != nil {
		for _, e := range t.Elements {
			teByPath[e.Path] = e
			arch := effectiveArchive(s, e, nil)
			out[e.Path] = effectiveMeta{
				unit:        e.Unit,
				description: e.Description,
				format:      e.Format,
				pvRange:     e.PvRange,
				alert:       e.Alert,
				archive:     arch,
			}
		}
	}
	if d != nil {
		for _, e := range d.Elements {
			m := out[e.Path]
			if e.Unit != nil {
				m.unit = e.Unit
			}
			if e.Description != nil {
				m.description = e.Description
			}
			if e.Format != nil {
				m.format = e.Format
			}
			if e.PvRange != nil {
				m.pvRange = e.PvRange
			}
			if e.Alert != nil {
				m.alert = e.Alert
			}
			te := teByPath[e.Path]
			arch := effectiveArchive(s, te, e)
			m.archive = arch
			out[e.Path] = m
		}
	}
	return out
}

// Plan returns the create summary of a Direction B document.
func Plan(doc *Document, settings ...*Settings) CreatePlan {
	var s *Settings
	if len(settings) > 0 {
		s = settings[0]
	}
	var p CreatePlan
	types := map[string]*TypeProposal{}
	for _, t := range doc.Types {
		types[t.Name] = t
		if t.Excluded {
			p.Excluded++
		} else if !t.Reuse {
			p.Types++
		}
	}
	for _, d := range doc.Datapoints {
		t := types[d.Type]
		if d.Excluded || t == nil || t.Excluded {
			p.Excluded++
			continue
		}
		p.Datapoints++
		for _, m := range datapointMeta(t, d, s) {
			p.ConfigWrites += m.writes()
		}
	}
	return p
}

// CreateInWinCCOa validates the draft of a Direction B device and creates
// its types and datapoints with their metadata through the native API. An
// existing type is never changed; an existing datapoint of the same type
// is skipped. The draft becomes the applied document and the value flow
// starts.
func (m *Manager) CreateInWinCCOa(ctx context.Context, name string) (*CreateResult, []Issue, error) {
	_, s, err := m.deviceConfig(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	if s.Direction != DirectionMqttToWinccoa {
		return nil, nil, errors.New("Create in WinCC OA is for Direction B devices")
	}
	if !m.deviceGate(s).Active() {
		return nil, nil, ErrStandby
	}
	if m.browse == nil {
		return nil, nil, ErrNotEmbedded
	}
	raw, err := m.getDoc(ctx, name, SlotDraft)
	if err != nil {
		return nil, nil, err
	}
	if raw == nil {
		return nil, nil, errors.New("the device has no draft")
	}
	res, err := Validate(ctx, raw, m.Env(name, s))
	if err != nil {
		return nil, nil, err
	}
	if HasErrors(res.Issues) {
		return nil, res.Issues, nil
	}
	doc := res.Doc
	api := m.browse.API()
	local, err := m.browse.LocalSystem(ctx)
	if err != nil {
		return nil, res.Issues, err
	}
	out := &CreateResult{}
	logObj := func(r ObjectResult) {
		out.Objects = append(out.Objects, r)
		out.ConfigWrites += r.ConfigWrites
		if r.Status == "failed" {
			out.Failed++
			m.log.Warn("uns create "+r.Kind+" failed", "device", name, r.Kind, r.Name, "err", r.Message)
		} else {
			m.log.Info("uns create "+r.Kind+" "+r.Status, "device", name, r.Kind, r.Name, "configWrites", r.ConfigWrites, "message", r.Message)
		}
	}
	types := map[string]*TypeProposal{}
	failedTypes := map[string]bool{}
	for _, t := range doc.Types {
		types[t.Name] = t
		switch {
		case t.Excluded:
			logObj(ObjectResult{Kind: "type", Name: t.Name, Status: "excluded"})
			continue
		case t.Reuse:
			logObj(ObjectResult{Kind: "type", Name: t.Name, Status: "skipped", Message: "existing type reused"})
			continue
		}
		if _, err := m.browse.TypeStructure(ctx, "", t.Name); err == nil {
			logObj(ObjectResult{Kind: "type", Name: t.Name, Status: "skipped", Message: "exists with the same layout"})
			continue
		}
		paths := make([]string, len(t.Elements))
		kinds := make([]uint32, len(t.Elements))
		for i, e := range t.Elements {
			paths[i], kinds[i] = e.Path, uint32(elementKind(e.ElementType))
		}
		tctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := api.EnsureType(tctx, t.Name, paths, kinds)
		cancel()
		if err != nil {
			failedTypes[t.Name] = true
			logObj(ObjectResult{Kind: "type", Name: t.Name, Status: "failed", Message: err.Error()})
			continue
		}
		logObj(ObjectResult{Kind: "type", Name: t.Name, Status: "created"})
	}
	var names []string
	for _, d := range doc.Datapoints {
		names = append(names, d.Name)
	}
	existing, _ := m.browse.DatapointTypes(ctx, "", names)
	archiveGroups, _ := m.browse.ArchiveGroups(ctx, "")
	for i, d := range doc.Datapoints {
		t := types[d.Type]
		if d.Excluded || t == nil || t.Excluded {
			logObj(ObjectResult{Kind: "datapoint", Name: d.Name, Status: "excluded"})
			continue
		}
		if failedTypes[d.Type] {
			logObj(ObjectResult{Kind: "datapoint", Name: d.Name, Status: "failed", Message: "its type was not created"})
			continue
		}
		if i < len(existing) && existing[i] != "" {
			logObj(ObjectResult{Kind: "datapoint", Name: d.Name, Status: "skipped", Message: "exists; fed by the flow, metadata unchanged"})
			continue
		}
		if err := api.DpCreate(ctx, d.Name, d.Type, 10*time.Second); err != nil {
			logObj(ObjectResult{Kind: "datapoint", Name: d.Name, Status: "failed", Message: err.Error()})
			continue
		}
		if err := waitExists(ctx, m.browse, d.Name); err != nil {
			logObj(ObjectResult{Kind: "datapoint", Name: d.Name, Status: "failed", Message: err.Error()})
			continue
		}
		writes, errs := m.writeMetadata(ctx, api, local, s, archiveGroups, t, d)
		r := ObjectResult{Kind: "datapoint", Name: d.Name, Status: "created", ConfigWrites: writes}
		if len(errs) > 0 {
			r.Status, r.Message = "failed", "created, but metadata failed: "+strings.Join(errs, "; ")
		}
		logObj(r)
	}
	if err := m.saveDoc(ctx, name, SlotApplied, raw); err != nil {
		return out, res.Issues, err
	}
	m.restartFlow(ctx, name)
	return out, res.Issues, nil
}

// waitExists waits until a created datapoint is known to the manager.
func waitExists(ctx context.Context, b *winccoabrowse.Service, dp string) error {
	deadline := time.Now().Add(10 * time.Second)
	for {
		t, err := b.DatapointTypes(ctx, "", []string{dp})
		if err == nil && t[0] != "" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("datapoint %s did not appear after create", dp)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (m *Manager) writeMetadata(ctx context.Context, api oahost.API, local string, s *Settings, archiveGroups []winccoabrowse.ArchiveGroup, t *TypeProposal, d *Datapoint) (int, []string) {
	var errs []string
	writes := 0
	meta := datapointMeta(t, d, s)
	teByPath := map[string]*TypeElement{}
	if t != nil {
		for _, e := range t.Elements {
			teByPath[e.Path] = e
		}
	}
	deByPath := map[string]*DatapointElement{}
	if d != nil {
		for _, e := range d.Elements {
			deByPath[e.Path] = e
		}
	}
	kindOf := map[string]oahost.Kind{}
	if t != nil {
		for _, e := range t.Elements {
			kindOf[e.Path] = elementKind(e.ElementType)
		}
		if t.Reuse {
			if ts, err := m.browse.TypeStructure(ctx, "", t.Name); err == nil {
				for _, e := range ts.Elements {
					if e.ValueElement {
						kindOf[e.Path] = oahost.Kind(e.Kind)
						if _, ok := meta[e.Path]; !ok {
							if arch := effectiveArchive(s, teByPath[e.Path], deByPath[e.Path]); arch != nil {
								meta[e.Path] = effectiveMeta{archive: arch}
							}
						}
					}
				}
			}
		}
	}
	paths := make([]string, 0, len(meta))
	for p := range meta {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		em := meta[p]
		dpe := local + ":" + d.Name + "."
		if p != "" {
			dpe += p
		}
		for _, f := range []struct {
			v     *string
			field oahost.MetaField
		}{{em.unit, oahost.MetaUnit}, {em.format, oahost.MetaFormat}, {em.description, oahost.MetaDescription}} {
			if f.v == nil {
				continue
			}
			if err := api.SetElementMeta(ctx, dpe, f.field, *f.v, 10*time.Second); err != nil {
				errs = append(errs, fmt.Sprintf("%s %v", p, err))
				continue
			}
			writes++
		}
		kind := kindOf[p]
		if em.pvRange != nil {
			names := []string{dpe + ":_pv_range.._type", dpe + ":_pv_range.._min", dpe + ":_pv_range.._max"}
			vals := []oahost.Value{{Kind: oahost.KindInt, Int: dpConfigPvRangeMinMax}, numValue(kind, em.pvRange.Min), numValue(kind, em.pvRange.Max)}
			if err := api.DpSet(ctx, names, vals, 10*time.Second); err != nil {
				errs = append(errs, fmt.Sprintf("%s _pv_range: %v", p, err))
			} else {
				writes++
			}
		}
		if em.alert != nil {
			names, vals := alertWrites(dpe, local, kind, em.alert)
			if err := api.DpSet(ctx, names, vals, 10*time.Second); err != nil {
				errs = append(errs, fmt.Sprintf("%s _alert_hdl: %v", p, err))
			} else {
				writes++
			}
		}
		if em.archive != nil && em.archive.Enabled && em.archive.Group != "" {
			const (
				archiveInfoType = 45 // DPCONFIG_DB_ARCHIVEINFO
				procValArch     = 15 // DPATTR_ARCH_PROC_VALARCH
			)
			groupDP := winccoabrowse.ResolveArchiveGroup(archiveGroups, em.archive.Group)
			if groupDP == "" {
				groupDP = em.archive.Group
				groupDP = strings.TrimSuffix(groupDP, ".")
				if idx := strings.Index(groupDP, ":"); idx >= 0 {
					groupDP = groupDP[idx+1:]
				}
			}
			if err := api.DpSet(ctx,
				[]string{dpe + ":_archive.._type", dpe + ":_archive.1._type"},
				[]oahost.Value{
					{Kind: oahost.KindInt, Int: archiveInfoType},
					{Kind: oahost.KindInt, Int: procValArch},
				}, 10*time.Second); err != nil {
				errs = append(errs, fmt.Sprintf("%s _archive: %v", p, err))
			} else if err := api.DpSet(ctx,
				[]string{
					dpe + ":_archive.1._proc",
					dpe + ":_archive.1._class",
					dpe + ":_archive.._archive",
				},
				[]oahost.Value{
					{Kind: oahost.KindInt, Int: procValArch},
					{Kind: oahost.KindString, Str: local + ":" + groupDP + "."},
					{Kind: oahost.KindBool, Bool: true},
				}, 10*time.Second); err != nil {
				errs = append(errs, fmt.Sprintf("%s _archive: %v", p, err))
			} else {
				writes++
			}
		}
	}
	return writes, errs
}

func numValue(kind oahost.Kind, f float64) oahost.Value {
	switch kind {
	case oahost.KindInt:
		return oahost.Value{Kind: oahost.KindInt, Int: int64(f)}
	case oahost.KindUint:
		return oahost.Value{Kind: oahost.KindUint, Uint: uint64(f)}
	}
	return oahost.Value{Kind: oahost.KindFloat, Float: f}
}

func str(s string) oahost.Value { return oahost.Value{Kind: oahost.KindString, Str: s} }
func boolV(b bool) oahost.Value { return oahost.Value{Kind: oahost.KindBool, Bool: b} }
func intV(i int64) oahost.Value { return oahost.Value{Kind: oahost.KindInt, Int: i} }

// alertWrites builds the one dpSet that configures _alert_hdl (§5): analog
// ranges sorted by limit, the good range in between; binary with the alarm
// state's class and text.
func alertWrites(dpe, local string, kind oahost.Kind, a *Alert) ([]string, []oahost.Value) {
	var names []string
	var vals []oahost.Value
	// set writes _alert_hdl.<detail>.<attr>; detail "" is the config itself.
	set := func(detail, attr string, v oahost.Value) {
		names = append(names, dpe+":_alert_hdl."+detail+"."+attr)
		vals = append(vals, v)
	}
	class := func(c string) oahost.Value { return str(local + ":" + c + ".") }
	text := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	if a.Kind == "binary" {
		alarm := true
		if a.AlarmValue != nil {
			alarm = *a.AlarmValue
		}
		set("", "_type", intV(dpConfigAlertBinary))
		set("", "_ok_range", boolV(!alarm))
		set("", "_class", class(a.Class))
		if alarm {
			set("", "_text1", str(text(a.Text)))
			set("", "_text0", str(""))
		} else {
			set("", "_text0", str(text(a.Text)))
			set("", "_text1", str(""))
		}
		set("", "_active", boolV(true))
		return names, vals
	}
	limits := slices.Clone(a.Limits)
	sort.Slice(limits, func(i, j int) bool { return limits[i].Limit < limits[j].Limit })
	n := len(limits) + 1
	set("", "_type", intV(dpConfigAlertAnalog))
	for r := 1; r <= n; r++ {
		set(strconv.Itoa(r), "_type", intV(dpDetailRangeMinMax))
	}
	// Range r lies between limits[r-2] and limits[r-1]. A below limit puts
	// its class on the range under it (value < limit), an above limit on
	// the range over it (value > limit); the limit value itself is good.
	for i, l := range limits {
		lower, upper := i+1, i+2
		incl := l.When == "above" // value == limit belongs to the lower range
		set(strconv.Itoa(lower), "_u_limit", numValue(kind, l.Limit))
		set(strconv.Itoa(lower), "_u_incl", boolV(incl))
		set(strconv.Itoa(upper), "_l_limit", numValue(kind, l.Limit))
		set(strconv.Itoa(upper), "_l_incl", boolV(!incl))
	}
	for r := 1; r <= n; r++ {
		var l *AlertLimit
		if r-1 < len(limits) && limits[r-1].When == "below" {
			l = limits[r-1]
		}
		if r >= 2 && limits[r-2].When == "above" {
			l = limits[r-2]
		}
		if l != nil {
			set(strconv.Itoa(r), "_class", class(l.Class))
			set(strconv.Itoa(r), "_text", str(text(l.Text)))
		}
	}
	set("", "_active", boolV(true))
	return names, vals
}
