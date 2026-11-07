package kubernetes

import (
	"bytes"
	"cmp"
	"container/heap"
	"log"
	"slices"
	"time"

	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// A capture over the save limits keeps its records in this order of tiers,
// newest event first within each tier.
const (
	// Changes to workloads, configuration, images, rollouts and Nodes,
	// warning bursts and crash terminations: what a reader and break
	// correlation use to explain a change.
	captureTierChanges = iota
	// Pod and Event creations and deletions: churn that break correlation
	// ignores, kept only when every other record fits.
	captureTierChurn
	captureTiers
)

// captureRecordTier places one droppable record in its tier.
func captureRecordTier(record storage.EvidenceRecord) int {
	if record.Type == "change" && (record.Kind == "Pod" || record.Kind == "Event") &&
		(record.Rule == string(kubechanges.Created) || record.Rule == string(kubechanges.Deleted)) {
		return captureTierChurn
	}
	return captureTierChanges
}

// captureTrim describes what fitting a capture to the history save limits
// left out: how many records and issues the capture offered, how many it
// kept, how many each tier lost, and the event times of the oldest and
// newest record it dropped.
type captureTrim struct {
	offered, kept int
	dropped       [captureTiers]int
	from, to      time.Time
}

func (trim captureTrim) droppedTotal() int { return trim.offered - trim.kept }

type captureCandidate struct {
	record storage.EvidenceRecord
	size   int
	tier   int
	// order is the record's place in the capture, which the kept records
	// keep, so a capture within the limits is saved exactly as produced.
	order int
}

// betterCaptureCandidate orders candidates best first: lower tier, then
// newestCaptureRecordFirst, then capture order.
func betterCaptureCandidate(a, b *captureCandidate) int {
	if order := cmp.Compare(a.tier, b.tier); order != 0 {
		return order
	}
	if order := newestCaptureRecordFirst(a.record, b.record); order != 0 {
		return order
	}
	return cmp.Compare(a.order, b.order)
}

// captureCandidates is a heap with the worst kept candidate on top.
type captureCandidates []*captureCandidate

func (h captureCandidates) Len() int           { return len(h) }
func (h captureCandidates) Less(i, j int) bool { return betterCaptureCandidate(h[i], h[j]) > 0 }
func (h captureCandidates) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *captureCandidates) Push(x any)        { *h = append(*h, x.(*captureCandidate)) }
func (h *captureCandidates) Pop() any {
	old := *h
	last := old[len(old)-1]
	old[len(old)-1] = nil
	*h = old[:len(old)-1]
	return last
}

// captureFit keeps a capture within the store's per-save limits while the
// capture is produced, so a capture far over the limits never holds more
// than one save's worth of records. What "now" depends on is always kept:
// issues, crash status and coverage gaps, passed to keep before any record
// is offered. Offered records are ranked by tier, newest event first, ties
// broken by a fixed order of the records' own fields. Whenever the room
// overflows, the worst kept record is dropped and becomes the cut, the best
// record dropped so far; a record ranked at or below the cut is dropped as it
// is offered, even when it would fit in the space left. The kept set is
// therefore every offered record ranked above the first record that does not
// fit after them: the longest run of the best records that fits, whatever
// order the records are offered in. What is dropped is always the lowest
// tier, oldest first, and the same capture always keeps the same records.
// When the always-kept records alone exceed the limits, every offered record
// is dropped and validation refuses the batch.
type captureFit struct {
	always       []*captureCandidate
	kept         captureCandidates
	cut          *captureCandidate
	reservedSize int
	usedSize     int
	issues       int
	offered      int
	trim         captureTrim
}

func newCaptureFit(issues []storage.EvidenceIssue) *captureFit {
	fit := &captureFit{issues: len(issues)}
	for _, issue := range issues {
		fit.reservedSize += storage.EvidencePayloadSize(issue.Payload)
	}
	return fit
}

func (fit *captureFit) keep(order int, record storage.EvidenceRecord) {
	fit.always = append(fit.always, &captureCandidate{record: record, order: order})
	fit.reservedSize += storage.EvidencePayloadSize(record.Payload)
}

