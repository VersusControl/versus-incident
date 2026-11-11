package finops_test

import (
	"testing"

	"github.com/VersusControl/versus-incident/pkg/aws/cost"
	"github.com/VersusControl/versus-incident/pkg/finops"
)

// TestCostTotalsConvertToFinopsTotals pins the mirrored shapes: a provider
// adapter's total converts to the core's with a conversion and no field
// mapping, which is what a source adapter needs to satisfy TotalsSource.
func TestCostTotalsConvertToFinopsTotals(t *testing.T) {
	day := finops.MustDate("2026-10-05")
	src := cost.Total{
		Day: day, Month: "2026-10", Account: "111111111111", Region: "us-east-1",
		Service: "network.nat", ServiceName: "Amazon Virtual Private Cloud", UsageType: "NatGateway-Bytes",
		RecordType: finops.RecordUsage, Team: "checkout", ResourceID: "nat-1", UsageGroup: "network traffic",
		Unit: "GB", MinDay: day, NetAmortizedMicros: 1_500_000, QuantityMicros: 42, Lines: 3,
		EstimatedTeam: true, EstimatedRegion: true,
	}
	got := finops.Total(src)
	if back := cost.Total(got); back != src {
		t.Fatalf("conversion changed a field: %+v", back)
	}
	if got.Day != day || got.Account != src.Account || got.NetAmortizedMicros != src.NetAmortizedMicros ||
		got.QuantityMicros != src.QuantityMicros || got.Lines != src.Lines || !got.EstimatedTeam || !got.EstimatedRegion {
		t.Fatalf("conversion lost a field: %+v", got)
	}
}
