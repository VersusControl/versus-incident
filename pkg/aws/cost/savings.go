package cost

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/computeoptimizer"
	cotypes "github.com/aws/aws-sdk-go-v2/service/computeoptimizer/types"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/awsauth"
	"github.com/VersusControl/versus-incident/pkg/finops"
)

// Savings guides are read-only how-tos the user runs themselves; Versus
// never applies a saving.

func hubGuide() *finops.Guide {
	return &finops.Guide{ConsolePath: "Billing and Cost Management → Cost Optimization Hub → Recommendations",
		Steps: []string{"Open the recommendation and review the projected utilisation.", "Test the new size in a lower environment before changing production."}}
}

var idleGuides = map[string]*finops.Guide{
	finops.IdleUnattachedVolume: {ConsolePath: "EC2 → Elastic Block Store → Volumes (filter: State = available)",
		Steps: []string{"Snapshot the volume if its data may be needed.", "Delete the volume."}},
	finops.IdleEmptyLB: {ConsolePath: "EC2 → Load Balancing → Load Balancers",
		Steps: []string{"Confirm no listener forwards to registered targets.", "Delete the load balancer, or remove it from the infrastructure code that created it."}},
	finops.IdleUnusedIP: {ConsolePath: "VPC → Elastic IP addresses (filter: not associated)",
		Steps: []string{"Release addresses that are not associated with a running resource."}},
	finops.IdleIdleInstance: {ConsolePath: "EC2 → Instances",
		Steps: []string{"Confirm the machine is not needed.", "Stop it, or terminate it after taking a snapshot."}},
}

func endpointGuide(vpc string, services []string) *finops.Guide {
	var b strings.Builder
	for _, s := range services {
		kind := "Gateway"
		if strings.Contains(s, ".ecr.") {
			kind = "Interface"
		}
		name := s[strings.LastIndex(s, ".")+1:]
		fmt.Fprintf(&b, "resource \"aws_vpc_endpoint\" \"%s\" {\n  vpc_id            = \"%s\"\n  service_name      = \"%s\"\n  vpc_endpoint_type = \"%s\"\n}\n", name, vpc, s, kind)
	}
	return &finops.Guide{ConsolePath: "VPC → Endpoints → Create endpoint",
		Steps:   []string{"Create the endpoints in " + vpc + " and associate the gateway endpoints with its private route tables.", "Watch NAT data processing fall over the next days."},
		Snippet: b.String()}
}

func commitmentGuide() *finops.Guide {
	return &finops.Guide{ConsolePath: "Billing and Cost Management → Savings Plans → Recommendations",
		Steps: []string{"Review the recommended hourly commitment and term.", "Purchase only what steady usage covers."}}
}

// List prices used only where no bill line or provider estimate exists.
const (
	loadBalancerMonthlyMicros  = 16_425_000 // $0.0225 per hour × 730 hours
	interfaceEndpointMonthly   = 7_300_000  // $0.01 per hour per zone × 730 hours
	interfaceEndpointZones     = 2
	minNetworkPathMicros       = 50 * finops.MicrosPerUnit
	maxComputeOptimizerRegions = 5
	idleAddressUsage           = "IdleAddress"
	natProcessingUsage         = "NatGateway-Bytes"
)

// computeOptimizer builds a pinned regional Compute Optimizer client.
func (e env) computeOptimizer(region string) (*computeoptimizer.Client, error) {
	if awsauth.ValidateRegion(region) != nil {
		return nil, awsauth.ErrNetwork
	}
	host := "compute-optimizer." + region + ".amazonaws.com"
	endpoint := "https://" + host
	cfg := e.config()
	cfg.Region = region
	return computeoptimizer.NewFromConfig(cfg, func(o *computeoptimizer.Options) {
		o.BaseEndpoint = &endpoint
		o.HTTPClient = pinnedHTTPClient{client: e.httpClient, host: host}
	}), nil
}

// resourcePart is the resource's own ID (model.ResourceID): an ARN is cut
// to its last resource segment so the account ID never reaches a title.
func resourcePart(id string) string { return model.ResourceID(id) }

