package awsfix

const (
	dataAccount       = "222222222222"
	sandboxAccount    = "333333333333"
	managementAccount = "999999999999"
	ec2Compute        = "Amazon Elastic Compute Cloud - Compute"
	s3Service         = "Amazon Simple Storage Service"
	rdsService        = "Amazon Relational Database Service"
	savingsPlanFee    = "Savings Plans for AWS Compute usage"
	businessSupport   = "AWS Support (Business)"
)

// CostNewUntaggedDB is one standalone account with a full September: steady
// compute (checkout) and object storage (platform); on Oct 20 a new database
// instance with no team tag starts billing $40 a day.
func CostNewUntaggedDB() *CostScenario {
	return &CostScenario{
		Name:        CostNewUntaggedDBName,
		Currency:    "USD",
		Range:       DateRange{Start: "2026-09-01", End: "2026-11-09"},
		PartialFrom: "2026-11-01",
		Names:       map[string]string{prodAccount: "Production"},
		Series: []CostSeries{
			series(prodAccount, ec2Compute, "USE1-BoxUsage:m6i.large", "Usage", "checkout", "Hrs", step("2026-09-01", "300", "48")),
			series(prodAccount, s3Service, "TimedStorage-ByteHrs", "Usage", "platform", "GB-Mo", step("2026-09-01", "50", "2000")),
			series(prodAccount, rdsService, "USE1-InstanceUsage:db.r6g.xlarge", "Usage", "", "Hrs", step("2026-10-20", "40", "24")),
		},
		Forecast:        Forecast{Mean: "11400", Low: "11000", High: "11800"},
		Coverage:        Coverage{Covered: "0", Total: "300"},
		TagActive:       true,
		Budgets:         []Budget{},
		Recommendations: []HubRecommendation{},
	}
}

// SingleAccount is one standalone account read as its own payer: a NAT step
// on Oct 25, compute split across two teams and a Savings Plan, an object
// storage price change on Nov 1, a global CDN line, a credit on Oct 15, daily
// tax and an unused Savings Plan fee. Like AWS, every day of the open month
// (November) is marked Estimated. Its budgets cover every scope the import
// accepts and every reason it skips one.
func SingleAccount() *CostScenario {
	nat := series(prodAccount, "EC2 - Other", "USE1-NatGateway-Bytes", "Usage", "checkout", "GB",
		step("2026-10-01", "20.1234567", "1000"), step("2026-10-25", "85.5", "4250"))
	nat.ResourceID = "nat-1a"
	compute := series(prodAccount, ec2Compute, "USE1-BoxUsage:m6i.large", "Usage", "checkout", "Hrs",
		step("2026-10-01", "300.0000005", "48"))
	compute.ResourceID = "i-0abc"
	monthly := func(name, amount string) Budget {
		return Budget{BudgetName: name, BudgetType: "COST", TimeUnit: "MONTHLY", BudgetLimit: Spend{Amount: amount, Unit: "USD"},
			Metrics: []string{"NetAmortizedCost"}}
	}
	checkout := monthly("Checkout", "5000")
	checkout.CostFilters = map[string][]string{"TagKeyValue": {"user:team$checkout"}}
	production := monthly("Production account", "30000")
	production.FilterExpression = &Expression{Dimensions: &KeyValues{Key: "LINKED_ACCOUNT", Values: []string{prodAccount}}}
	platform := monthly("Platform", "3000")
	platform.FilterExpression = &Expression{Tags: &KeyValues{Key: "user:team", Values: []string{"platform"}}}
	unblended := monthly("Unblended total", "40000")
	unblended.Metrics = []string{"UnblendedCost"}
	legacy := monthly("Legacy amortized", "40000")
	legacy.Metrics, legacy.CostTypes = nil, &CostTypes{UseAmortized: true}
	twoAccounts := monthly("Two accounts", "9000")
	twoAccounts.FilterExpression = &Expression{Or: []Expression{
		{Dimensions: &KeyValues{Key: "LINKED_ACCOUNT", Values: []string{prodAccount}}},
		{Dimensions: &KeyValues{Key: "LINKED_ACCOUNT", Values: []string{dataAccount}}},
	}}
	transfer := Budget{BudgetName: "Transfer", BudgetType: "USAGE", TimeUnit: "MONTHLY", BudgetLimit: Spend{Amount: "5", Unit: "GB"}}
	return &CostScenario{
		Name:        SingleAccountName,
		Currency:    "USD",
		Range:       DateRange{Start: "2026-10-01", End: "2026-11-09"},
		PartialFrom: "2026-11-01",
		Names:       map[string]string{prodAccount: "Production"},
		Series: []CostSeries{
			nat,
			compute,
			series(prodAccount, ec2Compute, "EUW1-BoxUsage:t3.medium", "Usage", "platform", "Hrs", step("2026-10-01", "40.25", "24")),
			series(prodAccount, ec2Compute, "USE1-BoxUsage:c6i.xlarge", "SavingsPlanCoveredUsage", "", "Hrs", step("2026-10-01", "100", "24")),
			series(prodAccount, s3Service, "TimedStorage-ByteHrs", "Usage", "platform", "GB-Mo",
				step("2026-10-01", "50", "2000"), step("2026-11-01", "55", "2000")),
			series(prodAccount, "Amazon CloudFront", "US-DataTransfer-Out-Bytes", "Usage", "", "GB", step("2026-10-01", "3.3", "40")),
			series(prodAccount, ec2Compute, "", "Credit", "", "", fee("2026-10-15", "-50")...),
			series(prodAccount, "Tax", "", "Tax", "", "", step("2026-10-01", "1.5", "")),
			series(prodAccount, savingsPlanFee, "", "SavingsPlanRecurringFee", "", "", step("2026-10-01", "4.8", "")),
		},
		Forecast:        Forecast{Mean: "9000.5", Low: "8800", High: "9200"},
		Coverage:        Coverage{Covered: "710", Total: "1000"},
		TagActive:       true,
		Budgets:         []Budget{monthly("AWS total", "42000"), checkout, production, platform, unblended, legacy, twoAccounts, transfer},
		ExpectedBudgets: map[string]string{"AWS total": "deployment", "Checkout": "team:checkout", "Production account": "account:" + prodAccount, "Platform": "team:platform"},
		ExpectedSkipped: map[string]string{"Unblended total": "not_net_amortized", "Legacy amortized": "not_net_amortized",
			"Two accounts": "filter_not_supported", "Transfer": "not_monthly_cost"},
		Recommendations: []HubRecommendation{
			{AccountID: prodAccount, ActionType: "Rightsize", ResourceID: "i-0abc", Region: usEast1, CurrentResourceSummary: "m6i.large",
				RecommendedResourceSummary: "m6i.medium", EstimatedMonthlySavings: 61.32, Tags: []HubTag{{Key: "team", Value: "checkout"}}},
			{AccountID: prodAccount, ActionType: "Delete", ResourceID: "vol-1", Region: usEast1, EstimatedMonthlySavings: 8},
		},
	}
}

