package agent

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/config"
)

// The benchmarks in this file measure what one log line costs on the learning
// path the worker runs today: redaction, then the drain-style miner (which
// tokenizes and masks variables before matching a template). The lines are
// built in code with the shapes a Kubernetes workload writes: JSON logs,
// logfmt, multi-line stack traces, 2 to 16 KiB long, full of high-cardinality
// identifiers (UUIDs, trace IDs, Pod names, numbers, timestamps).
//
// Each benchmark op is one line, so ns/op is ns per line and allocs/op is
// allocations per line. The miner is warmed with the same pool before timing,
// so the numbers are the steady state (templates already learned), which is
// where a long-running worker spends its time.

// miningLinePool is the number of distinct lines each shape cycles through. It
// is large enough that the IDs inside the lines never repeat in a way the
// redactor or the miner could exploit.
const miningLinePool = 1024

var miningSizes = []int{2 << 10, 4 << 10, 8 << 10, 16 << 10}

// miningShapes builds lines of one shape padded to about size bytes.
var miningShapes = map[string]func(r *rand.Rand, size int) string{
	"json":   benchJSONLine,
	"logfmt": benchLogfmtLine,
	"stack":  benchStackLine,
}

func benchUUID(r *rand.Rand) string {
	const hexDigits = "0123456789abcdef"
	b := make([]byte, 36)
	for i := range b {
		switch i {
		case 8, 13, 18, 23:
			b[i] = '-'
		default:
			b[i] = hexDigits[r.Intn(16)]
		}
	}
	return string(b)
}

func benchHex(r *rand.Rand, n int) string {
	const hexDigits = "0123456789abcdef"
	b := make([]byte, n)
	for i := range b {
		b[i] = hexDigits[r.Intn(16)]
	}
	return string(b)
}

func benchPodName(r *rand.Rand) string {
	const alnum = "bcdfghjklmnpqrstvwxz2456789"
	suffix := make([]byte, 5)
	for i := range suffix {
		suffix[i] = alnum[r.Intn(len(alnum))]
	}
	workloads := []string{"checkout", "payments", "cart", "search", "orders", "inventory"}
	return fmt.Sprintf("%s-%s-%s", workloads[r.Intn(len(workloads))], benchHex(r, 9), suffix)
}

func benchTimestamp(r *rand.Rand) string {
	return fmt.Sprintf("2026-11-21T%02d:%02d:%02d.%03dZ", r.Intn(24), r.Intn(60), r.Intn(60), r.Intn(1000))
}

var benchMessages = []string{
	"request completed",
	"error calling upstream service",
	"cache miss for key",
	"retrying database transaction after deadlock",
	"failed to publish event to broker",
	"order state transition",
}

var benchLevels = []string{"info", "info", "info", "warn", "error"}

// benchJSONLine is a structured JSON log line with a nested attribute bag that
// pads it to size. Keys are stable (they form the template); values are the
// high-cardinality part.
func benchJSONLine(r *rand.Rand, size int) string {
	var b strings.Builder
	b.Grow(size + 256)
	fmt.Fprintf(&b, `{"ts":"%s","level":"%s","logger":"com.example.%s","msg":"%s","request_id":"%s","trace_id":"%s","span_id":"%s","pod":"%s","user_id":%d,"latency_ms":%d.%03d,"path":"/api/v1/orders/%d/items/%d","client":"10.%d.%d.%d"`,
		benchTimestamp(r), benchLevels[r.Intn(len(benchLevels))], []string{"api", "db", "queue"}[r.Intn(3)],
		benchMessages[r.Intn(len(benchMessages))], benchUUID(r), benchHex(r, 32), benchHex(r, 16), benchPodName(r),
		r.Intn(10_000_000), r.Intn(5000), r.Intn(1000), r.Intn(1_000_000), r.Intn(50), r.Intn(256), r.Intn(256), r.Intn(256))
	b.WriteString(`,"attrs":{`)
	for i := 0; b.Len() < size-2; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		switch i % 4 {
		case 0:
			fmt.Fprintf(&b, `"sku_%d":"%s"`, i, benchHex(r, 12))
		case 1:
			fmt.Fprintf(&b, `"qty_%d":%d`, i, r.Intn(1000))
		case 2:
			fmt.Fprintf(&b, `"ref_%d":"%s"`, i, benchUUID(r))
		default:
			fmt.Fprintf(&b, `"note_%d":"item reserved in warehouse %d zone %c"`, i, r.Intn(90), 'a'+rune(r.Intn(6)))
		}
	}
	b.WriteString("}}")
	return b.String()
}