// computeOptimizerRecommendations is the fallback when Cost Optimization Hub
// is not enrolled: over-provisioned EC2 instances in the given regions
// (free).
func (f fetcher) computeOptimizerRecommendations(ctx context.Context, regions []string, tagKey string, keepTeams, keepNames bool) ([]finops.Recommendation, error) {
	var out []finops.Recommendation
	for _, region := range regions {
		client, err := f.env.computeOptimizer(region)
		if err != nil {
			return nil, err
		}
		var token *string
		done := false
		for page := 0; page < maxFreePages; page++ {
			var resp *computeoptimizer.GetEC2InstanceRecommendationsOutput
			err := f.send(ctx, func(ctx context.Context) error {
				var err error
				resp, err = client.GetEC2InstanceRecommendations(ctx, &computeoptimizer.GetEC2InstanceRecommendationsInput{MaxResults: aws.Int32(100), NextToken: token})
				return err
			})
			if err != nil {
				return nil, err
			}
			for _, r := range resp.InstanceRecommendations {
				if r.Finding != cotypes.FindingOverProvisioned {
					continue
				}
				var best *cotypes.InstanceRecommendationOption
				for i := range r.RecommendationOptions {
					o := &r.RecommendationOptions[i]
					if best == nil || o.Rank < best.Rank {
						best = o
					}
				}
				if best == nil || best.SavingsOpportunity == nil || best.SavingsOpportunity.EstimatedMonthlySavings == nil {
					continue
				}
				monthly, err := finops.ParseMicros(strconv.FormatFloat(best.SavingsOpportunity.EstimatedMonthlySavings.Value, 'f', -1, 64))
				if err != nil || monthly <= 0 {
					continue
				}
				name := ""
				if keepNames {
					// InstanceName is the Name tag value.
					name = aws.ToString(r.InstanceName)
				}
				rec := finops.Recommendation{ResourceID: resourcePart(aws.ToString(r.InstanceArn)), Name: name,
					Account: aws.ToString(r.AccountId), Region: region, Current: aws.ToString(r.CurrentInstanceType),
					Target: aws.ToString(best.InstanceType), Monthly: monthly, Source: "compute_optimizer",
					Guide: &finops.Guide{ConsolePath: "Compute Optimizer → EC2 instances", Steps: []string{"Review the recommended type and its performance risk.", "Change the type in the launch template or infrastructure code."}}}
				if keepTeams {
					for _, t := range r.Tags {
						if aws.ToString(t.Key) == tagKey {
							rec.Team = aws.ToString(t.Value)
						}
					}
				}
				out = append(out, rec)
			}
			token = resp.NextToken
			if token == nil || *token == "" {
				done = true
				break
			}
		}
		if !done {
			return nil, errTruncated
		}
	}
	return out, nil
}

// computeRegions are the regions with machine cost in the cached lines, up
// to a bound, for the Compute Optimizer fallback.
func computeRegions(lines []finops.CostLine) []string {
	cost := map[string]int64{}
	for _, l := range lines {
		if l.Service == "compute.vm" && l.Region != finops.GlobalRegion && l.Region != finops.UnknownRegion {
			cost[l.Region] += l.NetAmortizedMicros
		}
	}
	regions := make([]string, 0, len(cost))
	for r := range cost {
		regions = append(regions, r)
	}
	sort.Slice(regions, func(i, j int) bool {
		if cost[regions[i]] != cost[regions[j]] {
			return cost[regions[i]] > cost[regions[j]]
		}
		return regions[i] < regions[j]
	})
	if len(regions) > maxComputeOptimizerRegions {
		regions = regions[:maxComputeOptimizerRegions]
	}
	return regions
}

// inventoryTeam reads the team tag of a resource when its value is shown.
func inventoryTeam(r model.Resource, tagKey string, keep bool) string {
	if !keep {
		return ""
	}
	if v := r.Tags[tagKey]; v != model.RedactedValue {
		return v
	}
	return ""
}

// inventorySavings derives savings facts from the collected inventory and
// the cached cost of the last 30 days: load balancers with no registered
// target, unused public IPv4 addresses (billed as idle), and VPCs whose NAT
// processing could go through private endpoints. It reads the store only.
// usageKey is the account and region a billed idle or NAT fact belongs to.
type usageKey struct{ account, region string }

// billedUsage is what the savings rules read from cost data: NAT processing
// and unused-address charges per account and region, and how many days the
// window held (the divisor of the unused-address count).
type billedUsage struct {
	natCost   map[usageKey]int64
	idleIP    map[usageKey]int64
	idleHours map[usageKey]int64
	days      int
}

// billedFromLines sums the billed facts from the window's lines.
func billedFromLines(lines []finops.CostLine) billedUsage {
	b := billedUsage{natCost: map[usageKey]int64{}, idleIP: map[usageKey]int64{}, idleHours: map[usageKey]int64{}}
	days := map[finops.Date]bool{}
	for _, l := range lines {
		days[l.Day] = true
		k := usageKey{l.Account, l.Region}
		switch {
		case strings.Contains(l.UsageType, natProcessingUsage):
			b.natCost[k] += l.NetAmortizedMicros
		case strings.Contains(l.UsageType, idleAddressUsage):
			b.idleIP[k] += l.NetAmortizedMicros
			b.idleHours[k] += l.QuantityMicros
		}
	}
	b.days = len(days)
	return b
}