func (fit *captureFit) offer(order int, record storage.EvidenceRecord) {
	fit.offered++
	candidate := &captureCandidate{record: record, size: storage.EvidencePayloadSize(record.Payload), tier: captureRecordTier(record), order: order}
	if fit.cut != nil && betterCaptureCandidate(candidate, fit.cut) >= 0 {
		fit.drop(candidate)
		return
	}
	heap.Push(&fit.kept, candidate)
	fit.usedSize += candidate.size
	room := storage.MaxEvidenceBatchRecords - fit.issues - len(fit.always)
	for len(fit.kept) > 0 && (len(fit.kept) > room || fit.reservedSize+fit.usedSize > storage.MaxEvidenceBatchBytes) {
		// Every kept record ranks above the cut, so the record evicted now
		// is the best one dropped so far.
		dropped := heap.Pop(&fit.kept).(*captureCandidate)
		fit.usedSize -= dropped.size
		fit.cut = dropped
		fit.drop(dropped)
	}
}

func (fit *captureFit) drop(dropped *captureCandidate) {
	fit.trim.dropped[dropped.tier]++
	at := dropped.record.At
	if fit.trim.from.IsZero() || at.Before(fit.trim.from) {
		fit.trim.from = at
	}
	if at.After(fit.trim.to) {
		fit.trim.to = at
	}
}

// records returns every kept record in capture order and what was dropped.
func (fit *captureFit) records() ([]storage.EvidenceRecord, captureTrim) {
	all := make([]*captureCandidate, 0, len(fit.always)+len(fit.kept))
	all = append(append(all, fit.always...), fit.kept...)
	slices.SortFunc(all, func(a, b *captureCandidate) int { return cmp.Compare(a.order, b.order) })
	records := make([]storage.EvidenceRecord, len(all))
	for i, candidate := range all {
		records[i] = candidate.record
	}
	trim := fit.trim
	trim.offered = fit.issues + len(fit.always) + fit.offered
	trim.kept = fit.issues + len(all)
	return records, trim
}

// fitCaptureToSaveLimits applies captureFit to a prepared batch: change
// records and the crash terminations named are offered, everything else is
// kept. It reports whether anything was dropped.
func fitCaptureToSaveLimits(batch *storage.EvidenceBatch, terminations map[string]bool) (captureTrim, bool) {
	fit := newCaptureFit(batch.Issues)
	droppable := func(record storage.EvidenceRecord) bool {
		return record.Type == "change" || record.Type == "crash" && terminations[record.ID]
	}
	for i, record := range batch.Records {
		if !droppable(record) {
			fit.keep(i, record)
		}
	}
	for i, record := range batch.Records {
		if droppable(record) {
			fit.offer(i, record)
		}
	}
	records, trim := fit.records()
	batch.Records = records
	return trim, trim.droppedTotal() > 0
}

// newestCaptureRecordFirst orders droppable capture records by event time,
// newest first. Equal times fall back to the observed source before the
// reconstructed one, then to identity, type, observation time and content.
func newestCaptureRecordFirst(a, b storage.EvidenceRecord) int {
	if order := b.At.Compare(a.At); order != 0 {
		return order
	}
	if order := cmp.Compare(b.Priority, a.Priority); order != 0 {
		return order
	}
	if order := cmp.Compare(a.ID, b.ID); order != 0 {
		return order
	}
	if order := cmp.Compare(a.Type, b.Type); order != 0 {
		return order
	}
	if order := b.ObservedAt.Compare(a.ObservedAt); order != 0 {
		return order
	}
	return bytes.Compare(a.Payload, b.Payload)
}

// logCaptureTrim is the one signal that a capture was saved without some of
// its records: counts and the dropped event-time span, never record text.
// dropped_pod_churn counts Pod and Event creations and deletions;
// dropped_changes counts every other change and crash termination.
func logCaptureTrim(trim captureTrim) {
	log.Printf("kubernetes: history capture over the save limit, kept newest offered=%d kept=%d dropped=%d dropped_from=%s dropped_to=%s dropped_changes=%d dropped_pod_churn=%d",
		trim.offered, trim.kept, trim.droppedTotal(), trim.from.UTC().Format(time.RFC3339Nano), trim.to.UTC().Format(time.RFC3339Nano),
		trim.dropped[captureTierChanges], trim.dropped[captureTierChurn])
}