// Organization is the management account of an Organization: two connected
// accounts and one cost-only account, compute in two regions, Production
// split between the checkout and platform teams, a new database on Oct 20, a
// NAT step on Nov 3, an unused Savings Plan and a support fee billed to the
// payer each month. Like AWS, every day of the open month (November) is
// marked Estimated.
func Organization() *CostScenario {
	support := series(managementAccount, businessSupport, "", "Fee", "", "", fee("2026-10-01", "1200")...)
	support.Steps = append(support.Steps, fee("2026-11-01", "1200")...)
	return &CostScenario{
		Name:        OrganizationName,
		Currency:    "USD",
		Range:       DateRange{Start: "2026-10-01", End: "2026-11-09"},
		PartialFrom: "2026-11-01",
		Names: map[string]string{prodAccount: "Production", dataAccount: "Data", sandboxAccount: "Sandbox",
			managementAccount: "Management"},
		Series: []CostSeries{
			series(prodAccount, ec2Compute, "USE1-BoxUsage:m6i.large", "Usage", "checkout", "Hrs", step("2026-10-01", "300", "48")),
			series(prodAccount, ec2Compute, "EUW1-BoxUsage:m6i.large", "Usage", "checkout", "Hrs", step("2026-10-01", "100.333333", "16")),
			series(prodAccount, "EC2 - Other", "USE1-NatGateway-Bytes", "Usage", "checkout", "GB",
				step("2026-10-01", "20", "1000"), step("2026-11-03", "85", "4250")),
			series(prodAccount, s3Service, "TimedStorage-ByteHrs", "Usage", "platform", "GB-Mo", step("2026-10-01", "30", "1200")),
			series(dataAccount, ec2Compute, "USE1-BoxUsage:r6g.large", "Usage", "data", "Hrs", step("2026-10-01", "50", "24")),
			series(dataAccount, rdsService, "USE1-InstanceUsage:db.r6g.xlarge", "Usage", "", "Hrs", step("2026-10-20", "40", "24")),
			series(sandboxAccount, s3Service, "TimedStorage-ByteHrs", "Usage", "", "GB-Mo", step("2026-10-01", "10", "400")),
			series(managementAccount, savingsPlanFee, "", "SavingsPlanRecurringFee", "", "", step("2026-10-01", "12", "")),
			support,
		},
		Forecast:        Forecast{Mean: "31000", Low: "30000", High: "32000"},
		Coverage:        Coverage{Covered: "500", Total: "1000"},
		TagActive:       true,
		Budgets:         []Budget{},
		Recommendations: []HubRecommendation{},
	}
}

// LargeOrganizationAccounts is how many linked accounts LargeOrganization
// bills, with consecutive IDs from LargeOrganizationFirstAccount.
const (
	LargeOrganizationAccounts     = 60
	LargeOrganizationFirstAccount = 100000000001
)

// LargeOrganization is the management account of a large Organization: 60
// linked accounts with compute each, a support fee on the payer, and 50
// groups per Cost Explorer page, so a first setup cannot fit 20 paid requests
// without learning its page counts.
func LargeOrganization() *CostScenario {
	s := &CostScenario{
		Name:        LargeOrganizationName,
		Currency:    "USD",
		Range:       DateRange{Start: "2026-10-01", End: "2026-11-11"},
		PartialFrom: "2026-11-01",
		PageGroups:  50,
		Names:       map[string]string{managementAccount: "Management"},
		Series: []CostSeries{
			series(managementAccount, businessSupport, "", "Fee", "", "", fee("2026-11-01", "1200")...),
		},
		Forecast:        Forecast{Mean: "20000", Low: "19000", High: "21000"},
		Coverage:        Coverage{Covered: "0", Total: "1000"},
		TagActive:       true,
		Budgets:         []Budget{},
		Recommendations: []HubRecommendation{},
	}
	for _, account := range linkedAccounts(LargeOrganizationFirstAccount, LargeOrganizationAccounts) {
		s.Series = append(s.Series, series(account, ec2Compute, "USE1-BoxUsage:m6i.large", "Usage", "checkout", "Hrs",
			step("2026-10-01", "10.5", "24")))
	}
	return s
}