// benchLogfmtLine is a logfmt line (Go zap/slog/logrus style) padded with
// key=value pairs to size.
func benchLogfmtLine(r *rand.Rand, size int) string {
	var b strings.Builder
	b.Grow(size + 256)
	fmt.Fprintf(&b, `ts=%s level=%s caller=handler/orders.go:%d msg="%s" request_id=%s trace_id=%s pod=%s user=%d duration=%dms status=%d`,
		benchTimestamp(r), benchLevels[r.Intn(len(benchLevels))], 100+r.Intn(400), benchMessages[r.Intn(len(benchMessages))],
		benchUUID(r), benchHex(r, 32), benchPodName(r), r.Intn(10_000_000), r.Intn(5000), []int{200, 201, 404, 500, 503}[r.Intn(5)])
	for i := 0; b.Len() < size; i++ {
		switch i % 4 {
		case 0:
			fmt.Fprintf(&b, " item%d=%s", i, benchHex(r, 12))
		case 1:
			fmt.Fprintf(&b, " count%d=%d", i, r.Intn(100000))
		case 2:
			fmt.Fprintf(&b, " ref%d=%s", i, benchUUID(r))
		default:
			fmt.Fprintf(&b, ` region%d="eu-west-%d zone %c"`, i, r.Intn(3), 'a'+rune(r.Intn(3)))
		}
	}
	return b.String()
}

// benchStackLine is an exception with a stack trace delivered as one log
// line (a multi-line record joined by the shipper), padded with frames.
func benchStackLine(r *rand.Rand, size int) string {
	var b strings.Builder
	b.Grow(size + 256)
	fmt.Fprintf(&b, "%s ERROR [%s] c.e.orders.OrderService - failed to reserve order %d for request %s\n",
		benchTimestamp(r), benchPodName(r), r.Intn(10_000_000), benchUUID(r))
	fmt.Fprintf(&b, "java.lang.IllegalStateException: inventory row %d locked by transaction %s\n", r.Intn(1_000_000), benchHex(r, 16))
	packages := []string{"com.example.orders", "com.example.inventory", "org.springframework.transaction", "org.hibernate.engine", "io.netty.channel"}
	classes := []string{"OrderService", "InventoryRepository", "TransactionInterceptor", "SessionImpl", "DefaultChannelPipeline"}
	for i := 0; b.Len() < size; i++ {
		if i > 0 && i%25 == 0 {
			fmt.Fprintf(&b, "Caused by: java.sql.SQLException: lock wait timeout exceeded; try restarting transaction (session %s)\n", benchHex(r, 8))
		}
		k := r.Intn(len(packages))
		fmt.Fprintf(&b, "\tat %s.%s.method%d(%s.java:%d)\n", packages[k], classes[k], r.Intn(8), classes[k], 20+r.Intn(900))
	}
	return b.String()
}

// benchLines builds a deterministic pool of lines for one shape and size.
func benchLines(shape string, size int) []string {
	r := rand.New(rand.NewSource(int64(size)*31 + int64(len(shape))))
	build := miningShapes[shape]
	lines := make([]string, miningLinePool)
	for i := range lines {
		lines[i] = build(r, size)
	}
	return lines
}

// benchMixedLines interleaves every shape and size, the closest single
// number to a real cluster's mix.
func benchMixedLines() []string {
	var lines []string
	for _, shape := range []string{"json", "logfmt", "stack"} {
		for _, size := range miningSizes {
			lines = append(lines, benchLines(shape, size)[:miningLinePool/4]...)
		}
	}
	r := rand.New(rand.NewSource(7))
	r.Shuffle(len(lines), func(i, j int) { lines[i], lines[j] = lines[j], lines[i] })
	return lines
}

// benchRedactor is the redactor the shipped config builds: the default rules
// plus the two extra patterns in config.yaml, IP redaction off.
func benchRedactor(b *testing.B) *Redactor {
	b.Helper()
	r, errs := NewRedactor(false, []string{`(?i)password=\S+`, `Authorization:\s*Bearer\s+\S+`})
	if len(errs) != 0 {
		b.Fatalf("redactor: %v", errs)
	}
	return r
}

type miningStage int

const (
	stageRedact miningStage = iota
	stageTokenize
	stageMine
	stageRedactMine
	stageRedactMatchMine
)

