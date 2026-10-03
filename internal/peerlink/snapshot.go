package peerlink

import (
	"context"
	"time"

	"monstermq.io/edge/internal/mqtt/packets"
	"monstermq.io/edge/internal/peerlink/wire"
)

// snapshotState is a session's materialised retained snapshot (16.5), streamed one BATCH per
// FETCH(SNAPSHOT).
type snapshotState struct {
	pkts      []packets.Packet
	pos       int
	truncated uint64
}

// buildSnapshot materialises the current retained set, filtered exactly like capture: $ topics,
// the own namespace while native mode is active, Include and the resolved Exclude.
// A failed scan is an error: a partial set must not end with SNAPSHOT_END, or the consumer would
// take it as complete.
func (m *Manager) buildSnapshot(ctx context.Context) (*snapshotState, error) {
	st := &snapshotState{}
	limit := m.cfg.Snapshot.GetMaxTopics()
	err := m.retained.Snapshot(ctx, func(pk packets.Packet) bool {
		if len(pk.Payload) == 0 || !m.hook.accept(pk.TopicName) {
			return true
		}
		if limit > 0 && len(st.pkts) >= limit {
			st.truncated++
			return true
		}
		st.pkts = append(st.pkts, pk)
		return true
	})
	if err != nil {
		if ctx.Err() == nil {
			m.logger.Warn("peerlink: reading the retained set for a snapshot failed; the session ends", "error", err)
		}
		return nil, err
	}
	if st.truncated > 0 {
		m.logger.Warn("peerlink: retained snapshot truncated at Snapshot.MaxTopics", "maxTopics", limit, "truncated", st.truncated)
	}
	return st, nil
}

// snapshotRecord encodes a retained packet as a snapshot record. captureMonoMs is the source mono
// time "now - age", clamped at the epoch start; expirySec is chosen so that expirySec - age is the
// remaining expiry. ok is false for an expired value or one that cannot be encoded.
func snapshotRecord(pk packets.Packet, now time.Time, nowMono uint64) (wire.Record, bool) {
	rec := wire.Record{Flags: wire.FlagSnapshot}
	pk.FixedHeader.Retain = true
	pk.FixedHeader.Dup = false
	rec.SetPacket(&pk, 0)
	rec.ClientID = pk.Origin
	nowSec := now.Unix()
	created := pk.Created
	if created <= 0 || created > nowSec {
		created = nowSec
	}
	ageMs := uint64(nowSec-created) * 1000
	if ageMs > nowMono {
		ageMs = nowMono
	}
	rec.CaptureMonoMs = nowMono - ageMs
	rec.PublishWallNs = created * int64(time.Second)
	expiry := pk.Expiry
	if expiry == 0 && pk.Properties.MessageExpiryInterval > 0 {
		expiry = created + int64(pk.Properties.MessageExpiryInterval)
	}
	rec.ExpirySec = 0
	if expiry > 0 {
		remaining := expiry - nowSec
		if remaining <= 0 {
			return rec, false
		}
		rec.ExpirySec = uint32(min(remaining+int64(ageMs/1000), int64(^uint32(0))))
	}
	if !rec.ValidContent() || wire.RecordSize(&rec) == 0 {
		return rec, false
	}
	return rec, true
}

// serveSnapshot answers a FETCH(SNAPSHOT) with the next part of the snapshot. Snapshot records carry
// no offsets (baseOffset 0). The SNAPSHOT_END batch carries the number of topics cut off by
// Snapshot.MaxTopics in its lost field.
func (s *session) serveSnapshot(f *wire.Fetch) error {
	if s.snap == nil {
		if s.snapOK {
			snap, err := s.m.buildSnapshot(s.ctx)
			if err != nil {
				return err
			}
			s.snap = snap
		} else {
			s.snap = &snapshotState{}
		}
	}
	maxRecords, maxBytes, _, _ := fetchLimits(f)
	now := time.Now()
	nowMono := s.m.log.MonoMs(now)
	frames := s.frames[:0]
	total := 0
	for s.snap.pos < len(s.snap.pkts) && len(frames) < maxRecords {
		rec, ok := snapshotRecord(s.snap.pkts[s.snap.pos], now, nowMono)
		if !ok {
			s.snap.pkts[s.snap.pos] = packets.Packet{}
			s.snap.pos++
			continue
		}
		size := wire.RecordSize(&rec)
		if len(frames) > 0 && total+size > maxBytes {
			break
		}
		buf := make([]byte, size)
		wire.EncodeRecord(buf, &rec)
		frames = append(frames, buf)
		total += size
		s.snap.pkts[s.snap.pos] = packets.Packet{}
		s.snap.pos++
	}
	lso, leo := s.m.log.Bounds()
	h := wire.BatchHeader{FetchID: f.FetchID, Flags: wire.BatchFlagSnapshot, Count: uint32(len(frames)), LogStart: lso, Leo: leo}
	if len(frames) == 0 {
		h.Flags |= wire.BatchFlagEmpty
	}
	done := s.snap.pos >= len(s.snap.pkts)
	if done {
		h.Flags |= wire.BatchFlagSnapshotEnd
		h.Lost = s.snap.truncated
	}
	skipped := s.substituteTombstones(frames)
	err := s.writeBatch(&h, frames)
	clear(frames)
	s.frames = frames[:0]
	if done {
		s.snap = nil
	}
	if err != nil {
		return err
	}
	s.slot.snapshotServed.Add(uint64(h.Count))
	s.slot.servedSkipped.Add(uint64(skipped))
	return nil
}
