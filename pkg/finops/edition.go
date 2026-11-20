package finops

// DefaultCostRetentionMonths is how many months of cost lines the
// open-source build keeps.
const DefaultCostRetentionMonths = 13

// EditionOptions are the FinOps options a licensed build may turn on. The
// open-source build serves none of them: DefaultEditionOptions leaves every
// one off and the retention at 13 months, and the page shows them locked.
// A licensed build passes its own through the AWS mount; the features that
// read each field land with that build.
type EditionOptions struct {
	// ScheduledPDF sends the FinOps PDF by email on a schedule.
	ScheduledPDF bool
	// BrandedPDF renders the PDF on the server with the deployment's brand.
	BrandedPDF bool
	// Chargeback splits shared cost by rules.
	Chargeback bool
	// FinanceRole lets a finance-only role see FinOps without the other AWS
	// pages, and limits cost to the accounts it may see.
	FinanceRole bool
	// RetentionMonths is how many months of cost lines are kept; zero or
	// less is the open-source 13.
	RetentionMonths int
}

// DefaultEditionOptions are the open-source options: every licensed option
// off, 13 months of cost.
func DefaultEditionOptions() EditionOptions {
	return EditionOptions{RetentionMonths: DefaultCostRetentionMonths}
}

// Effective fills an unset retention with the open-source one.
func (options EditionOptions) Effective() EditionOptions {
	if options.RetentionMonths <= 0 {
		options.RetentionMonths = DefaultCostRetentionMonths
	}
	return options
}