func runMiningBench(b *testing.B, lines []string, stage miningStage) {
	redactor := benchRedactor(b)
	matcher, errs := NewRegexMatcher(config.AgentRegexConfig{DefaultPattern: "(?i).*error.*"})
	if len(errs) != 0 {
		b.Fatalf("matcher: %v", errs)
	}
	miner := NewMiner(0.4, 4, 100)
	// Warm the miner so the timed loop measures the steady state. The
	// single-iteration probe the testing package runs first is discarded, so
	// it skips the warm-up to keep the whole run inside the time box.
	if b.N > 1 {
		for _, line := range lines {
			miner.Cluster(redactor.Scrub(line))
		}
	}
	var bytes int64
	for _, line := range lines {
		bytes += int64(len(line))
	}
	b.SetBytes(bytes / int64(len(lines)))
	b.ReportAllocs()
	b.ResetTimer()
	sink := 0
	for i := 0; i < b.N; i++ {
		line := lines[i%len(lines)]
		switch stage {
		case stageRedact:
			sink += len(redactor.Scrub(line))
		case stageTokenize:
			sink += len(tokenize(line))
		case stageMine:
			id, _, _ := miner.Cluster(line)
			sink += len(id)
		case stageRedactMine:
			id, _, _ := miner.Cluster(redactor.Scrub(line))
			sink += len(id)
		case stageRedactMatchMine:
			scrubbed := redactor.Scrub(line)
			sink++
			if matcher.Match(scrubbed).Matched() {
				id, _, _ := miner.Cluster(scrubbed)
				sink += len(id)
			}
		}
	}
	b.StopTimer()
	if sink == 0 {
		b.Fatal("benchmark produced no output")
	}
	b.ReportMetric(float64(len(miner.Snapshot())), "templates")
}

// BenchmarkLogLineLearningCost reports the per-line cost of redact + tokenize
// + mine, per shape and size, and for the realistic mix. Run with
// -benchmem; ns/op is ns per line.
func BenchmarkLogLineLearningCost(b *testing.B) {
	stages := []struct {
		name  string
		stage miningStage
	}{
		{"redact", stageRedact},
		{"tokenize", stageTokenize},
		{"mine", stageMine},
		{"redact+mine", stageRedactMine},
		{"redact+match+mine", stageRedactMatchMine},
	}
	for _, shape := range []string{"json", "logfmt", "stack"} {
		for _, size := range miningSizes {
			lines := benchLines(shape, size)
			for _, st := range stages {
				b.Run(fmt.Sprintf("%s/%dKiB/%s", shape, size>>10, st.name), func(b *testing.B) {
					runMiningBench(b, lines, st.stage)
				})
			}
		}
	}
	mixed := benchMixedLines()
	for _, st := range stages {
		b.Run("mixed/"+st.name, func(b *testing.B) {
			runMiningBench(b, mixed, st.stage)
		})
	}
}

// BenchmarkLogLineRedactionRule splits the cost of running each rule's
// expression on every line of the realistic mix, so the expensive rules are
// visible. It times the reference redactor, which runs every expression on
// every line.
func BenchmarkLogLineRedactionRule(b *testing.B) {
	mixed := benchMixedLines()
	full := newReferenceAgentRedactor(b, false, []string{`(?i)password=\S+`, `Authorization:\s*Bearer\s+\S+`})
	for _, rule := range full.rules {
		single := &referenceAgentRedactor{rules: []referenceAgentRule{rule}}
		b.Run(rule.name, func(b *testing.B) {
			b.ReportAllocs()
			sink := 0
			for i := 0; i < b.N; i++ {
				sink += len(single.Scrub(mixed[i%len(mixed)]))
			}
			if sink == 0 {
				b.Fatal("benchmark produced no output")
			}
		})
	}
}

// TestLogLineLearningBenchShapesAreRealistic keeps the benchmark honest: each
// generated shape reaches its size, carries high-cardinality IDs the redactor
// or the miner masks, and settles into a small number of templates rather
// than one template per line.
func TestLogLineLearningBenchShapesAreRealistic(t *testing.T) {
	redactor, errs := NewRedactor(false, nil)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	for shape := range miningShapes {
		for _, size := range []int{2 << 10, 16 << 10} {
			lines := benchLines(shape, size)[:256]
			miner := NewMiner(0.4, 4, 100)
			for _, line := range lines {
				if len(line) < size {
					t.Fatalf("%s line is %d bytes, want at least %d", shape, len(line), size)
				}
				miner.Cluster(redactor.Scrub(line))
			}
			if n := len(miner.Snapshot()); n == 0 || n > 64 {
				t.Fatalf("%s/%d settled into %d templates for 256 lines; the shape is not template-like", shape, size, n)
			}
		}
	}
}