func (s *Source) inventorySavings(ctx context.Context, b billedUsage) (idle []finops.IdleResource, paths []finops.NetworkPath, gaps []finops.SavingsGap, err error) {
	o := s.options()
	natCost, idleIP, idleHours := b.natCost, b.idleIP, b.idleHours
	for k, amt := range idleIP {
		if amt <= 0 {
			continue
		}
		count := 1
		if n := b.days; n > 0 {
			count = max(1, int((idleHours[k]/int64(n)+12_000_000)/24_000_000))
		}
		idle = append(idle, finops.IdleResource{Kind: finops.IdleUnusedIP, ID: "idle-ipv4-" + k.region, Account: k.account,
			Region: k.region, Monthly: amt, Count: count, Basis: finops.BasisBilled})
	}
	for _, p := range s.payers() {
		if p.Organization {
			// Organization lines carry no usage type, so idle addresses and
			// NAT processing cannot be read from them without paid detail.
			gaps = append(gaps, finops.SavingsGap{Check: finops.IdleUnusedIP, Reason: finops.GapNeedsUsageDetail},
				finops.SavingsGap{Check: finops.SavingNetworkPath, Reason: finops.GapNeedsUsageDetail})
			break
		}
	}
	gaps = append(gaps,
		finops.SavingsGap{Check: finops.IdleOldSnapshot, Reason: finops.GapNotCollected},
		finops.SavingsGap{Check: finops.IdleStoppedVolumes, Reason: finops.GapNotCollected},
		finops.SavingsGap{Check: finops.SavingStorageClass, Reason: finops.GapNotCollected})
	if o.Inventory == nil {
		gaps = append(gaps, finops.SavingsGap{Check: finops.IdleEmptyLB, Reason: finops.GapNotCollected},
			finops.SavingsGap{Check: finops.SavingNetworkPath, Reason: finops.GapNotCollected})
		return idle, nil, gaps, nil
	}
	for _, account := range o.Connected {
		res, err := o.Inventory.Resources(ctx, snapshot.Query{Account: account})
		if err != nil {
			return nil, nil, nil, err
		}
		byARN := map[string]model.Resource{}
		for _, r := range res.Resources {
			byARN[r.ARN] = r
		}
		natVPCs := map[usageKey]map[string]int{}
		endpoints := map[string]map[string]bool{}
		for _, r := range res.Resources {
			switch r.Type {
			case "load-balancer":
				targets := 0
				for _, tg := range relationTargets(r, model.RelationRoutesTo) {
					targets += len(relationTargets(byARN[tg], model.RelationRoutesTo))
				}
				if targets == 0 {
					idle = append(idle, finops.IdleResource{Kind: finops.IdleEmptyLB, ID: r.Name, Name: r.Name, Account: r.Account,
						Region: r.Region, Team: inventoryTeam(r, o.TeamTagKey, o.TagAllowlisted), Monthly: loadBalancerMonthlyMicros, Basis: finops.BasisListPrice})
				}
			case "nat-gateway":
				if v := vpcOf(r); v != "" && r.State != "deleted" {
					k := usageKey{r.Account, r.Region}
					if natVPCs[k] == nil {
						natVPCs[k] = map[string]int{}
					}
					natVPCs[k][v]++
				}
			case "vpc-endpoint":
				if v := vpcOf(r); v != "" {
					if endpoints[v] == nil {
						endpoints[v] = map[string]bool{}
					}
					endpoints[v][r.Name] = true
				}
			}
		}
		for k, vpcs := range natVPCs {
			total := 0
			for _, n := range vpcs {
				total += n
			}
			for _, vpc := range sortedKeys(vpcs) {
				processing := natCost[k] * int64(vpcs[vpc]) / int64(total)
				if processing < minNetworkPathMicros {
					continue
				}
				var missing, names []string
				var endpointCost int64
				for _, svc := range []struct{ suffix, name string }{{"s3", "S3"}, {"dynamodb", "DynamoDB"}, {"ecr.dkr", "ECR"}} {
					full := "com.amazonaws." + k.region + "." + svc.suffix
					if endpoints[vpc][full] {
						continue
					}
					missing = append(missing, full)
					names = append(names, svc.name)
					if svc.suffix == "ecr.dkr" {
						missing = append(missing, "com.amazonaws."+k.region+".ecr.api")
						endpointCost += 2 * interfaceEndpointZones * interfaceEndpointMonthly
					}
				}
				if len(names) == 0 {
					continue
				}
				const target = "managed_services"
				paths = append(paths, finops.NetworkPath{Network: vpc, Account: k.account, Region: k.region,
					Processing: map[string]finops.Micros{target: processing}, HasEndpoint: map[string]bool{},
					EndpointCost: map[string]finops.Micros{target: endpointCost}, TargetNames: map[string]string{target: joinNames(names)},
					UpTo: true, Endpoints: len(names), Guide: endpointGuide(vpc, missing)})
			}
		}
	}
	return idle, paths, gaps, nil
}

func joinNames(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}
