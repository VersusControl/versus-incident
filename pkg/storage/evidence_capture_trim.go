package storage

import (
	"log/slog"
	"sort"

	"github.com/VersusControl/versus-incident/pkg/logging"
)

// inProcessCaptureTrimmed throttles the warning that a cluster's captures are
// larger than an in-process store keeps, keyed by cluster.
var inProcessCaptureTrimmed = logging.NewThrottle(0, nil)

// trimCaptureToPolicy keeps the newest history records of a capture that the
// store's policy can hold, before the capture is applied: at most MaxRecords
// records, and, newest first, records whose payloads fit MaxBytes, stopping at
// the first that does not fit. The store keeps only its newest records
// anyway, so this keeps what it would have kept, without building the whole
// capture into the stored document first. Issues are never trimmed. The
// dropped records are the oldest stretch of the capture and are accounted as
// pruned, as if the store had pruned them after the save, so readers see an
// older history bound and no gap. It answers how many records it dropped and
// the newest event time among them.
func trimCaptureToPolicy(batch *EvidenceBatch, policy EvidencePolicy) (int, EvidenceRecord) {
	if policy.MaxRecords <= 0 && policy.MaxBytes <= 0 {
		return 0, EvidenceRecord{}
	}
	fits := len(batch.Records) <= policy.MaxRecords || policy.MaxRecords <= 0
	if fits && policy.MaxBytes > 0 {
		total := 0
		for _, record := range batch.Records {
			total += len(record.Payload)
		}
		fits = total <= policy.MaxBytes
	}
	if fits {
		return 0, EvidenceRecord{}
	}
	// Newest first, in the reverse of the order the store prunes in.
	order := make([]int, len(batch.Records))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := batch.Records[order[i]], batch.Records[order[j]]
		at, bt := evidenceRetentionTime(a), evidenceRetentionTime(b)
		return at.After(bt) || at.Equal(bt) && a.ID > b.ID
	})
	keep := make([]bool, len(batch.Records))
	kept, bytes := 0, 0
	for _, index := range order {
		size := len(batch.Records[index].Payload)
		if policy.MaxRecords > 0 && kept >= policy.MaxRecords || policy.MaxBytes > 0 && bytes+size > policy.MaxBytes {
			break
		}
		keep[index] = true
		kept++
		bytes += size
	}
	records := make([]EvidenceRecord, 0, kept)
	var newestDropped EvidenceRecord
	for i, record := range batch.Records {
		if keep[i] {
			records = append(records, record)
			continue
		}
		if record.At.After(newestDropped.At) {
			newestDropped = record
		}
	}
	dropped := len(batch.Records) - len(records)
	batch.Records = records
	return dropped, newestDropped
}

// warnCaptureTrimmed logs, at most once per throttle window for a cluster,
// that its captures carry more history than the in-process store keeps. It
// answers whether a line was written.
func warnCaptureTrimmed(throttle *logging.Throttle, scope EvidenceScope, policy EvidencePolicy, offered, dropped int) bool {
	storage := "file"
	if policy.Mode == "ephemeral" {
		storage = "memory"
	}
	return throttle.Fail(slog.LevelWarn, "kubernetes-history-capture-trimmed\x00"+scope.ClusterID, "over in-process history limit",
		"storage: kubernetes cluster %q sent a capture of %d history records, more than %s history storage keeps (%d records, %d MiB); kept the newest %d. Use Postgres storage for clusters this large",
		scope.ClusterID, offered, storage, policy.MaxRecords, policy.MaxBytes>>20, offered-dropped)
}
